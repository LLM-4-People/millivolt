package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// costKeysFor returns the per-provider cost key paths from config (empty when
// the provider has no override, in which case the built-in auto-detection in
// metrics.ExtractCost applies). Providers report cost under different key
// names; this is the field-name mapping, never a static price.
func (s *Server) costKeysFor(provider string) []string {
	if ov, ok := s.cfg().Providers[provider]; ok {
		return ov.CostKeys
	}
	return nil
}

// usageKeysFor returns the per-provider usage field-name overrides from config
// (canonical field -> custom key path), empty when the provider has none.
func (s *Server) usageKeysFor(provider string) map[string]string {
	if ov, ok := s.cfg().Providers[provider]; ok {
		return ov.UsageKeys
	}
	return nil
}

// analyzerFor returns the SSE analyzer wired for one relayed record: the
// provider's configured cost/usage key paths and the capture-preview gate.
// The analyzer's zero value is otherwise stream-ready - every other field is
// unexported per-line state - so this is the whole external wiring.
func (s *Server) analyzerFor(rec *metrics.Record) sse.Analyzer {
	var a sse.Analyzer
	a.CostKeys = s.costKeysFor(rec.Provider)
	a.UsageKeys = s.usageKeysFor(rec.Provider)
	a.CapturePreview = s.cfg().CaptureBodyPreview
	return a
}

// markClientGone records the LOCAL client disappearing mid-relay (a write to
// it failed on a broken pipe, or its request context canceled): the client's
// own cancellation - client_disconnected, the documented `cancel` bucket,
// never an error by itself. Status 499 is stamped only while NO error
// outcome is decided (status < 400: 0 pre-outcome, or a committed 2xx/3xx
// whose stream never finished - the client disconnected BEFORE a final
// outcome). A decided 4xx/5xx/429 keeps exactly what the upstream sent:
// 429 stays flow control, a 5xx stays a genuine error ("a genuine 5xx counts
// even when the client then cancelled") with ClientDisconnected also true,
// so the upstream failure never vanishes from the error rate, the error
// explorer, or the errors-only purge. The single owner of the client-gone
// classification; every post-commit write failure funnels through here
// (Cursor keeps its own documented exception in cursor_bidi.go - the
// upstream 200 stays).
func markClientGone(rec *metrics.Record) {
	rec.ClientDisconnected = true
	if rec.StatusCode < 400 {
		rec.StatusCode = metrics.StatusClientClosedRequest
	}
}

// errStreamRead is the persisted error-type vocabulary member for a failure
// reading or translating an upstream body stream: markStreamErr's record
// stamp, and the in-band type the cursor turn error surface emits.
const errStreamRead = "stream_read_error"

// markStreamErr classifies an error from an upstream body copy/scan loop: when
// the LOCAL client disconnected (its request context canceled) the loop failed
// because the proxy's relay to the client died - that is the client's own
// cancellation, recorded as a client disconnect (the 499/cancel bucket when no
// error outcome was decided yet; a decided error status stays, per
// markClientGone) and NOT as an error. Anything else is genuine upstream
// trouble (or a translation failure) and gets the stream-read error type
// stamped (the already-committed upstream status stays on the record). The
// single choke point for every relay loop; Cursor paths keep their own
// documented exception (TurnResult.ClientAbort in cursor_bidi.go - the
// upstream DID answer, so the 200 stays).
func markStreamErr(ctx context.Context, rec *metrics.Record, err error) {
	if ctx.Err() == context.Canceled {
		markClientGone(rec)
		return
	}
	rec.ErrorType = errStreamRead
	rec.ErrorMsg = err.Error()
}

// classifyResendFailure runs the shared failure policy of the two relay
// quality re-send loops (serveNonStreaming, streamBodyWithRetry): the LOCAL
// client's own cancellation is markClientGone (never an upstream error), an
// operator storm-queue rejection is reported for the caller's per-surface
// sink (which owns the record stamp), and the genuine transport default
// stamps the record trio - 502 + upstream_unreachable + transport text -
// ahead of the caller's sink. The wire sinks stay per-surface (see
// writeTransportFailure, the fresh-send twin): the non-streaming loop's
// status line is still open, so it answers with an api_error JSON http.Error;
// the streaming loop's status line is already committed, so it emits
// in-band on the SSE socket.
func (s *Server) classifyResendFailure(r *http.Request, rec *metrics.Record, err error) (clientGone, stormQueue bool) {
	if r.Context().Err() == context.Canceled {
		markClientGone(rec)
		return true, false
	}
	if isStormQueueRejection(err) {
		return false, true
	}
	rec.StatusCode = http.StatusBadGateway
	rec.ErrorType = typeUpstreamUnreachable
	rec.ErrorMsg = transportErrText(err)
	return false, false
}

// nonStreamBody relays a non-streaming response verbatim while capturing the
// usage block (and any error payload) for metrics. The body is scanned
// incrementally so large responses are not fully buffered. A read error caused
// by the LOCAL client disconnecting (ctx canceled by the client's close) is
// recorded as a client disconnect, never as an upstream failure.
func (s *Server) nonStreamBody(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record) {
	// Read into a single buffer; on EOF, parse usage from the captured bytes.
	buf := make([]byte, 0, nonStreamGrowInit)
	chunk := make([]byte, nonStreamChunk)
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			if _, werr := w.Write(chunk[:n]); werr != nil {
				markClientGone(rec)
				return
			}
			// Keep a bounded prefix. Large documents can exceed this capture
			// and then have unavailable metrics; forwarding remains verbatim.
			if len(buf)+n <= nonStreamCaptureMax {
				buf = append(buf, chunk[:n]...)
			}
		}
		if err != nil {
			if err != io.EOF {
				markStreamErr(ctx, rec, err)
			}
			break
		}
	}
	if len(buf) > 0 {
		analyzeNonStreamBytes(buf, rec, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)
	}
}

// analyzeNonStreamBytes runs the non-streaming metrics extraction (usage,
// finish_reason, tool calls, cost, in-band error envelope) over the captured
// body bytes. Shared by the teeing relay (nonStreamBody) and the spooled
// quality path (serveNonStreaming) so the two can never drift.
func analyzeNonStreamBytes(buf []byte, rec *metrics.Record, usageKeys map[string]string, costKeys []string, preview bool) {
	if len(buf) == 0 {
		return
	}
	if rec.Usage.OutputTokens == 0 {
		extractUsageBytes(buf, rec, usageKeys, preview)
	}
	// Cost is read straight from the provider's response (dynamic), so it
	// reflects what the provider actually charged for this request.
	if cost, ok := metrics.ExtractCost(buf, costKeys); ok {
		rec.Cost = cost
	}
	// A 200 response can still carry a top-level {"error":{...}} envelope
	// (the in-band failure shape; streaming's counterpart is captured by
	// the SSE analyzer). Null/empty error values are decoys, not failures
	// - the envelope decoder returns "" for those, and normal completion
	// bodies have no top-level error key at all, so success paths are
	// never mislabeled. The client received a failure - record it.
	if rec.ErrorType == "" && metrics.HasErrorKey(buf) {
		if typ, code, msg := metrics.ParseErrorEnvelope(buf); typ != "" {
			rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = typ, code, msg
		}
	}
}

// chatRequestBody reports whether the upstream request body was a chat/
// completions-shaped call (the "\"messages\""/"\"input\""/"\"prompt\"" keys),
// the gate for classifying an EMPTY 200 body as a degenerate completion: a
// foreign passthrough endpoint's empty-but-legal response is never flagged.
func chatRequestBody(b []byte) bool {
	return bytes.Contains(b, []byte(`"messages"`)) ||
		bytes.Contains(b, []byte(`"input"`)) ||
		bytes.Contains(b, []byte(`"prompt"`))
}

// spoolBody drains body up to qualitySpoolMax (plus one chunk) before
// returning. overflow=true when the cap was hit - the body is certainly not a
// degenerate shell and the caller commits to verbatim relay. err is the
// terminal read error (io.EOF for a complete body).
func spoolBody(body io.Reader) (b []byte, overflow bool, err error) {
	buf := make([]byte, nonStreamChunk)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if len(b)+n > qualitySpoolMax {
				b = append(b, buf[:n]...)
				return b, true, nil
			}
			b = append(b, buf[:n]...)
		}
		if rerr != nil {
			return b, false, rerr
		}
	}
}

// commitSpooledBody writes a fully spooled non-stream body verbatim: response
// headers, the status line, the bytes, then the shared non-stream analysis.
// A failed client write marks the disconnect and stops before the analysis.
// The gates that choose this epilogue (the in-band error envelope passthrough
// and the healthy commit) stay at their call sites.
func (s *Server) commitSpooledBody(w http.ResponseWriter, resp *http.Response, spooled []byte, rec *metrics.Record) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(spooled); werr != nil {
		markClientGone(rec)
		return
	}
	analyzeNonStreamBytes(spooled, rec, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)
}

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
// ordering) before adopting the record fields.
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
			s.writeStormQueueError(w, rec, err)
			return nil, nil, false
		}
		http.Error(w, errJSON(typeAPIError, "upstream error: "+transportErrText(err)), http.StatusBadGateway)
		return nil, nil, false
	}
	return next, nextCancel, true
}

// serveNonStreaming relays a non-streaming response with quality handling. A
// 200 whose body classifies degenerate (metrics.ClassifyNonStreamBody -
// same predicate as the streaming paths) triggers a transparent retry BEFORE
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
		if resp.StatusCode != http.StatusOK {
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if isEventStream(resp.Header.Get("Content-Type")) {
				if _, cerr := io.Copy(disconnectWriter{w, rec}, resp.Body); cerr != nil {
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

		// Format translation is fully buffered by design; the status line is
		// written only after a successful translation, so a failed translation
		// carries a real error status instead of a committed 200.
		if t.format != "" && t.format != "openai" {
			full, rerr := io.ReadAll(io.LimitReader(resp.Body, maxTranslateBodyBytes+1))
			if rerr != nil {
				markStreamErr(ctx, rec, rerr)
				if rec.ClientDisconnected {
					return
				}
				rec.StatusCode = http.StatusBadGateway
				http.Error(w, errJSON(typeAPIError, "upstream error: "+transportErrText(rerr)), http.StatusBadGateway)
				return
			}
			// Past the cap the document cannot be translated, and verbatim
			// relay would break the translated-shape contract: fail closed
			// with a real 502 (the status line is still uncommitted here).
			if int64(len(full)) > maxTranslateBodyBytes {
				rec.StatusCode = http.StatusBadGateway
				rec.ErrorType = "response_too_large"
				rec.ErrorMsg = "upstream translated body exceeds " + config.FormatByteSize(maxTranslateBodyBytes)
				http.Error(w, errJSON(typeAPIError, "upstream response too large to translate"), http.StatusBadGateway)
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
			out, terr := translateResponse(t.format, full)
			if terr != nil {
				rec.StatusCode = http.StatusBadGateway
				rec.ErrorType = "transform_error"
				rec.ErrorMsg = terr.Error()
				if errors.Is(terr, metrics.ErrMetricRange) {
					invalidateUsage(rec, terr)
				}
				http.Error(w, errJSON(typeAPIError, "translate upstream response: "+terr.Error()), http.StatusBadGateway)
				return
			}
			// The translated response owns completion shape and normalized
			// usage. Custom cost paths still address the ORIGINAL document.
			analyzeNonStreamBytes(out, rec, s.usageKeysFor(rec.Provider), nil, s.cfg().CaptureBodyPreview)
			if cost, ok := metrics.ExtractCost(full, s.costKeysFor(rec.Provider)); ok {
				rec.Cost = cost
			}
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if _, werr := w.Write(out); werr != nil {
				markClientGone(rec)
			}
			return
		}

		// Spool the whole 2xx body before writing anything.
		spooled, over, spoolErr := spoolBody(resp.Body)
		if over {
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			s.nonStreamBody(ctx, w, io.MultiReader(bytes.NewReader(spooled), resp.Body), rec)
			return
		}
		if spoolErr != nil && spoolErr != io.EOF {
			markStreamErr(ctx, rec, spoolErr)
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if _, werr := w.Write(spooled); werr != nil {
				markClientGone(rec)
			}
			resp.Body.Close()
			return
		}

		if metrics.HasErrorKey(spooled) {
			// The provider failed in-band on a 200: its own statement. The
			// cheap byte-scan gate is followed by the canonical decode: a
			// decoy key (null/empty/numeric error value - ParseErrorEnvelope
			// returns "" for those, the exact semantics the SSE analyzer uses)
			// is NOT a failure, so the body falls through to degenerate
			// classification instead of riding this verbatim path as a
			// silently-empty success. A retryable server-availability class
			// (retryableUpstreamErrorClass, the OpenAI retry semantics) with
			// quality budget left is absorbed and re-sent before any byte
			// reaches the client - nothing is written yet on this path, so
			// the re-send is always wire-safe; every other class, and an
			// exhausted budget, relays verbatim and lets the record carry it.
			if typ, code, msg := metrics.ParseErrorEnvelope(spooled); typ != "" {
				if s.retryableUpstreamErrorClass(typ, code) && qualityLeft > 0 {
					qualityLeft--
					// The attempt log carries the provider's own envelope
					// (the explorer groups it under the provider's type).
					at := metrics.RetryAttempt{
						StatusCode: resp.StatusCode,
						ErrorType:  typ,
						ErrorCode:  code,
						ErrorMsg:   msg,
						At:         time.Now(),
					}
					captureUpstreamMeta(&at, resp, t.authHeader)
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
					rec.StatusCode = resp.StatusCode
					if resp.StatusCode >= 400 {
						captureErrorFromResponse(resp, rec)
					}
					captureUpstreamHeaders(resp, rec, t.authHeader)
					rec.FinalAttemptAt = time.Now()
					continue
				}
				s.commitSpooledBody(w, resp, spooled, rec)
				return
			}
		}

		if code := metrics.ClassifyNonStreamBody(spooled); code != "" || (len(bytes.TrimSpace(spooled)) == 0 && chatRequestBody(body)) {
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
				rec.StatusCode = resp.StatusCode
				if resp.StatusCode >= 400 {
					captureErrorFromResponse(resp, rec)
				}
				captureUpstreamHeaders(resp, rec, t.authHeader)
				rec.FinalAttemptAt = time.Now()
				continue
			}
			// Budget exhausted (or zero): surface a real failure. The
			// reasoning-only class is a provider-grade failure - it stamps
			// the upstream_error type (the streaming twin's in-band envelope
			// carries the same pair), so the explorer groups it with provider
			// errors on both surfaces; the void classes keep their
			// class-typed stamp.
			analyzeNonStreamBytes(spooled, rec, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)
			if thinkingClass {
				rec.ErrorType = sse.TypeUpstreamError
			} else {
				rec.ErrorType = code
			}
			rec.ErrorCode = code
			rec.ErrorMsg = metrics.DegenerateMessage(code)
			rec.StatusCode = http.StatusBadGateway
			http.Error(w, errJSONCode(typeAPIError, metrics.DegenerateMessage(code), code), http.StatusBadGateway)
			return
		}

		// Healthy: commit the spooled body verbatim.
		s.commitSpooledBody(w, resp, spooled, rec)
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
		rescue := s.streamBody(ctx, w, resp.Body, rec, qualityLeft > 0, thinkingLeft > 0, uerr)
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
					if werr := emitErrorSSE(w, rec.ID, typ, msg); werr != nil {
						markClientGone(rec)
					}
				}
				return
			}
			if werr := emitErrorSSE(w, rec.ID, typeUpstreamUnreachable, rec.ErrorMsg); werr != nil {
				markClientGone(rec)
			}
			return
		}
		// ServeHTTP's defer still binds the FIRST body (and its per-send
		// cancel). Own this attempt's body + cancel; LIFO closes the body,
		// then cancels the send deadline.
		resp = next
		defer nextCancel()
		defer next.Body.Close()
		rec.StatusCode = resp.StatusCode
		rec.FinalAttemptAt = time.Now()
		captureUpstreamHeaders(resp, rec, t.authHeader)
		if resp.StatusCode >= 400 {
			// A non-200 retry cannot change the committed status line:
			// capture the detail and surface it in-band (the first-attempt
			// committed-SSE contract), never as raw JSON on the stream.
			captureErrorFromResponse(resp, rec)
			typ, msg := upstreamErrorFields(rec, resp.StatusCode)
			if werr := emitErrorSSE(w, rec.ID, typ, msg); werr != nil {
				markClientGone(rec)
			}
			return
		}
	}
}

// extractUsageBytes parses the response body: token usage (auto-detecting
// OpenAI/Anthropic key names, with per-provider overrides), model, request id,
// tool calls, finish_reason, and - only when preview is enabled - a bounded
// response preview. Best-effort: silently ignores parse failures.
func extractUsageBytes(body []byte, rec *metrics.Record, usageKeys map[string]string, preview bool) {
	// Decode the usage object generically so we can walk provider-specific key
	// names; the rest of the body uses typed fields.
	var raw struct {
		Usage   map[string]any `json:"usage"`
		Model   string         `json:"model"`
		ID      string         `json:"id"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role string `json:"role"`
				// Content can be a string OR an array of parts (OpenAI multimodal
				// shape); keep it raw so either form registers as content presence.
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}
	if raw.Usage != nil {
		rec.Usage = metrics.ParseUsage(raw.Usage, usageKeys)
	}
	if raw.Model != "" && rec.ProviderModel == "" {
		rec.ProviderModel = raw.Model
	}
	if raw.ID != "" && rec.ProviderRequestID == "" {
		rec.ProviderRequestID = raw.ID
	}
	if len(raw.Choices) > 0 {
		if raw.Choices[0].FinishReason != "" {
			rec.FinishReason = raw.Choices[0].FinishReason
		}
		// Content presence drives the tool-call-only detection in FinalizeRecord
		// (a content:null tool_calls response bills only tool args). Non-streaming
		// never sets FirstAnswerAt, so flag content directly through the shared
		// flattenContent walk: any non-empty string or parts text counts.
		if s, ok := flattenContent(raw.Choices[0].Message.Content); ok && s != "" {
			rec.HadAnswerContent = true
			// Response preview: a bounded rune-safe prefix of the text content,
			// stored only when capture_body_preview is enabled (off by default).
			if preview && rec.ResponsePreview == "" {
				rec.ResponsePreview = metrics.TruncatePreview(s)
			}
		}
		if n := len(raw.Choices[0].Message.ToolCalls); n > 0 {
			rec.ToolCalls = n
			names := make([]string, 0, n)
			for _, tc := range raw.Choices[0].Message.ToolCalls {
				if tc.Function.Name != "" {
					names = append(names, tc.Function.Name)
				}
			}
			rec.ToolNames = names
		}
	}
}

// parseErrorBody extracts the provider's structured error detail (type, code,
// message) from a response body via the canonical envelope decoder
// (metrics.ParseErrorEnvelope), defaulting the type when absent. Shared by the
// final-response path and the per-retry-attempt log so both parse identically.
//
// The message is bounded (metrics.TruncatePreview) because a provider's error
// message can echo user-supplied content and is otherwise arbitrary
// provider-controlled bytes - the record stays bounded regardless of
// capture_body_preview. When the (bounded) body doesn't parse (e.g. it was
// truncated mid-token), fall back to a bounded raw prefix so the record keeps a
// classifiable signature instead of a blank one.
func parseErrorBody(b []byte) (typ, code, msg string) {
	if len(b) == 0 {
		return "", "", ""
	}
	if typ, code, msg = metrics.ParseErrorEnvelope(b); typ != "" {
		return typ, code, msg
	}
	if json.Valid(b) {
		// Decodable but null/empty error value: classify, no message.
		return "provider_error", "", ""
	}
	// Unparseable (e.g. an oversized body truncated mid-token): keep a
	// bounded raw prefix so the error is still classifiable.
	return "provider_error", "", metrics.TruncatePreview(string(b))
}

// upstreamMeta is the per-response provider metadata read from response
// headers. readUpstreamMeta is the single owner of the header-name
// vocabulary - the request-id aliases, server, processing time, echoed model
// and rate-limit state - shared by the record's final-response capture and
// the per-attempt capture, so every attempt stores exactly the same
// information. The has* flags preserve the record path's only-when-present
// semantics: a re-capture never zeroes a field the response did not carry.
type upstreamMeta struct {
	RequestID          string
	Server             string
	Model              string
	ProcessingMs       int
	RateLimitRemaining int
	RateLimitLimit     int
	hasProcessingMs    bool
	hasRemaining       bool
	hasLimit           bool
}

// readUpstreamMeta extracts one response's upstream metadata (request id,
// server, processing time, echoed model, rate-limit state) from its headers.
func readUpstreamMeta(h http.Header) upstreamMeta {
	m := upstreamMeta{
		RequestID: firstNonEmpty(
			h.Get("X-Request-Id"),
			h.Get("X-Openai-Request-Id"),
			h.Get("X-Api-Request-Id"),
			h.Get("Request-Id"),
		),
		Server: h.Get("Server"),
		Model:  h.Get("X-Model"),
	}
	m.ProcessingMs, m.hasProcessingMs = headerIntValue(h, "X-Openai-Processing-Ms", "anthropic-processing-ms")
	m.RateLimitRemaining, m.hasRemaining = headerIntValue(h, "X-Ratelimit-Remaining-Requests", "anthropic-ratelimit-requests-remaining")
	m.RateLimitLimit, m.hasLimit = headerIntValue(h, "X-Ratelimit-Limit-Requests", "anthropic-ratelimit-requests-limit")
	return m
}

// captureUpstreamHeaders refreshes the record's upstream header metadata -
// the redacted audit headers, provider request id/server/processing time and
// echoed model, and rate-limit state - from the given response. ServeHTTP
// captures for the first response; the quality loops re-capture after every
// successful re-send so the record describes the attempt whose body the
// client actually received.
func captureUpstreamHeaders(resp *http.Response, rec *metrics.Record, authHeader string) {
	rec.ResponseHeaders = captureHeaders(resp.Header, authHeader)
	m := readUpstreamMeta(resp.Header)
	rec.ProviderRequestID = m.RequestID
	rec.ProviderServer = m.Server
	if m.Model != "" {
		rec.ProviderModel = m.Model
	}
	if m.hasProcessingMs {
		rec.ProcessingMs = m.ProcessingMs
	}
	if m.hasRemaining {
		rec.RateLimitRemaining = m.RateLimitRemaining
	}
	if m.hasLimit {
		rec.RateLimitLimit = m.RateLimitLimit
	}
}

// captureUpstreamMeta stores the same upstream-response metadata on an
// absorbed attempt - the attempt-log twin of captureUpstreamHeaders
// (readUpstreamMeta owns the vocabulary). The attempt is built fresh for
// this response, so absent headers stay zero; there is no previous value to
// preserve and nothing re-captures an attempt later.
func captureUpstreamMeta(at *metrics.RetryAttempt, resp *http.Response, authHeader string) {
	at.ResponseHeaders = captureHeaders(resp.Header, authHeader)
	m := readUpstreamMeta(resp.Header)
	at.ProviderRequestID = m.RequestID
	at.ProviderServer = m.Server
	at.ProviderModel = m.Model
	at.ProcessingMs = m.ProcessingMs
	at.RateLimitRemaining = m.RateLimitRemaining
	at.RateLimitLimit = m.RateLimitLimit
}

// captureErrorFromResponse reads a bounded portion of an error response body
// to record the provider's error type/message, then rewinds the body so it
// can still be relayed to the client. The rewind REPLACES resp.Body with a
// NopCloser: a caller that must close the real body has to register its
// body-Close defer BEFORE this call (defer evaluates resp.Body at
// registration, so an early-registered defer still closes the real body).
func captureErrorFromResponse(resp *http.Response, rec *metrics.Record) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
	if err == nil && len(b) > 0 {
		rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = parseErrorBody(b)
	}
	// Re-serve the captured prefix followed by the live remainder so the client
	// still receives the FULL original body byte-for-byte (the prefix alone
	// would truncate a large error payload). resp.Body is still open, positioned
	// right after the prefix we read.
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), resp.Body))
}

// prefixRemainderBody re-serves a captured body prefix followed by the live
// remainder of the original body, closing that original body exactly once on
// Close. Used when a peeked error body overflowed the capture cap: the final
// response must relay byte-for-byte ("an exhausted budget surfaces the last
// upstream response verbatim"), so the unread remainder stays live instead of
// being truncated away.
type prefixRemainderBody struct {
	prefix []byte
	rest   io.ReadCloser
}

func (b *prefixRemainderBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	return b.rest.Read(p)
}

func (b *prefixRemainderBody) Close() error { return b.rest.Close() }

// quota429Peek is the single owner of the bounded durable-quota peek on a 429
// response: it reads at most maxErrBodyBytes+1 bytes of the error body and
// classifies the in-cap envelope through the canonical parser
// (metrics.ParseErrorEnvelope + isNonRetryableQuotaErr). An in-cap body is
// closed and reported together with its durable classification and the
// envelope's protocol tokens (type, code - the quota-pause reason label); an
// overflowed body (longer than maxErrBodyBytes) is deliberately left OPEN
// and unclassified (durable=false, leftOpen=true) - the classification stays
// bounded by the cap. Each transport then applies its own overflow policy at
// the call site: the generic relay re-serves prefix + live remainder
// verbatim, the cursor transport closes and re-wraps only the truncated
// prefix.
func quota429Peek(resp *http.Response) (durable bool, typ, code string, peeked []byte, leftOpen bool) {
	peeked, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes+1))
	if len(peeked) > maxErrBodyBytes {
		return false, "", "", peeked, true
	}
	resp.Body.Close()
	typ, code, _ = metrics.ParseErrorEnvelope(peeked)
	return isNonRetryableQuotaErr(typ, code), typ, code, peeked, false
}

// withSendTimeout derives the per-attempt upstream send context: the
// X-Proxy-Timeout-Ms routing header bounds ONE send (upstream headers + that
// body's relay phase), so a fresh deadline anchors at each attempt's Do -
// waits that precede the send (operator holds, group serialization, backoff,
// Retry-After windows) run on the base request context and never burn the
// budget. A non-positive timeout returns the base context with a no-op cancel
// so callers can always defer the returned func.
func withSendTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// admitSendAttempt gates one upstream send attempt on the retry-driver
// admission policy shared by the generic relay and the cursor bidi driver:
// the WaitSend hold rule (the opening send honors operator holds when it is
// itself a retry - "a retry is a new send and waits"; later attempts always
// do), the storm permit, and the attemptStart stamp taken only after every
// admission gate so waits never burn send deadlines. own is WaitSend's
// returned token (unchanged on error). The caller keeps its own
// end-of-request choreography.
func (s *Server) admitSendAttempt(ctx context.Context, groupKey string, hooks scheduler.WaiterHooks, firstSendIsRetry bool, attempt int, own bool) (bool, *scheduler.StormPermit, time.Time, error) {
	own, err := s.scheduler.WaitSend(ctx, groupKey, hooks, attempt > 0 || firstSendIsRetry, own)
	if err != nil {
		return own, nil, time.Time{}, err
	}
	permit, err := s.waitStorm(ctx, hooks)
	if err != nil {
		return own, nil, time.Time{}, err
	}
	return own, permit, time.Now(), nil
}

// absorbTransportRetry books one absorbed transport-level failure: the
// attempt-log entry (transport class, the caller's rendered message), the
// retry counter with a live publish, and the Trip that paces the group and
// keeps the send token claimed. Which transport errors are retryable, the
// message rendering, and the give-up condition stay per-transport. Returns
// the send-token ownership after Trip.
func (s *Server) absorbTransportRetry(rec *metrics.Record, groupKey, msg string, own bool) bool {
	rec.Attempts = append(rec.Attempts, metrics.RetryAttempt{
		StatusCode: 0,
		ErrorType:  "transport",
		ErrorMsg:   msg,
		At:         time.Now(),
	})
	rec.Retries++
	s.publishUpdate(rec)
	return s.scheduler.Trip(groupKey, s.scheduler.BackoffFor(groupKey, 0)) || own
}

// absorbHTTPRetry books one absorbed retryable HTTP response: the provider
// retry hint (parseRetryAfter) and the attempt-log entry (status, bounded
// error-body envelope via parseErrorBody, hint milliseconds, the response's
// upstream metadata via captureUpstreamMeta), appended with the retry
// counter. Returns the hint - the pacing floor for the caller's Trip and
// RetryAfterMs stamp. The bounded body read, per-send cleanup, record
// stamps and publish order stay per-transport: the generic relay interplays
// with the durable-quota 429 peek and clears stale error fields after its
// publish; the cursor driver closes its duplex pipe and stamps before
// publishing.
func (s *Server) absorbHTTPRetry(rec *metrics.Record, resp *http.Response, errBody []byte, authHeader string) time.Duration {
	retryAfter := parseRetryAfter(resp)
	at := metrics.RetryAttempt{
		StatusCode:   resp.StatusCode,
		RetryAfterMs: int(retryAfter.Milliseconds()),
		At:           time.Now(),
	}
	at.ErrorType, at.ErrorCode, at.ErrorMsg = parseErrorBody(errBody)
	captureUpstreamMeta(&at, resp, authHeader)
	rec.Attempts = append(rec.Attempts, at)
	rec.Retries++
	return retryAfter
}

// doWithRetry executes the upstream request, transparently absorbing transient
// failures: 429 and any 5xx are retried with backoff while the client has not
// yet received any bytes, so the client only ever sees the final outcome. It
// parses provider retry hints (Retry-After, x-ratelimit-reset) as a floor on
// the wait; adaptive exponential backoff still grows. Any retry (429, 5xx,
// transport) trips the provider+key group: that request owns the
// next send. After success or exhaust, exactly one sibling is the probe; a
// clean first-attempt success restores full concurrency, another retry keeps
// it at one-at-a-time. Exhausted retryable failures double a request-level
// backoff (base_backoff → max_backoff); a success resets it. WaitSend also
// honors operator pause on retries and remaining nextAllowedAt across pause.
// When retries are exhausted the last upstream response is returned as-is
// (failures are never masked). A 429 whose error envelope is a durable
// quota/billing condition (metrics.IsNonRetryableQuotaErr) follows
// quota_pause_mode: unarmed targets surface it immediately (waiting cannot
// clear it), armed OpenAI-wire targets park the provider and this request
// behind the quota gate or hold until a re-send resolves 2xx - those quota
// re-sends never count against the transient retry budget.
//
// firstSendIsRetry marks an invocation whose OPENING send is itself a retry
// (the degenerate-200 quality re-run): its first WaitSend honors operator
// holds exactly like a 429/5xx retry - "a retry is a new send and waits" -
// instead of the first-attempt exemption that lets an already-acquired
// in-flight request finish.
//
// The returned CancelFunc belongs to the FINAL response's per-send deadline
// (X-Proxy-Timeout-Ms): the caller must defer it past the body relay so the
// deadline keeps governing the body exactly like the request-lifetime wrap it
// replaced, and so absorbed attempts' contexts (canceled inside, right after
// their bodies are drained) never leak. It is always non-nil on success.
func (s *Server) doWithRetry(ctx context.Context, groupKey string, r *http.Request, t *target, key string, body []byte, rec *metrics.Record, hooks scheduler.WaiterHooks, firstSendIsRetry bool) (*http.Response, context.CancelFunc, error) {
	// MaxRetries comes from config (single source of truth for the default).
	maxRetries := s.cfg().MaxRetries
	own := false
	retried := false
	// ordinary counts only transient absorb attempts (429/5xx/transport).
	// Quota-pause re-sends park for an account condition, not a transient
	// failure, and must never exhaust the transient budget.
	ordinary := 0
	end := func(failed bool) {
		if failed {
			// Exhausted retryable: double request backoff, keep one probe.
			s.scheduler.FailSend(groupKey, own)
			return
		}
		if own {
			// Retried → one next probe. Clean first-attempt → full concurrency.
			s.scheduler.EndSend(groupKey, retried)
		}
	}
	// permit and attemptStart are declared outside the loop so the admission
	// call can assign them (and own) with a plain = : a := would shadow the
	// outer own inside the loop body and silently drop the Trip-claimed send
	// token between attempts.
	var (
		permit       *scheduler.StormPermit
		attemptStart time.Time
	)
	for attempt := 0; ; attempt++ {
		var err error
		own, permit, attemptStart, err = s.admitSendAttempt(ctx, groupKey, hooks, firstSendIsRetry, attempt, own)
		if err != nil {
			end(false)
			return nil, nil, err
		}
		// Per-send deadline: X-Proxy-Timeout-Ms bounds this ONE send (the
		// Do below and the returned body), anchored on the attemptStart the
		// admission helper stamped - a fresh budget per attempt, never eaten
		// by the waits above.
		attemptCtx, attemptCancel := withSendTimeout(ctx, t.timeout)
		// Rebuild the upstream request (body can't be reused across retries).
		upstream, err := s.buildUpstreamRequest(attemptCtx, r, t, key, body)
		if err != nil {
			permit.Cancel()
			attemptCancel()
			end(false)
			return nil, nil, err
		}
		resp, err := s.httpClient().Do(upstream)
		if err != nil {
			// Observe cancellation before cleanup: canceling our own send
			// context must not turn a transient transport error into an abort.
			attemptErr := attemptCtx.Err()
			attemptCancel() // no response body was received; nothing to drain
			// Caller aborted (client disconnect / Ctrl+C) or the per-send
			// deadline fired - never retry; surface verbatim.
			if ctx.Err() != nil || attemptErr != nil {
				permit.Cancel()
				end(false)
				return nil, nil, err
			}
			// Genuine transport failure (no response received). Retry the
			// transient ones (header timeout, conn reset/refused, EOF, DNS
			// timeout) transparently; give up on permanent ones (NXDOMAIN,
			// TLS errors) or when the retry budget is spent.
			retryable := isRetryableTransportErr(err)
			active := s.observeStormTransport(ctx, permit, retryable)
			if !retryable || ordinary >= maxRetries && !s.allowStormRetry(ctx, active) {
				end(retryable)
				return nil, nil, err
			}
			own = s.absorbTransportRetry(rec, groupKey, transportErrText(err), own)
			ordinary++
			retried = true
			continue
		}

		// A 429 is normally transient flow control, but a 429 can also be the
		// provider announcing a durable account condition (quota/credits/spend
		// limits) that waiting can never clear. Only the body's structured
		// error type/code tells them apart, so the bounded envelope peek
		// (quota429Peek) runs BEFORE the retry decision. With the quota pause
		// armed for this target, a durable 429 instead parks the provider and
		// this request: the attempt is absorbed, settleQuotaFailure opens the
		// recovery gate (retry mode) or the provider hold (manual mode), and
		// the next admission below waits out the pause - the loop re-sends
		// until a send resolves 2xx and the queue drains. Unarmed targets
		// surface the 429 immediately - no retry budget burned, no pacing of
		// the group, the absorbed-attempt log stays clean (nothing was
		// absorbed).
		//
		// This relay's overflow policy: a peek that OVERFLOWS the cap leaves
		// the body open (peekLeftOpen), so the final relay can serve
		// prefix + live remainder verbatim and an oversized 429 keeps normal
		// retry semantics.
		var errBody []byte
		peekLeftOpen := false
		if resp.StatusCode == http.StatusTooManyRequests {
			var durable bool
			var quotaTyp, quotaCode string
			durable, quotaTyp, quotaCode, errBody, peekLeftOpen = quota429Peek(resp)
			if durable {
				if !s.settleQuotaFailure(permit, t.format, hooks.Client, hooks.Provider, quotaReason(quotaTyp, quotaCode), parseRetryAfter(resp)) {
					if attempt > 0 {
						rec.FinalAttemptAt = attemptStart
					}
					end(false)
					// Re-serve the captured bytes so the client still gets the
					// full error body verbatim.
					resp.Body = io.NopCloser(bytes.NewReader(errBody))
					return resp, attemptCancel, nil
				}
				// Parked by the quota pause: book the absorbed attempt (with
				// the provider's Retry-After hint for the record) and loop.
				// Quota waits never burn the ordinary retry budget - only
				// transient attempts count against maxRetries.
				attemptCancel() // the in-cap body was drained by the peek
				retryAfter := s.absorbHTTPRetry(rec, resp, errBody, t.authHeader)
				rec.RetryAfterMs = int(retryAfter.Milliseconds())
				// A durable quota condition is an account state, not flow
				// control: RateLimited stays unset for the same reason the
				// unarmed path does.
				rec.ErrorType = ""
				rec.ErrorMsg = ""
				s.publishUpdate(rec)
				retried = true
				continue
			}
		}

		// Only retry transient conditions: 429 (rate limit) and any 5xx
		// (upstream/server error). 4xx client errors are final. When retries
		// are exhausted the last upstream response is surfaced verbatim, so
		// the client sees the real error - retries are transparent, failures
		// are not masked.
		retryable := isRetryable(resp.StatusCode)
		active := s.observeStormHTTP(ctx, permit, resp, retryable)
		if state := stormState(ctx); state != nil && state.response == permit {
			state.responseCtx = attemptCtx
		}
		if !retryable || ordinary >= maxRetries && !s.allowStormRetry(ctx, active) {
			// This is the attempt whose response reaches the client: TTFT is
			// measured from here, so a retried request shows the successful
			// attempt's responsiveness, not the accumulated retry delay.
			if attempt > 0 {
				rec.FinalAttemptAt = attemptStart
			}
			end(isRetryable(resp.StatusCode))
			if errBody != nil {
				if peekLeftOpen {
					// Oversized 429, budget exhausted: relay the COMPLETE body -
					// captured prefix + live remainder (byte-for-byte, never a
					// truncated prefix). Close() closes the original body.
					resp.Body = &prefixRemainderBody{prefix: errBody, rest: resp.Body}
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(errBody))
				}
			}
			return resp, attemptCancel, nil
		}

		// Read the failed attempt's error body (bounded) so the proxy logs the
		// full failure detail even though the client never sees it. The body is
		// then discarded - it is never forwarded. Best-effort: a read failure
		// just means less error detail for this absorbed attempt. (An in-cap
		// 429 body was already captured+closed by the durable-quota peek above;
		// an OVERSIZED 429's peek left the body open, so close it here - the
		// remainder is discarded with the rest of the attempt.)
		if errBody == nil {
			errBody, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
			resp.Body.Close()
		} else if peekLeftOpen {
			resp.Body.Close()
		}
		// The absorbed attempt's send is over (body drained): release its
		// per-send deadline before the next attempt mints a fresh one.
		attemptCancel()

		// Record the absorbed attempt so the dashboard/drawer show the errors
		// that preceded the final (e.g. 200) outcome.
		retryAfter := s.absorbHTTPRetry(rec, resp, errBody, t.authHeader)
		s.publishUpdate(rec)
		// 429/503 are genuine rate limits; other 5xx are transient upstream
		// errors. Record the distinction so the dashboard doesn't mislabel a
		// 502 as a rate limit.
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			rec.RateLimited = true
		}
		rec.ErrorType = ""
		rec.ErrorMsg = ""
		rec.RetryAfterMs = int(retryAfter.Milliseconds())

		ordinary++
		retried = true
		own = s.scheduler.Trip(groupKey, s.scheduler.BackoffFor(groupKey, retryAfter)) || own
	}
}

// isRetryable reports whether a status is a transient condition worth
// retrying transparently. Provider-agnostic: 429 (Too Many Requests) and any
// 5xx (upstream/server error: 500, 502, 503, 504, …) are retried; 4xx client
// errors and 2xx/3xx are final.
func isRetryable(code int) bool {
	return code == 429 || (code >= 500 && code <= 599)
}

// isRetryableTransportErr reports whether a client.Do transport error (no
// HTTP response received) is a transient failure worth retrying. The caller
// must have already ruled out caller-abort via ctx.Err() - this only classifies
// genuine upstream/network failures. Retryable: header/i/o timeouts, connection
// reset/refused, unexpected EOF, DNS timeout/temporary. Not retryable: DNS
// NXDOMAIN (host doesn't exist), TLS/cert errors, and other permanent failures.
func isRetryableTransportErr(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// Retry transient DNS (timeout/temporary-servfail); NXDOMAIN is permanent.
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	// Timeout-class net errors (e.g. "timeout awaiting response headers").
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Syscall-level dial/read/write failures (connection refused/reset).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Connection dropped mid-handshake.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// transportErrText renders a transport error for storage/display without the
// *url.Error wrapper, whose message embeds the full request URL - including
// any client query string, which may carry sensitive values. Only the
// underlying network error text is surfaced.
func transportErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// parseRetryAfter extracts a delay from provider retry hints. Checks
// Retry-After (seconds or HTTP-date), then OpenAI's x-ratelimit-reset-requests
// (e.g. "2s" or a duration string), then anthropic-ratelimit-reset-requests.
// Every path is bounded to [0, scheduler.MaxRetryHint]: the stored hint lands
// in the record's RetryAfterMs and feeds BackoffFor, whose pathological-hint
// clamp only catches positives - an overflow-wrapped negative or an
// implementation-defined non-finite float conversion must never get stored.
func parseRetryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			switch {
			case secs <= 0:
				return 0
			case secs > int(scheduler.MaxRetryHint/time.Second):
				return scheduler.MaxRetryHint
			default:
				// Within the ceiling the seconds product can no longer wrap.
				return time.Duration(secs) * time.Second
			}
		}
		if ts, err := http.ParseTime(v); err == nil {
			return scheduler.ClampRetryHint(time.Until(ts))
		}
	}
	for _, h := range []string{
		"X-Ratelimit-Reset-Requests",
		"X-Ratelimit-Reset-Tokens",
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-reset",
	} {
		if v := resp.Header.Get(h); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				return scheduler.ClampRetryHint(d)
			}
			// NaN and +/-Inf are malformed like any parse failure: fall
			// through to the next candidate. A finite value is range-checked
			// before the float-to-Duration conversion so it can never be
			// out of range.
			if secs, err := strconv.ParseFloat(v, 64); err == nil &&
				!math.IsNaN(secs) && !math.IsInf(secs, 0) {
				switch {
				case secs <= 0:
					return 0
				case secs > scheduler.MaxRetryHint.Seconds():
					return scheduler.MaxRetryHint
				default:
					return time.Duration(secs * float64(time.Second))
				}
			}
		}
	}
	return 0
}

// transformResponse relays a format-translated upstream response, translating
// it back to OpenAI Chat Completions shape on the fly. This handles only the
// STREAMING branch (the caller routes non-streaming responses through
// serveNonStreaming, which buffers the translation before the status line -
// transformation errors there carry a real error status instead of a committed
// 200). Errors during streaming translation are surfaced as an upstream error;
// the client has already received the status code by this point, so we
// best-effort the conversion.
func (s *Server) transformResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *metrics.Record, format string, stream bool) {
	if stream || isEventStream(resp.Header.Get("Content-Type")) {
		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
		// Stream translation: convert Anthropic SSE -> OpenAI SSE, while
		// feeding the transformed stream through the metrics analyzer.
		if format == "anthropic" {
			pr, pw := io.Pipe()
			costKeys := s.costKeysFor(rec.Provider)
			var sourceCost float64
			var sourceCostReported bool
			done := make(chan struct{})
			go func() {
				defer close(done)
				// Propagate a translation/scan failure to the reader as a pipe
				// error instead of faking a clean EOF (CloseWithError(nil) ==
				// Close). The goroutine must NOT touch rec - the handler
				// finalizes the record right here, so the reader side
				// (streamBodyTranslated) classifies the surfaced error
				// (translation failure vs client disconnect) itself.
				err := providerformat.StreamToOpenAI(pw, resp.Body, nil, func(payload []byte) {
					if cost, ok := metrics.ExtractCost(payload, costKeys); ok {
						sourceCost, sourceCostReported = cost, true
					}
				})
				pw.CloseWithError(err)
			}()
			// Re-run the SSE analyzer over the transformed stream.
			resetTurnMetrics(rec)
			s.streamBodyTranslated(ctx, w, pr, rec, flusher)
			// Close both ends of upstream work on early client failure, then
			// join before reading source accounting or finalizing the record.
			pr.Close()
			resp.Body.Close()
			<-done
			if sourceCostReported {
				rec.Cost = sourceCost
			}
			return
		}
		// Note: format "cursor" never reaches here - it is served by the
		// dedicated bidirectional handler (serveCursorBidi), bypassing
		// doWithRetry/transformResponse entirely.
		if _, err := io.Copy(disconnectWriter{w, rec}, resp.Body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return
	}
}

// streamBodyTranslated is like streamBody but for a pre-transformed SSE stream
// that does not need line splitting (the transformer emits complete frames).
// A scanner error here is the transformer's failure propagated through the
// pipe: a genuine translation/upstream error - unless the client disconnected,
// in which case the loop above already exited via a failed write to w.
func (s *Server) streamBodyTranslated(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record, flusher http.Flusher) {
	if flusher == nil {
		if _, err := io.Copy(disconnectWriter{w, rec}, body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, sseScannerBufInit), sseScannerBufMax)
	a := s.analyzerFor(rec)
	for scanner.Scan() {
		line := scanner.Bytes()
		// One Write per frame so a keepalive comment cannot splice onto a
		// data line between the payload and its trailing newline.
		frame := make([]byte, len(line)+1)
		copy(frame, line)
		frame[len(line)] = '\n'
		if _, err := w.Write(frame); err != nil {
			markClientGone(rec)
			return
		}
		a.Feed(line, time.Now())
		flusher.Flush()
	}
	// A scanner error is the transformer's failure arriving through the pipe
	// (CloseWithError): classify it here, in the handler, so the classification
	// stays race-free with the deferred record finalize. markStreamErr keeps a
	// client-disconnect-induced pipe close out of the error fields.
	if err := scanner.Err(); err != nil {
		markStreamErr(ctx, rec, err)
	}
	a.Fill(rec)
	if err := scanner.Err(); errors.Is(err, metrics.ErrMetricRange) {
		invalidateUsage(rec, err)
	}
}

// streamRescue classifies why streamBody wants the caller to transparently
// re-send the SAME request on the same committed SSE connection. Every class
// guarantees the client received no completion-contract bytes (no answer
// content, no tool call, no refusal), so the fresh stream's frames append
// cleanly; each class is booked as an absorbed retry attempt (Retries +
// Attempts, the dashboard's correction flag) and the re-send re-enters the
// ordinary admission path (operator holds, storm, pacing).
type streamRescue int

const (
	// rescueNone: the stream finished and its outcome is final - finalize.
	rescueNone streamRescue = iota
	// rescueEmptyTruncation: the stream hit clean EOF with no terminal
	// marker while the client had received no content-bearing frame at all
	// (only role/keepalive). The quality_retries budget owns this class.
	rescueEmptyTruncation
	// rescueReasoningTruncation: the stream died mid-thinking - clean EOF or
	// a genuine upstream read error, still no terminal marker - after the
	// client received reasoning deltas but nothing answer-shaped. The
	// thinking_retries budget owns this class.
	rescueReasoningTruncation
	// rescueReasoningStop: the stream terminated cleanly (stop, absent, or
	// interruption finish_reason) after reasoning-only output, and the
	// terminal region is still WITHHELD by the hold, so nothing signed the
	// void on the wire; the re-send replaces the withheld finish. The
	// thinking_retries budget owns this class (metrics.CodeReasoningOnly).
	rescueReasoningStop
	// rescueUpstreamError: the provider signaled failure in-band (an error
	// event streamed inside the 200) in a retryable server-availability
	// class, while nothing content-bearing had reached the client - the
	// error frame itself was dropped before the wire, so the re-send is
	// invisible. The quality_retries budget owns this class;
	// thinking_retries owns it when only reasoning was relayed. The
	// upstreamErrorRescue return carries the provider's error envelope for
	// the absorbed attempt's log entry.
	rescueUpstreamError
)

// upstreamErrorRescue carries the provider's in-band error envelope of an
// absorbed rescueUpstreamError attempt to the re-send loop's bookkeeping
// (the attempt log shows the provider's own type/code/message - what the
// error explorer groups it under). reasoning selects the budget exactly like
// the truncation twins: reasoning frames already relayed are auxiliary
// display text, so the thinking budget owns the rescue.
type upstreamErrorRescue struct {
	typ, code, msg string
	reasoning      bool
}

// streamBody forwards an SSE body, preserving bytes exactly, and flushes after
// each event boundary so TTFT == upstream TTFT. Alongside the copy it scans
// for token timestamps and the final usage chunk using an SSE analyzer.
//
// The relay withholds the stream's terminal region - everything from the
// first event carrying a non-null finish_reason or a [DONE] - until the
// stream ends, then releases it verbatim. When the analyzer classifies the
// finished stream as degenerate (metrics.ClassifyOutcome: truncated /
// tool_calls-without-calls / empty stop / reasoning-only stop), the held
// region is replaced with an in-band OpenAI error chunk the client treats as
// a retryable failure. The withholding is what makes that work: SDKs treat
// the finish chunk as the stream's end, so an error written after it would be
// ignored. The hold is bounded by terminalHoldMax (past it the stream
// commits to verbatim relay - no degenerate stream has a large "terminal"
// region), and every non-held byte is written through immediately, so the hot
// path only ever reserves one event's worth of bytes.
//
// Transparent re-sends: while a rescue budget remains, a truncation that
// relayed nothing content-bearing (allowTruncationRetry, the quality
// budget) or a mid-thinking death that relayed only reasoning
// (allowThinkingRescue, the thinking budget) returns the matching
// rescueReasoning* class instead of surfacing an error: the caller owns the
// re-send and the attempt bookkeeping. A reasoning-only clean stop is equally
// rescuable (rescueReasoningStop) because its terminal region never left the
// hold. A provider error event streamed in-band on the 200 is the same
// contract from the other side: while nothing content-bearing was relayed
// the frame is still unwritten, so a retryable server-availability class
// (retryableUpstreamErrorClass, the OpenAI retry semantics) is dropped and
// rescueUpstreamError re-sends - the multi-line flush discovery, a frame
// after opened content, a non-retryable class, or a spent budget falls back
// to verbatim relay exactly as before. The returned rescue is only ever
// reported in those no-contract-bytes states - never after answer content,
// tool calls, a refusal, an in-band error that reached the wire, or a read
// error caused by our own context (client gone or the per-send deadline).
// With the budget spent (rescueNone), the truncation or degenerate class
// surfaces in-band exactly as before: finish_reason length is a terminator
// and never rescuable. uerr receives the provider's envelope for a
// rescueUpstreamError return (the caller's attempt bookkeeping); it is
// untouched for every other outcome.
func (s *Server) streamBody(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record, allowTruncationRetry, allowThinkingRescue bool, uerr *upstreamErrorRescue) (rescue streamRescue) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		if _, err := io.Copy(disconnectWriter{w, rec}, body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return rescueNone
	}
	var (
		lineBuf bytes.Buffer // partial line + current chunk bytes not yet consumed
		out     bytes.Buffer // released bytes awaiting one client write
		hold    []byte       // bytes withheld from the terminal-candidate event onward
		holding bool
		buf     [4096]byte
		// holdAborted marks that the hold was abandoned (its bytes flushed on
		// the terminalHoldMax overflow): the stream then COMMITS to verbatim
		// relay - no degenerate classification may substitute anything (a
		// stream with a >64KiB terminal region is not one of the degenerate
		// classes, and the client already saw the finish-reason bytes).
		holdAborted bool
		// rescueRequested carries the rescue decision out of finishHold
		// (which returns only client-gone).
		rescueRequested streamRescue
	)
	a := s.analyzerFor(rec)
	writeOut := func() bool {
		if out.Len() == 0 {
			return true
		}
		o := out.Bytes()
		if _, werr := w.Write(o); werr != nil {
			markClientGone(rec)
			return false
		}
		if bytes.IndexByte(o, '\n') >= 0 {
			flusher.Flush()
		}
		out.Reset()
		return true
	}
	// finishHold resolves the withheld terminal region: classify the stream and
	// either release the held bytes verbatim or replace them with the in-band
	// degenerate error - never stacked on top of a provider-sent in-band error,
	// and never written to a client already gone. Returns false when the client
	// disappeared. Called the moment the outcome is decided: at a [DONE] /
	// Responses-terminal line (everything that can arrive has arrived - no need
	// to wait for EOF / a stalled upstream to close the connection), and at
	// clean EOF for streams that end without those markers. A rescuable
	// reasoning-only stop with budget remaining instead records
	// rescueRequested: the held region never reached the client, so the caller
	// re-sends and the fresh attempt's own terminal region takes its place.
	finishHold := func() bool {
		if !writeOut() {
			return false
		}
		if holdAborted {
			// Commit-to-verbatim mode: the overflow already released the
			// terminal bytes - nothing may be substituted, ever.
			flusher.Flush()
			return true
		}
		code := a.OutcomeCode()
		if code != "" && !a.HasInBandError() && !rec.ClientDisconnected {
			// The stop rescue requires the terminal region still WITHHELD:
			// once a finishHold call released it the outcome was already
			// signed on the wire, and no re-send may append behind it.
			// RetryableReasoningStop is the analyzer's wire-safety gate (no
			// refusal/function_call bytes, not a Responses-shaped feed).
			if holding && code == metrics.CodeReasoningOnly && allowThinkingRescue && a.RetryableReasoningStop() {
				hold = hold[:0]
				holding = false
				rescueRequested = rescueReasoningStop
				return true
			}
			emitDegenerateSSE(w, &a, time.Now(), code, rec)
		} else if len(hold) > 0 {
			if _, werr := w.Write(hold); werr != nil {
				markClientGone(rec)
				return false
			}
		}
		hold = hold[:0]
		holding = false
		flusher.Flush()
		return true
	}
	for {
		n, err := body.Read(buf[:])
		if n > 0 {
			now := time.Now()
			lineBuf.Write(buf[:n])
			for {
				b := lineBuf.Bytes()
				idx := bytes.IndexByte(b, '\n')
				if idx < 0 {
					break
				}
				line := b[:idx]
				prev := a.Terminated()
				a.Feed(line, now)
				switch {
				case holding:
					hold = append(hold, line...)
					hold = append(hold, '\n')
					// The terminal marker itself: everything that defines the
					// outcome has arrived - resolve the hold NOW instead of
					// waiting for EOF (a stalled upstream that keeps the
					// connection open after [DONE] must not starve the client's
					// completion forever).
					if sse.TerminalLine(line) {
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
					}
				case !prev && a.Terminated():
					// This line opened the terminal region: withhold it (and
					// everything after) until the outcome is decided.
					holding = true
					hold = append(hold, line...)
					hold = append(hold, '\n')
					if !writeOut() {
						return rescueNone
					}
					if sse.TerminalLine(line) {
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
					}
				default:
					// A provider error event that just parsed, still unwritten:
					// a retryable server-availability class with budget left is
					// dropped here (the client never sees this frame) and the
					// caller transparently re-sends - the OpenAI retry semantics
					// the provider asked for ("please retry"), applied at the
					// only wire state where re-sending cannot duplicate anything.
					// Anything else falls through to verbatim relay.
					if typ, code, msg, reasoning := a.RetryableUpstreamError(); typ != "" &&
						s.retryableUpstreamErrorClass(typ, code) && !rec.ClientDisconnected &&
						ctx.Err() == nil &&
						((reasoning && allowThinkingRescue) || (!reasoning && allowTruncationRetry)) {
						// Flush the complete lines the loop already fed to the
						// analyzer (role/keepalive/reasoning) so the wire matches
						// the record before the fresh attempt appends behind them.
						if !writeOut() {
							return rescueNone
						}
						// Close the client's pending SSE event: the dropped
						// frame was its first data line, so field lines of the
						// same event (an Anthropic-style `event: error`) may have
						// been relayed and must not type the fresh attempt's
						// first chunk. A blank line ends that event with no
						// data - spec parsers discard it - and is a no-op
						// between complete events.
						out.WriteByte('\n')
						if !writeOut() {
							return rescueNone
						}
						uerr.typ, uerr.code, uerr.msg, uerr.reasoning = typ, code, msg, reasoning
						return rescueUpstreamError
					}
					out.Write(line)
					out.WriteByte('\n')
				}
				lineBuf.Next(idx + 1)
			}
			if holding {
				// Retain a partial line for the next read: the hold owns only
				// complete lines already seen by the analyzer. TCP/read chunk
				// boundaries must never discard a terminal usage event.
				if len(hold)+lineBuf.Len() > terminalHoldMax {
					hold = append(hold, lineBuf.Bytes()...)
					lineBuf.Reset()
					if _, werr := w.Write(hold); werr != nil {
						markClientGone(rec)
						return rescueNone
					}
					flusher.Flush()
					hold = hold[:0]
					holding = false
					holdAborted = true
				}
			} else if lineBuf.Len() > sseScannerBufMax {
				// Safety guardrail: a newline-free (or one giant) stream from a
				// buggy or hostile upstream must not grow lineBuf without bound.
				// A newline-free chunk never enters the line loop above, so the
				// check must live here too; past the cap, feed the accumulated
				// bytes to the analyzer (it only byte-scans, so a fragment is
				// harmless), relay, and reset.
				a.Feed(lineBuf.Bytes(), now)
				out.Write(lineBuf.Bytes())
				lineBuf.Reset()
			}
			if !writeOut() {
				return rescueNone
			}
		}
		if err != nil {
			now := time.Now()
			// Flush any trailing partial line into the analyzer. A partial
			// line can never be a terminal marker, so it is never withheld.
			// Its bytes stay in `out` (unflushed) until the outcome below
			// decides whether to write them - a rescue drops them, so the
			// client never sees an unterminated line; the fresh attempt
			// opens a clean new event stream.
			if lineBuf.Len() > 0 {
				b := lineBuf.Bytes()
				a.Feed(b, now)
				if holding {
					hold = append(hold, b...)
				} else {
					out.Write(b)
				}
				lineBuf.Reset()
			}
			if err != io.EOF {
				// A mid-thinking death by read error (connection reset,
				// unexpected EOF from the upstream) is the same rescuable
				// truncation as clean EOF - but only when only reasoning was
				// relayed, and never when our own context caused the failure:
				// the client is gone (the outer context is canceled), or the
				// per-send deadline fired. The deadline lives on a CHILD
				// context (doWithRetry's withSendTimeout), so the outer ctx
				// stays clean when it kills the read - reject the deadline
				// sentinel itself (and any error wrapping it): a re-send
				// would mint a fresh deadline the client did not ask for.
				if allowThinkingRescue && ctx.Err() == nil &&
					!errors.Is(err, context.DeadlineExceeded) &&
					a.RetryableReasoningTruncation() && !rec.ClientDisconnected {
					return rescueReasoningTruncation
				}
				// A real read failure is the transport's own error - release
				// the held bytes verbatim (byte preservation) and never
				// substitute a synthetic outcome on top.
				markStreamErr(ctx, rec, err)
				if holding {
					if _, werr := w.Write(hold); werr != nil {
						markClientGone(rec)
						return rescueNone
					}
					hold = hold[:0]
				}
				if !writeOut() {
					return rescueNone
				}
				a.Fill(rec)
				return rescueNone
			}
			// Clean EOF: resolve the hold (truncation is decided here - a
			// stream without any terminal marker). A trailing partial line
			// released into out is written before any substituted chunk by
			// finishHold's writeOut-first ordering.
			//
			// Proxy-side truncation rescue: when a budget remains and the
			// stream died truncated with no completion-contract bytes on the
			// wire, hand the re-send to the caller instead of emitting. The
			// empty class (quality budget) leaves the client only
			// role/keepalive frames (a trailing partial line, if any, is
			// equally empty - content here would have marked the analyzer);
			// the thinking class (thinking budget) may additionally leave
			// reasoning deltas, which are auxiliary display text the fresh
			// stream appends after. The record is filled by the succeeding
			// attempt alone. No truncation can coexist with an open hold:
			// holding implies a seen terminator.
			if allowTruncationRetry && a.RetryableTruncation() && !rec.ClientDisconnected {
				return rescueEmptyTruncation
			}
			if allowThinkingRescue && a.RetryableReasoningTruncation() && !rec.ClientDisconnected {
				return rescueReasoningTruncation
			}
			if !finishHold() {
				return rescueNone
			}
			if rescueRequested != rescueNone {
				// A reasoning-only stream whose withheld terminal region was
				// resolved as a rescue at EOF (the finish chunk arrived but
				// no [DONE] ever did).
				return rescueRequested
			}
			a.Fill(rec)
			return rescueNone
		}
	}
}

// emitDegenerateSSE writes the in-band OpenAI error chunk + closing [DONE] in
// place of a withheld terminal region, and feeds both lines to the analyzer so
// the record carries the failure exactly like a provider-sent in-band error.
// The code comes from metrics.ClassifyOutcome (the single canonical owner of
// the degenerate classes + their messages). A failed write to the (already
// dying) client still marks client-disconnected on the record.
func emitDegenerateSSE(w http.ResponseWriter, a *sse.Analyzer, now time.Time, code string, rec *metrics.Record) {
	if b, err := sse.ErrorEnvelope("", sse.TypeUpstreamError, code, metrics.DegenerateMessage(code)); err == nil {
		line := sse.DataFrame(b)
		a.Feed(line[:len(line)-2], now)
		if _, werr := w.Write(line); werr != nil {
			markClientGone(rec)
			return
		}
	}
	a.Feed([]byte("data: [DONE]"), now)
	if _, werr := io.WriteString(w, sse.DoneFrame); werr != nil {
		markClientGone(rec)
	}
}
