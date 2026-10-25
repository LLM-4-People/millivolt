package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// absorbResend books ONE absorbed attempt on the record (Attempts, Retries,
// the live publish, the storm observation) and re-sends the SAME request -
// the shared middle of serveNonStreaming's two pre-write absorb sites (the
// degenerate-200 classes and the retryable in-band error envelope), which
// differ only in the attempt's error fields and the budget they spend.
// Nothing has been written to the client on this surface, so the re-send is
// always wire-safe; its opening WaitSend honors operator holds ("a retry is
// a new send and waits"). ok=false means the re-send failure was already
// surfaced on this socket (the client left, the storm queue rejected the
// retry with a 429 + Retry-After, or the transport failed with a 502) and
// the request is finished; ok=true returns the adopted attempt's response
// and its per-send cancel, which the caller defers (function-scoped LIFO
// ordering), with the record adoption (the status, the >=400 error detail,
// the upstream headers) already run here - the single owner of the tail both
// quality loops used to duplicate. The adopted attempt's send start
// (FinalAttemptAt) is stamped at the re-send's admission inside doWithRetry,
// exactly like the first send's own absorbed retries.
// ResponseBytes is deliberately NOT reset by this adoption: on this surface
// the abandoned attempts wrote nothing (the spool precedes every write), so
// the accumulated total already equals the adopted attempt's body alone -
// "the bytes the client received".
func (s *Server) absorbResend(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target, key string, body []byte, rec *metrics.Record, groupKey string, hooks scheduler.WaiterHooks, at metrics.RetryAttempt, old *http.Response) (next *http.Response, nextCancel context.CancelFunc, ok bool) {
	rec.Attempts = append(rec.Attempts, at)
	rec.Retries++
	s.publishUpdate(rec)
	s.finishStormResponse(ctx, true)
	old.Body.Close()
	next, nextCancel, err := s.doWithRetry(ctx, groupKey, r, t, key, body, rec, hooks, true)
	if err != nil {
		clientGone, stormQueue := s.classifyResendFailure(r, rec, err)
		switch {
		case clientGone:
			return nil, nil, false
		case stormQueue:
			// Already classified: the sink owner stamps the record and writes
			// the 429 + Retry-After.
			s.writeStormQueueError(w, rec, t.inbound, err)
			return nil, nil, false
		}
		writeProtocolHTTPError(w, t.inbound, http.StatusBadGateway, typeAPIError, "upstream error: "+transportErrText(err))
		return nil, nil, false
	}
	rec.StatusCode = next.StatusCode
	if next.StatusCode >= 400 {
		s.captureErrorFromResponse(ctx, next, rec)
	}
	captureUpstreamHeaders(next, rec, t.authHeader)
	return next, nextCancel, true
}

// serveNonStreaming relays a non-streaming response with quality handling. A
// 200 whose body classifies degenerate (metrics.AnalyzeNonStreamBody's
// outcome - the same predicate as the streaming paths) triggers a transparent
// retry BEFORE
// the status line is written; past the budget the body is surfaced as an
// HTTP 502 with an OpenAI error envelope - a failure, never a silently empty
// success (the cursor empty_turn precedent, and 502/5xx is retryable in
// common API clients). Two budgets govern the loop: quality_retries owns the
// void classes (empty completion, empty tool_calls) and the retryable
// in-band error envelope (a transient server-availability class the OpenAI
// retry semantics cover, re-sent before any byte reaches the client; every
// other class relays verbatim as the provider's own final statement), and
// thinking_retries owns the reasoning-only class
// (metrics.CodeReasoningOnly) - the non-streaming twin of the streaming
// mid-thinking rescue, so both surfaces treat the same outcome identically.
// Translated (anthropic) non-streaming bodies are never
// quality-classified: Anthropic documents legitimate empty end_turn answers
// and refusals that must not be re-run (and re-running a server_tool_use turn
// would double-execute tools upstream).
func (s *Server) serveNonStreaming(ctx context.Context, w http.ResponseWriter, resp *http.Response, r *http.Request, t *target, key string, body []byte, rec *metrics.Record, groupKey string, hooks scheduler.WaiterHooks) {
	qualityLeft := s.cfg().QualityRetries
	thinkingLeft := s.cfg().ThinkingRetries
	for {
		// Final error outcome: relay verbatim (the error detail was already
		// captured by captureErrorFromResponse before the status line), with
		// no translation and no quality inspection. An event-stream error body
		// relays raw - the status itself already carries the failure, so the
		// terminal-hold substitution never injects a synthetic in-band error
		// into an error stream.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if t.translate {
				// The upstream error body is in the target protocol; the
				// client expects its own. Translate the envelope, keep the
				// upstream status, and fall back to a bounded generic error
				// when the body is not a recognizable envelope.
				full, rerr := s.readWholeDocument(ctx, resp.Body, maxErrBodyBytes)
				status := resp.StatusCode
				out, terr := providerformat.TranslateErrorBetween(t.inbound, t.outbound, resp.StatusCode, full)
				if !statusAllowsBody(resp.StatusCode) {
					// A body-forbidden status (1xx, 204, 304) cannot carry the
					// translated error: writing it makes writeCounted fail and
					// markClientGone rewrite the record as a 499 client cancel.
					// Fail closed with the gateway sink instead.
					status = http.StatusBadGateway
					rec.StatusCode = status
					msg := fmt.Sprintf("upstream HTTP %d cannot carry an error body", resp.StatusCode)
					if rec.ErrorType == "" {
						rec.ErrorType = "upstream_error"
						rec.ErrorMsg = msg
					}
					out, _ = providerformat.EncodeError(t.inbound, &ir.Error{
						Type: "api_error", Status: status, Message: msg,
					})
				} else if rerr != nil {
					// The provider's error document could not be read within
					// the whole-document bound. Fail closed with a real
					// gateway status instead of asserting the upstream status
					// over a partial or missing envelope.
					status = http.StatusBadGateway
					rec.StatusCode = status
					if rec.ErrorType == "" {
						rec.ErrorType = "upstream_error"
						rec.ErrorMsg = "upstream error body could not be read"
					}
					out, _ = providerformat.EncodeError(t.inbound, &ir.Error{
						Type: "api_error", Status: status,
						Message: "upstream error: " + transportErrText(rerr),
					})
				} else if terr != nil {
					out, _ = providerformat.EncodeError(t.inbound, &ir.Error{
						Type: "api_error", Message: fmt.Sprintf("upstream HTTP %d", resp.StatusCode),
					})
				}
				copyResponseHeaders(w.Header(), resp.Header)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if _, werr := writeCounted(w, rec, out); werr != nil {
					markClientGone(rec)
				}
				return
			}
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if isEventStream(resp.Header.Get("Content-Type")) {
				// Flush each read: an upstream error event that stays open
				// must reach the client immediately, not sit in the server's
				// response buffer until the body ends.
				flusher, _ := w.(http.Flusher)
				if cerr := copyFlushing(disconnectWriter{w, rec}, resp.Body, flusher); cerr != nil {
					markStreamErr(ctx, rec, cerr)
				}
				// The relaying copy above may abort early on a client
				// disconnect, leaving the real upstream body unread. It is
				// closed by the body-Close defer registered BEFORE
				// captureErrorFromResponse re-wrapped resp.Body (ServeHTTP's
				// for the first response, the quality re-run's own defer for
				// a replacement): defer evaluates resp.Body at registration,
				// so that defer still holds the REAL body - the early
				// binding, not this call, is what closes the stream. Here
				// resp.Body is the NopCloser re-wrap, so the Close below is
				// a no-op, kept only so a future Close-propagating wrap
				// cannot leak.
				resp.Body.Close()
			} else {
				s.nonStreamBody(ctx, w, resp.Body, rec)
			}
			return
		}

		// 204/205 carry no document and 202 is an accepted asynchronous
		// response, not a completion. Relay all three verbatim for every
		// protocol before the translation and quality branches so neither
		// path invents an empty completion or translates a non-completion
		// acknowledgement body.
		if isNonCompletionStatus(resp.StatusCode) {
			s.relayNonCompletionStatus(ctx, w, resp, rec, t.inbound)
			return
		}

		// Format translation is fully buffered by design; the status line is
		// written only after a successful translation, so a failed translation
		// carries a real error status instead of a committed 200.
		if t.translate {
			// Reserve the whole-document buffer process-wide before reading.
			// The reservation is held until this branch has written the
			// translated body, so concurrent translations cannot multiply the
			// per-document cap without bound.
			reserve := responseReadReserve(resp.ContentLength, maxTranslateBodyBytes+1)
			if !s.respBudget.acquire(reserve) {
				writeResponseBufferBudget(w, rec, t.inbound)
				return
			}
			defer s.respBudget.release(reserve)
			// The per-send deadline (X-Proxy-Timeout-Ms) lives on the
			// attempt context that built this upstream request, not on the
			// base request context; read under it so an explicit body
			// budget longer than the fallback still applies.
			readCtx := ctx
			if resp.Request != nil {
				readCtx = resp.Request.Context()
			}
			full, rerr := s.readWholeDocument(readCtx, resp.Body, maxTranslateBodyBytes+1)
			if rerr != nil {
				markStreamErr(ctx, rec, rerr)
				if rec.ClientDisconnected {
					return
				}
				rec.StatusCode = http.StatusBadGateway
				writeProtocolHTTPError(w, t.inbound, http.StatusBadGateway, typeAPIError,
					"upstream error: "+transportErrText(rerr))
				return
			}
			// Past the cap the document cannot be translated, and verbatim
			// relay would break the translated-shape contract: fail closed
			// with a real 502 (the status line is still uncommitted here).
			if int64(len(full)) > maxTranslateBodyBytes {
				rec.StatusCode = http.StatusBadGateway
				rec.ErrorType = "response_too_large"
				rec.ErrorMsg = "upstream translated body exceeds " + config.FormatByteSize(maxTranslateBodyBytes)
				writeProtocolHTTPError(w, t.inbound, http.StatusBadGateway, typeAPIError,
					"upstream response too large to translate")
				return
			}
			// The provider's in-band error envelope on a 200 is its own
			// statement of failure: record it (the translated output still
			// relays to the client).
			if rec.ErrorType == "" && metrics.HasErrorKey(full) {
				if typ, code, msg := metrics.ParseErrorEnvelope(full); typ != "" {
					rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = typ, code, msg
				}
			}
			// Source usage is authoritative even when the codec cannot read
			// the provider's custom keys. Parse it before encoding so the
			// client wire and the canonical response carry the real counts.
			sourceUsage, sourceHasUsage, usageErr := providerformat.SourceUsage(t.outbound, full, s.usageKeysFor(rec.Provider))
			var usageOverride *ir.Usage
			if usageErr == nil && sourceHasUsage {
				usageOverride = &sourceUsage
			}
			out, respIR, warnings, terr := providerformat.TranslateResponseBetweenIR(t.inbound, t.outbound, full, usageOverride)
			if terr != nil {
				rec.StatusCode = http.StatusBadGateway
				// Keep a provider error already parsed from the upstream document;
				// the translation failure must not overwrite its classification.
				if rec.ErrorType == "" {
					rec.ErrorType = "transform_error"
					rec.ErrorMsg = terr.Error()
				}
				if errors.Is(terr, metrics.ErrMetricRange) {
					invalidateUsage(rec, terr)
				}
				writeProtocolHTTPError(w, t.inbound, http.StatusBadGateway, typeAPIError,
					"translate upstream response: "+terr.Error())
				return
			}
			// Translation can still grow the document (wrapper fields and
			// provider-string rendering). The input cap above is not an output
			// cap: past the same buffering bound the response cannot be relayed
			// within the translated-shape contract, so fail closed before any
			// byte reaches the client.
			if int64(len(out)) > maxTranslateBodyBytes {
				rec.StatusCode = http.StatusBadGateway
				rec.ErrorType = "response_too_large"
				rec.ErrorMsg = "translated response exceeds " + config.FormatByteSize(maxTranslateBodyBytes)
				writeProtocolHTTPError(w, t.inbound, http.StatusBadGateway, typeAPIError,
					"translated response too large")
				return
			}
			// A nil canonical response means the 200 body was a provider error
			// envelope. A translated client would read a 200 as success and
			// ignore the translated error object, so the status line carries
			// the failure (the same 502 the non-streaming quality path uses
			// for a failure surfaced from a 200).
			clientStatus := resp.StatusCode
			if respIR == nil {
				clientStatus = http.StatusBadGateway
				rec.StatusCode = clientStatus
				if rec.ErrorType == "" {
					rec.ErrorType = "upstream_error"
					rec.ErrorMsg = "upstream returned an in-band error"
				}
			}
			// The source document owns usage and cost accounting; provider
			// usage_keys address its shape, not the normalized translated
			// wire. Read the translated output for completion structure only.
			// A canonical failure never reaches this point: a non-nil
			// respIR.Error makes TranslateResponseBetweenIR return a nil
			// canonical response, and the branch above already stamped the
			// failure and the 502 status from the source error envelope.
			analyzeNonStreamBytes(out, rec, nil, nil, s.cfg().CaptureBodyPreview)
			if t.inbound != ir.Chat {
				// The non-stream analyzer reads Chat-shaped choices. For an
				// Anthropic or Responses client those fields live only in the
				// canonical response, so stamp the structure the translated
				// wire no longer exposes in that shape.
				applyTranslatedStructure(rec, respIR, s.cfg().CaptureBodyPreview)
			} else if respIR != nil && respIR.ProviderToolCalls > 0 {
				// The Chat encoder has no shape for a provider-executed item,
				// so the translated wire carries no tool_calls and the
				// Chat-shaped analysis counts zero while the finish reason
				// already says tool_calls. Blocks are counted by that
				// analysis; add only the provider items.
				rec.ToolCalls += respIR.ProviderToolCalls
			}
			if usageErr != nil {
				invalidateUsage(rec, usageErr)
			} else if sourceHasUsage {
				rec.Usage = metrics.Usage{
					InputTokens:     sourceUsage.InputTokens,
					OutputTokens:    sourceUsage.OutputTokens,
					TotalTokens:     sourceUsage.TotalTokens,
					CacheReadTokens: sourceUsage.CacheReadTokens,
					CacheWrite:      sourceUsage.CacheWriteTokens,
					ReasoningTokens: sourceUsage.ReasoningTokens,
				}
			}
			if cost, ok := metrics.ExtractCost(full, s.costKeysFor(rec.Provider)); ok {
				rec.Cost = cost
			}
			copyResponseHeaders(w.Header(), resp.Header)
			// The translated body is JSON regardless of the upstream type.
			w.Header().Set("Content-Type", "application/json")
			// Set the translation warnings after the upstream copy so an
			// upstream header of the same name cannot replace or mask them.
			setWarningsHeader(w.Header(), warnings)
			w.WriteHeader(clientStatus)
			if _, werr := writeCounted(w, rec, out); werr != nil {
				markClientGone(rec)
			}
			return
		}

		// Spool the whole 2xx body before writing anything. The spool holds
		// the document in memory, so it takes the same process-wide
		// whole-document reservation as translation and discovery; otherwise
		// concurrent spools could multiply the budget. The reservation is
		// released once this attempt is classified and written, or abandoned
		// for a retry, before any re-send can spool again. The translated
		// branch above returns before this point, so the two never stack on
		// one attempt.
		reserve := qualitySpoolReserve(resp.ContentLength)
		if !s.respBudget.acquire(reserve) {
			writeResponseBufferBudget(w, rec, t.inbound)
			return
		}
		spooled, over, spoolErr := spoolBody(resp.Body)
		if over {
			// Past the cap the body streams through, but the bounded prefix
			// is still live in nonStreamBody's MultiReader until that write
			// finishes. Keep the reservation for the prefix, not just the
			// spool read, or concurrent over-cap bodies would all run with
			// the response budget at zero.
			defer s.respBudget.release(reserve)
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			s.nonStreamBody(ctx, w, io.MultiReader(bytes.NewReader(spooled), resp.Body), rec)
			return
		}
		if spoolErr != nil && spoolErr != io.EOF {
			s.respBudget.release(reserve)
			if ctx.Err() == context.Canceled {
				markClientGone(rec)
				resp.Body.Close()
				return
			}
			// The status line is still uncommitted: a partial upstream body
			// must not be relabeled as a successful 200. Surface a clean 502
			// through the shared transport-failure writer, and stamp the
			// decided status on the record first - the provisional upstream
			// 200 was captured before the body spooled, and writeClientError
			// only supplies a status when none is decided. A concurrent client
			// cancel keeps its markClientGone classification instead.
			markStreamErr(ctx, rec, spoolErr)
			if rec.ClientDisconnected {
				resp.Body.Close()
				return
			}
			rec.StatusCode = http.StatusBadGateway
			writeClientError(w, rec, t.inbound, errStreamRead, "upstream error: "+transportErrText(spoolErr), http.StatusBadGateway)
			resp.Body.Close()
			return
		}

		analysis := nonStreamAnalysis(spooled, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)

		if analysis.ErrorType != "" {
			// The provider failed in-band on a 200: its own statement. The
			// canonical decode (analysis.ErrorType) treats a decoy key
			// (null/empty/numeric error value - ParseErrorEnvelope returns ""
			// for those, the exact semantics the SSE analyzer uses) as NOT a
			// failure, so the body falls through to degenerate classification
			// instead of riding this verbatim path as a silently-empty
			// success. A retryable server-availability class
			// (retryableUpstreamErrorClass, the OpenAI retry semantics) with
			// quality budget left is absorbed and re-sent before any byte
			// reaches the client - nothing is written yet on this path, so
			// the re-send is always wire-safe; every other class, and an
			// exhausted budget, relays verbatim and lets the record carry it.
			if s.retryableUpstreamErrorClass(analysis.ErrorType, analysis.ErrorCode) && qualityLeft > 0 {
				qualityLeft--
				// The attempt log carries the provider's own envelope
				// (the explorer groups it under the provider's type).
				at := metrics.RetryAttempt{
					StatusCode: resp.StatusCode,
					ErrorType:  analysis.ErrorType,
					ErrorCode:  analysis.ErrorCode,
					ErrorMsg:   analysis.ErrorMsg,
					At:         time.Now(),
				}
				captureUpstreamMeta(&at, resp, t.authHeader)
				s.respBudget.release(reserve)
				next, nextCancel, ok := s.absorbResend(ctx, w, r, t, key, body, rec, groupKey, hooks, at, resp)
				if !ok {
					return
				}
				// ServeHTTP's defer still binds the FIRST body (and its
				// per-send cancel). Own this attempt's body + cancel; LIFO
				// closes the body, then cancels the send deadline.
				resp = next
				defer nextCancel()
				defer next.Body.Close()
				continue
			}
			s.commitSpooledBody(w, resp, spooled, rec, analysis)
			s.respBudget.release(reserve)
			return
		}

		if code := analysis.Outcome; code != "" || (len(bytes.TrimSpace(spooled)) == 0 && chatShapedResponse(t, body)) {
			if code == "" {
				// An entirely empty 200 body classifies as the same void the
				// structured forms do - gated on the REQUEST being chat-shaped
				// (a foreign passthrough endpoint's empty-but-legal 200 is not
				// our business). The empty-body class is the most common
				// degenerate form (litellm's "defaulting to empty chunk here").
				code = metrics.CodeEmptyCompletion
			}
			// The reasoning-only class consumes the thinking budget (the
			// streaming twin's rescue); every other degenerate class consumes
			// the quality budget. The attempt log carries the provider-error
			// view of the absorbed attempt for the reasoning-only class, the
			// raw class for the voids - matching each surface's exhausted
			// stamping below.
			thinkingClass := code == metrics.CodeReasoningOnly
			left := &qualityLeft
			if thinkingClass {
				left = &thinkingLeft
			}
			if *left > 0 {
				*left--
				// Absorb the degenerate attempt and retry the SAME request -
				// nothing was written to the client, so this is a safe
				// pre-write retry like the 429/5xx attempts.
				at := metrics.RetryAttempt{
					StatusCode: resp.StatusCode,
					ErrorType:  code,
					ErrorMsg:   metrics.DegenerateMessage(code),
					At:         time.Now(),
				}
				if thinkingClass {
					at.ErrorType = sse.TypeUpstreamError
					at.ErrorCode = code
				}
				captureUpstreamMeta(&at, resp, t.authHeader)
				s.respBudget.release(reserve)
				next, nextCancel, ok := s.absorbResend(ctx, w, r, t, key, body, rec, groupKey, hooks, at, resp)
				if !ok {
					return
				}
				// ServeHTTP's defer still binds the FIRST body (and its
				// per-send cancel). Own this attempt's body + cancel; LIFO
				// closes the body, then cancels the send deadline.
				resp = next
				defer nextCancel()
				defer next.Body.Close()
				continue
			}
			// Budget exhausted (or zero): surface a real failure. The
			// reasoning-only class is a provider-grade failure - it stamps
			// the upstream_error type (the streaming twin's in-band envelope
			// carries the same pair), so the explorer groups it with provider
			// errors on both surfaces; the void classes keep their
			// class-typed stamp.
			applyNonStreamAnalysis(rec, analysis)
			if thinkingClass {
				rec.ErrorType = sse.TypeUpstreamError
			} else {
				rec.ErrorType = code
			}
			rec.ErrorCode = code
			rec.ErrorMsg = metrics.DegenerateMessage(code)
			rec.StatusCode = http.StatusBadGateway
			writeProtocolHTTPErrorCode(w, t.inbound, http.StatusBadGateway, typeAPIError, code, metrics.DegenerateMessage(code))
			s.respBudget.release(reserve)
			return
		}

		// Healthy: commit the spooled body verbatim.
		s.applyNativeNonChatStructure(rec, t, spooled)
		s.commitSpooledBody(w, resp, spooled, rec, analysis)
		s.respBudget.release(reserve)
		return
	}
}

// streamBodyWithRetry relays a streaming response and transparently re-sends
// the SAME request when the attempt ended without any completion-contract
// bytes on the wire - the streaming twin of serveNonStreaming's degenerate-200
// retry. Three rescue sources share the loop: quality_retries owns the
// empty truncation (clean EOF before any content-bearing chunk) and the
// retryable in-band provider error dropped before the wire
// (rescueUpstreamError, retryableUpstreamErrorClass's server-availability
// classes - the OpenAI retry semantics), and thinking_retries owns the
// mid-thinking rescues - a truncation or upstream read error after
// reasoning-only output, a cleanly finished reasoning-only stream whose
// terminal region the hold still withholds (metrics.CodeReasoningOnly), and
// an in-band error that arrived after reasoning-only output. The failed
// attempt leaves the client at most role/keepalive/reasoning frames (every
// OpenAI SDK accumulates chat delta frames independently; reasoning is
// auxiliary display text), so the fresh stream appends cleanly to the
// committed SSE connection; the idle pacer keeps the socket alive during the
// re-send's queue/hold/backoff waits. finish_reason length is a clean
// terminator and never rescued. The absorbed attempt is logged like every
// other (Retries/Attempts, the dashboard's correction flag) while a rescue
// that eventually succeeds stays a success record; an exhausted budget
// surfaces the in-band error exactly as before, and a provider error that
// already reached the wire - content was relayed, the class is
// non-retryable, or the budget is spent - is verbatim relay and the client's
// to retry. Re-send failures are answered in-band: applyUpstream has
// already committed the status line before this function runs (even for
// clients that never asked for streaming), so no JSON error can land after
// relayed event-stream bytes, and the record carries the real error status.
// Format-translated streams (and Cursor's bidirectional bridge) keep their
// own signaling and are not re-sent here.
func (s *Server) streamBodyWithRetry(ctx context.Context, w http.ResponseWriter, resp *http.Response, r *http.Request, t *target, key string, body []byte, rec *metrics.Record, groupKey string, hooks scheduler.WaiterHooks) {
	qualityLeft := s.cfg().QualityRetries
	thinkingLeft := s.cfg().ThinkingRetries
	for {
		uerr := &upstreamErrorRescue{}
		// Seed the analyzer from the routed wire before any frame arrives.
		// The request-shape fallback stays for an endpoint the router does
		// not classify.
		wire := t.wire()
		if wire == "" && chatShapedResponse(t, body) {
			wire = ir.Chat
		}
		rescue := s.streamBody(ctx, w, resp.Body, rec, qualityLeft > 0, thinkingLeft > 0, uerr, wire)
		if rescue == rescueNone {
			return
		}
		// Absorb the failed attempt and retry the SAME request: no
		// completion-contract bytes reached the client, so this is a safe
		// re-send. The attempt log carries the provider-error view of the
		// absorbed attempt (the explorer groups it under upstream_error).
		at := metrics.RetryAttempt{
			StatusCode: resp.StatusCode,
			ErrorType:  sse.TypeUpstreamError,
			At:         time.Now(),
		}
		switch rescue {
		case rescueUpstreamError:
			// The provider's own in-band envelope: the attempt log and the
			// error explorer group the absorbed attempt under the provider's
			// error type (api_error and kin), not a synthetic class. The
			// budget mirrors the truncation twins: reasoning frames already
			// relayed put the rescue on the thinking budget.
			if uerr.reasoning {
				thinkingLeft--
			} else {
				qualityLeft--
			}
			at.ErrorType, at.ErrorCode, at.ErrorMsg = uerr.typ, uerr.code, uerr.msg
		case rescueReasoningStop:
			thinkingLeft--
			at.ErrorCode = metrics.CodeReasoningOnly
			at.ErrorMsg = metrics.DegenerateMessage(metrics.CodeReasoningOnly)
		case rescueReasoningTruncation:
			thinkingLeft--
			at.ErrorCode = metrics.CodeTruncated
			at.ErrorMsg = metrics.DegenerateMessage(metrics.CodeTruncated)
		default:
			qualityLeft--
			at.ErrorCode = metrics.CodeTruncated
			at.ErrorMsg = metrics.DegenerateMessage(metrics.CodeTruncated)
		}
		captureUpstreamMeta(&at, resp, t.authHeader)
		rec.Attempts = append(rec.Attempts, at)
		rec.Retries++
		s.publishUpdate(rec)
		s.finishStormResponse(ctx, true)
		resp.Body.Close()
		// The re-send is a retry: its opening WaitSend honors operator
		// holds ("a retry is a new send and waits").
		next, nextCancel, err := s.doWithRetry(ctx, groupKey, r, t, key, body, rec, hooks, true)
		if err != nil {
			clientGone, stormQueue := s.classifyResendFailure(r, rec, err)
			switch {
			case clientGone:
				return
			case stormQueue:
				// The status line is committed (applyUpstream ran before this
				// loop): the rejection goes in-band on the SSE socket.
				if typ, msg, ok := s.stormQueueErrorRecord(rec, err); ok {
					if werr := writeProtocolStreamError(w, rec, t.inbound, rec.ID, rec.Model, typ, "", msg); werr != nil {
						markClientGone(rec)
					}
				}
				return
			}
			if werr := writeProtocolStreamError(w, rec, t.inbound, rec.ID, rec.Model, typeUpstreamUnreachable, "", rec.ErrorMsg); werr != nil {
				markClientGone(rec)
			}
			return
		}
		// ServeHTTP's defer still binds the FIRST body (and its per-send
		// cancel). Own this attempt's body + cancel; LIFO closes the body,
		// then cancels the send deadline. ResponseBytes keeps ACCUMULATING
		// across this adoption (unlike the non-streaming twin): the abandoned
		// attempt's role/keepalive/reasoning frames genuinely reached the
		// client and the fresh stream appends behind them, so the running
		// total stays "the bytes the client received". FinalAttemptAt was
		// stamped at the re-send's admission inside doWithRetry (the same
		// anchor as the non-streaming twin).
		resp = next
		defer nextCancel()
		defer next.Body.Close()
		rec.StatusCode = resp.StatusCode
		captureUpstreamHeaders(resp, rec, t.authHeader)
		if isNonCompletionStatus(resp.StatusCode) {
			// The retry answered with an asynchronous acceptance or a
			// status-only response, not a completion stream. The first
			// attempt's 200 SSE status line is already committed, so the 202
			// body must never be relayed raw as SSE input. Surface one
			// protocol-native in-band error - the same truncated class an
			// exhausted empty retry would report - and stop before streamBody
			// can scan the JSON body as event-stream bytes.
			rec.ErrorType = sse.TypeUpstreamError
			rec.ErrorCode = metrics.CodeTruncated
			rec.ErrorMsg = metrics.DegenerateMessage(metrics.CodeTruncated)
			if werr := writeProtocolStreamError(w, rec, t.inbound, rec.ID, rec.Model, rec.ErrorType, rec.ErrorCode, rec.ErrorMsg); werr != nil {
				markClientGone(rec)
			}
			return
		}
		if resp.StatusCode >= 400 {
			// A non-200 retry cannot change the committed status line:
			// capture the detail and surface it in-band (the first-attempt
			// committed-SSE contract), never as raw JSON on the stream.
			s.captureErrorFromResponse(ctx, resp, rec)
			typ, msg := upstreamErrorFields(rec, resp.StatusCode)
			if werr := writeProtocolStreamError(w, rec, t.inbound, rec.ID, rec.Model, typ, "", msg); werr != nil {
				markClientGone(rec)
			}
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || !isEventStream(resp.Header.Get("Content-Type")) {
			// The first attempt committed an event-stream socket. Only another
			// 2xx event stream can continue that socket. A retry answering a
			// 1xx, a redirect, or a non-SSE 2xx completion (for example JSON)
			// must not be scanned and injected as raw stream bytes, and a
			// redirect is never followed: surface one protocol-native in-band
			// error and stop before streamBody reads the document as SSE.
			rec.ErrorType = sse.TypeUpstreamError
			rec.ErrorCode = metrics.CodeTruncated
			rec.ErrorMsg = metrics.DegenerateMessage(metrics.CodeTruncated)
			if werr := writeProtocolStreamError(w, rec, t.inbound, rec.ID, rec.Model, rec.ErrorType, rec.ErrorCode, rec.ErrorMsg); werr != nil {
				markClientGone(rec)
			}
			return
		}
	}
}
