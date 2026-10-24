package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// readWholeDocument is the single server-side reader of the whole-document
// timing settings. Both the translated reply path and model discovery use it,
// so the fallback and ceiling cannot drift between callers.
func (s *Server) readWholeDocument(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	cfg := s.cfg()
	return readWholeDocument(ctx, body, limit, cfg.WholeDocumentReadTimeout, cfg.WholeDocumentReadMax)
}

// readWholeDocument reads at most limit bytes, failing closed when the body
// makes no completion before its deadline. The configured fallback applies
// when the caller has no deadline; a caller deadline is honored up to the
// configured ceiling so it cannot pin the response-buffer reservation
// indefinitely. Closing the body is what unblocks a transport Read; callers
// release their response-buffer reservation on return. The partial prefix is
// returned with the error so byte accounting stays exact.
func readWholeDocument(ctx context.Context, body io.ReadCloser, limit int64, fallback, ceiling time.Duration) ([]byte, error) {
	readCtx := ctx
	// bound is the guardrail this read applies, or 0 when a caller deadline
	// at or below the ceiling already supplies the shorter bound.
	bound := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		if ceiling > 0 && time.Until(deadline) > ceiling {
			bound = ceiling
		}
	} else if fallback > 0 {
		bound = fallback
	}
	if bound > 0 {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}
	stop := context.AfterFunc(readCtx, func() { body.Close() })
	defer stop()
	b, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		// Closing a wrapped transport body cancels its attempt context, so the
		// parent ctx may read as canceled after our own deadline fired. The
		// bounded child's deadline error distinguishes the guardrail from a
		// caller cancel: when bound is set it is always earlier than the
		// caller's deadline.
		if bound > 0 && readCtx.Err() == context.DeadlineExceeded {
			return b, fmt.Errorf("upstream document read exceeded %s: %w", bound, err)
		}
		return b, err
	}
	return b, nil
}

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

// translatedAnalyzer reads the normalized client wire produced by a
// translation. Provider usage/cost overrides address the source document, so
// they must not be applied to this wire.
func (s *Server) translatedAnalyzer() sse.Analyzer {
	var a sse.Analyzer
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
	if rec.ClientDisconnected {
		// The write that failed already classified this as the local client
		// leaving (a 499/plain disconnect). The same error must not be
		// re-stamped as an upstream read failure just because a write-progress
		// deadline, rather than the request context, surfaced it.
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
			if _, werr := writeCounted(w, rec, chunk[:n]); werr != nil {
				markClientGone(rec)
				// Break to the analysis of the captured prefix: the
				// upstream delivered those bytes, and the disconnect is
				// recorded without losing their payload.
				break
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

// isNonCompletionStatus reports an upstream status that acknowledges a
// request without carrying a completion: 202 is an asynchronous acceptance
// (its body, when present, belongs to the upstream protocol) and 204/205 are
// status-only. These never enter translation or completion classification.
func isNonCompletionStatus(status int) bool {
	return status == http.StatusAccepted || status == http.StatusNoContent || status == http.StatusResetContent
}

// relayNonCompletionStatus relays a non-completion status verbatim with the
// upstream headers. It is the one writer for 202/204/205 on the native and
// non-streaming translated paths, so neither translation nor completion
// classification can replace the upstream status.
func (s *Server) relayNonCompletionStatus(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *metrics.Record) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	s.relayNonCompletionBody(ctx, w, resp, rec)
}

// relayNonCompletionBody relays the body of a response whose status and
// headers are already committed.
func (s *Server) relayNonCompletionBody(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *metrics.Record) {
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusResetContent {
		return
	}
	if isEventStream(resp.Header.Get("Content-Type")) {
		flusher, _ := w.(http.Flusher)
		if cerr := copyFlushing(disconnectWriter{w, rec}, resp.Body, flusher); cerr != nil {
			markStreamErr(ctx, rec, cerr)
		}
		return
	}
	s.nonStreamBody(ctx, w, resp.Body, rec)
}

// nonStreamAnalysis is the proxy's single call point for the metrics body
// analysis. Production always uses metrics.AnalyzeNonStreamBody; the variable
// is a test seam so the request-path regression can count analyses (one per
// spooled body) and prove the quality gate and commit share it.
var nonStreamAnalysis = metrics.AnalyzeNonStreamBody

// analyzeNonStreamBytes runs the non-streaming metrics extraction over a
// captured body: one analysis decode, then the shared record apply owner.
// Used by the teeing relay (nonStreamBody), the translated-body path, and
// tests that exercise a raw body directly.
func analyzeNonStreamBytes(buf []byte, rec *metrics.Record, usageKeys map[string]string, costKeys []string, preview bool) {
	if len(buf) == 0 {
		return
	}
	applyNonStreamAnalysis(rec, nonStreamAnalysis(buf, usageKeys, costKeys, preview))
}

// applyNonStreamAnalysis is the single owner that writes one non-stream
// analysis onto a record. The quality-exhausted and commit paths both call it,
// so their field assignment order and gates cannot drift.
func applyNonStreamAnalysis(rec *metrics.Record, a metrics.NonStreamAnalysis) {
	// The historical extractor skipped every record-facing assignment when
	// the body's usage/model/id/tool detail did not type-decode, and it only
	// ran at all while the record had no output tokens yet.
	if a.RecordFieldsOK && rec.Usage.OutputTokens == 0 {
		// Usage fills only a record that has no canonical usage yet. A native
		// non-Chat body stamps its folded source usage before this analysis
		// (including a zero-output body, whose raw Chat view would otherwise
		// erase the cache folding), and an unrepresentable provider count is
		// explicitly invalid rather than repopulated field by field.
		if a.HasUsage && rec.Usage == (metrics.Usage{}) && !rec.UsageInvalid {
			rec.Usage = a.Usage
		}
		if a.Model != "" && rec.ProviderModel == "" {
			rec.ProviderModel = a.Model
		}
		if a.ID != "" && rec.ProviderRequestID == "" {
			rec.ProviderRequestID = a.ID
		}
		if a.FinishReason != "" {
			rec.FinishReason = a.FinishReason
		}
		if a.HadAnswerContent {
			rec.HadAnswerContent = true
			if a.Preview != "" && rec.ResponsePreview == "" {
				rec.ResponsePreview = a.Preview
			}
		}
		if a.ToolCalls > 0 {
			rec.ToolCalls = a.ToolCalls
			rec.ToolNames = a.ToolNames
		}
	}
	// Cost is read straight from the provider's response (dynamic), so it
	// reflects what the provider actually charged for this request.
	if a.HasCost {
		rec.Cost = a.Cost
	}
	// A 200 response can still carry a top-level {"error":{...}} envelope.
	// The analysis owns the decoy/null rules; an already-decided error is
	// never overwritten.
	if rec.ErrorType == "" && a.ErrorType != "" {
		rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = a.ErrorType, a.ErrorCode, a.ErrorMsg
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

// chatShapedResponse reports whether the UPSTREAM response follows the
// Chat/Responses completion contract. The known target protocols are
// authoritative; an arbitrary passthrough path falls back to the request-body
// heuristic. Anthropic Messages also uses a "messages" array, so the heuristic
// alone would put its legitimate empty and thinking-only turns into the
// OpenAI degenerate-substitution and rescue rules.
func chatShapedResponse(t *target, body []byte) bool {
	if t != nil {
		switch t.outbound {
		case ir.Chat, ir.Responses, ir.Cursor:
			return true
		case ir.Anthropic:
			return false
		}
		switch t.inbound {
		case ir.Chat, ir.Responses, ir.Cursor:
			return true
		case ir.Anthropic:
			return false
		}
	}
	return chatRequestBody(body)
}

// qualitySpoolReadCap is the most one spool attempt can hold: the kept body
// cap plus the single overflowing read chunk. The overflowing chunk is
// retained so the verbatim streaming passthrough loses no byte, so the
// reservation that bounds the spool must cover it too.
const qualitySpoolReadCap = qualitySpoolMax + nonStreamChunk

// qualitySpoolReserve is the respBudget reservation for one spool attempt:
// the declared Content-Length when the body fits the spool, otherwise the
// full bounded read cap. It sits next to spoolBody so the reservation always
// describes the buffer it protects.
func qualitySpoolReserve(contentLength int64) int64 {
	return responseReadReserve(contentLength, qualitySpoolReadCap)
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
// A failed client write marks the disconnect but does not skip the analysis:
// the upstream delivered and charged the body, so the record keeps its
// payload. The analysis is the caller's one decode of the spooled body, reused
// here instead of decoding again. The gates that choose this epilogue (the
// in-band error envelope passthrough and the healthy commit) stay at their
// call sites.
// applyTranslatedStructure stamps the record fields the Chat-oriented
// non-stream analyzer cannot see when the client-facing wire is Anthropic or
// Responses, translated or native: finish reason, answer presence, preview,
// tool calls and the canonical usage. Model and id stay with the shared
// non-stream analysis when it can read the body.
func applyTranslatedStructure(rec *metrics.Record, resp *ir.Response, preview bool) {
	if rec == nil || resp == nil {
		return
	}
	// The record keeps the canonical source reason (for example Anthropic's
	// pause_turn or refusal); only the Chat wire mapper approximates it.
	rec.FinishReason = string(resp.Finish)
	if resp.Usage != (ir.Usage{}) && !rec.UsageInvalid {
		// The canonical usage is already normalized (Anthropic cache counts
		// folded into input, total derived); the translated wire may omit the
		// total field entirely. An explicitly invalid usage stays zeroed.
		rec.Usage = metrics.Usage{
			InputTokens:     resp.Usage.InputTokens,
			OutputTokens:    resp.Usage.OutputTokens,
			TotalTokens:     resp.Usage.TotalTokens,
			CacheReadTokens: resp.Usage.CacheReadTokens,
			CacheWrite:      resp.Usage.CacheWriteTokens,
			ReasoningTokens: resp.Usage.ReasoningTokens,
		}
	}
	if resp.Model != "" && rec.ProviderModel == "" {
		rec.ProviderModel = resp.Model
	}
	if resp.ID != "" && rec.ProviderRequestID == "" {
		rec.ProviderRequestID = resp.ID
	}
	names := make([]string, 0, metrics.RetainedToolNamesMax)
	seen := make(map[string]struct{}, metrics.RetainedToolNamesMax)
	for _, b := range resp.Blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Text != "" {
				rec.HadAnswerContent = true
				if preview && rec.ResponsePreview == "" {
					rec.ResponsePreview = metrics.TruncatePreview(b.Text)
				}
			}
		case ir.BlockRefusal:
			// A refusal is answer-shaped output, but not answer content, on
			// every path (streamed delta and non-stream native alike). Only
			// the preview sees it; marking HadAnswerContent would keep a
			// refusal+tool turn out of the tool-only settlement.
			if b.Refusal != "" {
				if preview && rec.ResponsePreview == "" {
					rec.ResponsePreview = metrics.TruncatePreview(b.Refusal)
				}
			}
		case ir.BlockToolCall:
			rec.ToolCalls++
			name := ""
			if b.ToolCall != nil {
				name = b.ToolCall.Name
			}
			if name == "" || len(names) >= metrics.RetainedToolNamesMax {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		rec.ToolNames = names
	}
	// Provider-executed Responses call items have no block (the translated
	// wire drops them with a warning), but they are model tool invocations:
	// count them so the record's finish and tool-only token settlement agree
	// with the streaming analyzer.
	rec.ToolCalls += resp.ProviderToolCalls
	// A canonical failure carried by the provider stop reason (or a native
	// failed envelope) owns the record classification. The translated body
	// may still be a Chat success shape, so the record must read the canonical
	// response, not only the client wire.
	stampNativeResponseError(rec, resp.Error)
}

// applyNativeNonChatStructure stamps the canonical structure of a native
// Responses or Anthropic 2xx body. The Chat-oriented non-stream analysis
// cannot see those wires, so finish reason, tool calls, answer presence,
// preview and folded cache usage would stay empty. Best-effort by design: a
// body the codec cannot decode keeps the existing analysis and relays
// unchanged, because metrics must never block the client. Unrepresentable
// source usage is the exception: it is parsed independently so accounting is
// invalidated even when the envelope cannot be decoded into the canonical IR.
func (s *Server) applyNativeNonChatStructure(rec *metrics.Record, t *target, spooled []byte) {
	if rec == nil || t == nil {
		return
	}
	wire := t.wire()
	switch wire {
	case ir.Responses, ir.Anthropic:
	default:
		return
	}
	// The provider's usage object is authoritative and honors custom
	// usage_keys; the canonical decoder folds Anthropic's cache counts and
	// derives the total only for its own documented shape. Parse it before
	// the structural decode so an overflow cannot escape invalidation when
	// DecodeResponse rejects the same envelope.
	usage, ok, usageErr := providerformat.SourceUsage(wire, spooled, s.usageKeysFor(rec.Provider))
	resp, err := providerformat.DecodeResponse(wire, spooled)
	if err != nil || resp == nil {
		if usageErr != nil {
			invalidateUsage(rec, usageErr)
		}
		return
	}
	applyTranslatedStructure(rec, resp, s.cfg().CaptureBodyPreview)
	if usageErr != nil {
		// Invalidate after the provider error is stamped so its type/code
		// classification is preserved (invalidateUsage owns that rule).
		invalidateUsage(rec, usageErr)
		return
	}
	if ok && !rec.UsageInvalid {
		rec.Usage = metrics.Usage{
			InputTokens:     usage.InputTokens,
			OutputTokens:    usage.OutputTokens,
			TotalTokens:     usage.TotalTokens,
			CacheReadTokens: usage.CacheReadTokens,
			CacheWrite:      usage.CacheWriteTokens,
			ReasoningTokens: usage.ReasoningTokens,
		}
	}
}

// stampNativeResponseError records a terminal failure carried inside a 2xx
// response: a native Responses failed envelope or a canonical error synthesized
// by a translated source codec. A provider error object keeps its own
// classification; a provider code without a type keeps the canonical
// provider_error fallback; a bare failure (error null or absent) is an upstream
// lifecycle failure. Existing error detail is never overwritten, and the body
// and HTTP status stay untouched.
func stampNativeResponseError(rec *metrics.Record, e *ir.Error) {
	if rec == nil || e == nil {
		return
	}
	if rec.ErrorType == "" {
		switch {
		case e.Type != "":
			rec.ErrorType = e.Type
		case e.Code != "":
			rec.ErrorType = "provider_error"
		default:
			rec.ErrorType = sse.TypeUpstreamError
		}
	}
	if rec.ErrorCode == "" {
		rec.ErrorCode = e.Code
	}
	if rec.ErrorMsg == "" {
		rec.ErrorMsg = e.Message
	}
}

func (s *Server) commitSpooledBody(w http.ResponseWriter, resp *http.Response, spooled []byte, rec *metrics.Record, analysis metrics.NonStreamAnalysis) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, werr := writeCounted(w, rec, spooled); werr != nil {
		markClientGone(rec)
	}
	applyNonStreamAnalysis(rec, analysis)
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
				if rerr != nil {
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
			s.relayNonCompletionStatus(ctx, w, resp, rec)
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
// echoed model, rate-limit state, and the transport's auto-decompress fact -
// from the given response. ServeHTTP captures for the first response; the
// quality loops re-capture after every successful re-send so the record
// describes the attempt whose body the client actually received.
func captureUpstreamHeaders(resp *http.Response, rec *metrics.Record, authHeader string) {
	rec.ResponseHeaders = captureHeaders(resp.Header, authHeader)
	// The wire-compression fact lives only here: on auto-decompress the
	// transport deletes Content-Encoding and Content-Length from the header
	// map, so the captured headers cannot show it. resp.Uncompressed is set
	// by the transport exactly when it decoded a gzip body, and it is read
	// before the relay consumes the body.
	rec.UpstreamGzip = resp.Uncompressed
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

// copyFlushing relays a body with one Flush after each successful write, so a
// long-lived event-stream error body reaches the client as it arrives. The
// stack read buffer avoids io.Copy's per-call allocation.
func copyFlushing(w io.Writer, body io.Reader, flusher http.Flusher) error {
	var buf [32 * 1024]byte
	for {
		n, rerr := body.Read(buf[:])
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
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

// captureErrorFromResponse reads a bounded prefix of an error response body
// under the shared whole-document bound, then re-serves the captured prefix
// followed by the live remainder so the client still receives the full
// original body. The bound is what keeps a provider that holds or drips a
// small error body from delaying the handler past whole_document_read_timeout
// before the translated or buffered owner performs its own bounded read. A
// timed-out read closes the body; the captured prefix is still re-served and
// the owner's read then fails closed.
func (s *Server) captureErrorFromResponse(ctx context.Context, resp *http.Response, rec *metrics.Record) {
	b, err := s.readWholeDocument(ctx, resp.Body, maxErrBodyBytes)
	if err == nil && len(b) > 0 {
		rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = parseErrorBody(b)
	}
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
// (metrics.ParseErrorEnvelope + isNonRetryableQuotaErr). The caller arms the
// shared upstream idle guard on resp.Body at receipt (doWithRetry and
// openCursorHTTP), so a tarpit that sends headers and stalls cannot hold this
// read past upstream_stream_idle_timeout. An in-cap body is closed and reported
// together with its durable classification and the envelope's protocol tokens
// (type, code - the quota-pause reason label); an overflowed body (longer than
// maxErrBodyBytes) is deliberately left OPEN and unclassified
// (durable=false, leftOpen=true) - the classification stays bounded by the
// cap. Each transport then applies its own overflow policy at the call site:
// the generic relay re-serves prefix + live remainder verbatim, the cursor
// transport closes and re-wraps only the truncated prefix.
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
// budget. With no caller deadline the attempt still gets its own cancelable
// child context: the shared upstream body idle guard cancels that context to
// unblock a stalled transport read, so the no-timeout path must not degrade to
// a no-op cancel. Callers always defer the returned func.
func withSendTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// upstreamTiming decomposes ONE upstream send attempt's transport time via
// net/http/httptrace: the dial phase (ConnectStart to ConnectDone), the TLS
// handshake (TLSHandshakeStart to TLSHandshakeDone), the wait for the first
// byte of response headers (the attempt's own send start to
// GotFirstResponseByte), and the pool-hit fact (GotConn's
// GotConnInfo.Reused). start is the attempt's send start - attemptStart,
// stamped by admitSendAttempt after every admission gate - so
// upstream_ttfb_ms anchors exactly where FinalAttemptAt/TTFT anchor.
//
// The struct lives on the stack of ONE attempt and is wired into that
// attempt's own context: an abandoned attempt's trace dies with its context,
// so the record is only ever stamped by adopt at the points where a send
// driver returns the attempt whose response the client will receive - the
// same adoption FinalAttemptAt follows. Attempts absorbed inside the drivers
// (429/5xx/transport) never reach an adoption point, so their observations
// are dropped wholesale and the fields keep describing the last adopted
// attempt.
//
// Both transports in use fire these hooks: the stdlib net/http transport
// fires ConnectStart/ConnectDone (via the nettrace context value
// httptrace.WithClientTrace installs), the TLS handshake pair, GotConn with
// GotConnInfo.Reused and GotFirstResponseByte; x/net/http2 (the cursor
// bidi transport) reads ContextClientTrace and fires GotConn with its own
// reused flag and GotFirstResponseByte.
type upstreamTiming struct {
	// mu guards every field: httptrace fires the connect hooks on the
	// transport's dial goroutines, and a Happy-Eyeballs losing dial can stamp
	// ConnectDone while the request goroutine has already adopted the winning
	// connection's observations. Dial events are rare per attempt, so a small
	// mutex is the obviously-correct owner.
	mu        sync.Mutex
	start     time.Time // the attempt's send start (post-admission)
	connectS  time.Time // first ConnectStart (Happy Eyeballs can dial more than once)
	connectD  time.Time // last ConnectDone: the whole dial phase, fallbacks included
	tlsS      time.Time
	tlsD      time.Time
	firstByte time.Time
	reused    bool
}

// trace returns the httptrace.ClientTrace whose closures stamp this
// attempt's observations. Each hook is a plain timestamp: a hook that never
// fires (no dial on a pooled connection, no handshake on plaintext, no
// headers on a transport failure) simply leaves its zero value, and adopt
// renders that as "not observed".
func (u *upstreamTiming) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.connectS.IsZero() {
				u.connectS = time.Now()
			}
		},
		ConnectDone: func(network, addr string, err error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.connectD = time.Now()
		},
		TLSHandshakeStart: func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.tlsS.IsZero() {
				u.tlsS = time.Now()
			}
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.tlsD = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.reused = info.Reused
		},
		GotFirstResponseByte: func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.firstByte.IsZero() {
				u.firstByte = time.Now()
			}
		},
	}
}

// adopt stamps the record with this attempt's four observations. It runs
// only when the attempt's response is the one the relay commits to, so the
// persisted fields describe the final adopted attempt. Every observed
// positive interval rounds up to 1ms like FinalizeRecord's TTFT: 0 is
// reserved for "not observed", and an interval so short the clock resolved
// it to zero still means the hooks fired - observed, never absent.
func (u *upstreamTiming) adopt(rec *metrics.Record) {
	u.mu.Lock()
	defer u.mu.Unlock()
	rec.UpstreamConnectMs = observedMs(u.connectS, u.connectD)
	rec.UpstreamTLSMs = observedMs(u.tlsS, u.tlsD)
	rec.UpstreamTTFBMs = observedMs(u.start, u.firstByte)
	rec.UpstreamConnReused = u.reused
}

// observedMs renders one hook-pair interval as integer milliseconds: 0 only
// when either timestamp is missing (the hook never fired - "not observed");
// a stamped pair is an observation, so it never reports below 1ms.
func observedMs(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	if ms := end.Sub(start).Milliseconds(); ms > 0 {
		return ms
	}
	return 1
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
		// FinalAttemptAt anchors TTFT at the adopted attempt's send start.
		// Stamp every attempt, including the first: TTFT is always measured
		// from the upstream send, never from Start (which also covers
		// queueing and request parsing). Later attempts re-stamp their own
		// start, so the surviving value always describes the attempt whose
		// response is adopted. Only a request that never sent an upstream
		// attempt keeps the zero value (TTFT is then absent).
		rec.FinalAttemptAt = attemptStart
		// Per-send deadline: X-Proxy-Timeout-Ms bounds this ONE send (the
		// Do below and the returned body), anchored on the attemptStart the
		// admission helper stamped - a fresh budget per attempt, never eaten
		// by the waits above. The attempt's httptrace decomposition rides the
		// same context, so an abandoned attempt's trace dies with it (see
		// upstreamTiming).
		timing := upstreamTiming{start: attemptStart}
		attemptCtx, attemptCancel := withSendTimeout(httptrace.WithClientTrace(ctx, timing.trace()), t.timeout)
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

		// Guard the response body at receipt, before any consumer reads it.
		// The durable-quota peek, the absorbed-attempt error-body drain, and
		// the caller's capture/relay all read resp.Body; a provider that sends
		// headers and then goes silent must not pin the handler, the upstream
		// connection and the inflight reservation outside the shared idle
		// bound. Cancelling this attempt's context is what aborts a blocked
		// transport read; Close stops the timer and releases the context.
		resp.Body = s.wrapUpstreamBody(resp.Body, attemptCancel)

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
				if !s.settleQuotaFailure(permit, t.format, hooks.Client, hooks.Provider, metrics.NonRetryableQuotaClass(quotaTyp, quotaCode), parseRetryAfter(resp)) {
					end(false)
					// This response reaches the client: adopt this attempt's
					// transport timing (the failed attempts before it never
					// touched the record; FinalAttemptAt was already stamped at
					// this attempt's admission above).
					timing.adopt(rec)
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
			// measured from its send start (FinalAttemptAt was stamped at its
			// admission above), so a retried request shows the successful
			// attempt's responsiveness, not the accumulated retry delay. The
			// httptrace decomposition is adopted the same way: the record
			// keeps this attempt's dial/handshake/header-wait facts, and the
			// absorbed attempts' traces died with their contexts.
			timing.adopt(rec)
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
			// The body was armed with the shared idle guard when this attempt's
			// response arrived (and re-served prefixes above retain the live
			// remainder's guard), so the caller keeps a bounded read.
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

// translationErrorCode is the stable in-band code for a streaming translation
// failure after HTTP 200 was committed. The client receives the shared
// upstream_error envelope; the translator's own error text stays in the
// record, never on the wire.
const translationErrorCode = "translation_error"

// translationErrorMessage is the non-sensitive client-facing text for a
// failed streaming translation. It never echoes the translator's error or
// provider payloads.
const translationErrorMessage = "stream translation failed"

// translatedStreamWriter observes the translator's writes into the pipe
// without buffering the stream: it records whether the inbound protocol's
// terminal frame was written and whether an in-band error frame was already
// forwarded. transformResponse uses that state after the pipe fails so the
// client receives a terminal signal exactly once.
type translatedStreamWriter struct {
	dst        io.Writer
	terminal   func([]byte) bool
	done       bool
	errorFrame bool
}

func (t *translatedStreamWriter) Write(p []byte) (int, error) {
	n, err := t.dst.Write(p)
	if err != nil || n != len(p) {
		return n, err
	}
	// The translator emits each event as one complete Write through
	// sse.EmitFrame, so these comparisons observe frame boundaries without
	// retaining any bytes.
	if t.terminal != nil && t.terminal(p) {
		t.done = true
		return n, nil
	}
	if !t.errorFrame && frameIsError(p) {
		t.errorFrame = true
	}
	return n, nil
}

// closeFailedTranslation appends the terminal frames a failed translation
// pipe did not deliver. The status line and any translated content are
// already on the wire, so a protocol-native error frame (unless the
// translator already forwarded a provider error) followed by that protocol's
// terminal event is the only remaining signal. A failed write means the client is gone: markClientGone
// keeps the cancellation classification and no further writes are attempted.
func closeFailedTranslation(w http.ResponseWriter, flusher http.Flusher, rec *metrics.Record, inbound ir.Protocol, forwardedError bool) {
	if !forwardedError {
		if err := writeProtocolError(w, rec, inbound, rec.ID, sse.TypeUpstreamError, translationErrorCode, translationErrorMessage); err != nil {
			markClientGone(rec)
			return
		}
	}
	if err := writeProtocolTerminal(w, rec, inbound, rec.ID, rec.Model, sse.TypeUpstreamError, translationErrorCode, translationErrorMessage); err != nil {
		markClientGone(rec)
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// transformResponse relays a format-translated upstream response, encoding it
// in the client's protocol on the fly. This handles only the STREAMING branch (the caller routes non-streaming responses through
// serveNonStreaming, which buffers the translation before the status line -
// transformation errors there carry a real error status instead of a committed
// 200). A streaming translation failure after the 200 was committed is closed
// in-band with one protocol-native error frame and terminal event, so the
// client never sees an incomplete stream.
func (s *Server) transformResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *metrics.Record, t *target, stream bool) {
	if stream || isEventStream(resp.Header.Get("Content-Type")) {
		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
		// Stream translation: convert Anthropic SSE -> OpenAI SSE, while
		// feeding the transformed stream through the metrics analyzer.
		if t.translate {
			pr, pw := io.Pipe()
			tracked := &translatedStreamWriter{dst: pw, terminal: terminalFrameFor(t.inbound)}
			costKeys := s.costKeysFor(rec.Provider)
			// The source analyzer owns provider usage/cost keys; the
			// translated wire is normalized and must not be read with them.
			// Seed its wire so a zero-event source (empty body, comments or
			// pings) still classifies as the routed protocol's truncation
			// instead of returning an empty outcome and losing the canonical
			// upstream_error/truncated stamp.
			sourceAnalyzer := s.analyzerFor(rec)
			sourceAnalyzer.SetWire(t.wire())
			var sourceRec metrics.Record
			var sourceCost float64
			var sourceCostReported bool
			var translationErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				// Propagate a translation/scan failure to the reader as a pipe
				// error instead of faking a clean EOF (CloseWithError(nil) ==
				// Close). The goroutine must NOT touch rec - the handler
				// finalizes the record right here, so the reader side
				// (streamBodyTranslated) classifies the surfaced error
				// (translation failure vs client disconnect) itself. The
				// tracking writer records the translator's terminal frames so
				// the handler can close an incomplete stream exactly once.
				translationErr = providerformat.TranslateStreamBetween(t.inbound, t.outbound, tracked, resp.Body, nil, func(eventType string, payload []byte) {
					sourceAnalyzer.FeedEvent(eventType, payload, time.Now())
					if cost, ok := metrics.ExtractCost(payload, costKeys); ok {
						sourceCost, sourceCostReported = cost, true
					}
				}, providerformat.StreamOptions{IncludeUsage: t.streamIncludeUsage, FallbackModel: rec.Model})
				pw.CloseWithError(translationErr)
			}()
			// Re-run the SSE analyzer over the transformed stream.
			resetTurnMetrics(rec)
			s.streamBodyTranslated(ctx, w, pr, rec, flusher, t.inbound)
			// Close both ends of upstream work on early client failure, then
			// join before reading source accounting or finalizing the record.
			pr.Close()
			resp.Body.Close()
			<-done
			// The pipe delivered every frame the translator produced. A
			// failure without [DONE] would leave the client waiting forever,
			// so close the committed stream with the one terminal pair the
			// client understands. The synthetic frame is not fed back through
			// the analyzer: the record keeps markStreamErr's translation
			// classification rather than being relabeled a provider error.
			if translationErr != nil && !tracked.done && !rec.ClientDisconnected {
				closeFailedTranslation(w, flusher, rec, t.inbound, tracked.errorFrame)
			}
			// Source accounting is authoritative for translated streams. The
			// translated analyzer refused provider overrides; stamp what the
			// source actually reported after the client turn is finalized.
			fillRecord(&sourceAnalyzer, &sourceRec)
			// The source observer owns the provider error class. The
			// client-wire analyzer sees the target encoder's normalized error
			// (api_error, provider_error) and loses the source code, so the
			// first real source error wins over that view. A truncation is
			// synthesized by the translator inside TranslateStreamBetween and
			// therefore never reaches the source analyzer as an error event;
			// stamp the canonical truncation unless a genuine translator
			// failure already owns the record. Healthy streams are unchanged.
			if sourceRec.ErrorType != "" {
				rec.ErrorType = sourceRec.ErrorType
				rec.ErrorCode = sourceRec.ErrorCode
				rec.ErrorMsg = sourceRec.ErrorMsg
			} else if translationErr == nil && sourceAnalyzer.OutcomeCode() == metrics.CodeTruncated {
				rec.ErrorType = sse.TypeUpstreamError
				rec.ErrorCode = metrics.CodeTruncated
				rec.ErrorMsg = metrics.DegenerateMessage(metrics.CodeTruncated)
			}
			// Invalid accounting is sticky: the source observer may have
			// captured only the usage object that failed validation (or a
			// later partial one), and copying it would restore a refundable
			// count for a turn already marked unavailable.
			if sourceRec.UsageInvalid {
				invalidateUsage(rec, metrics.ErrMetricRange)
			}
			if sourceAnalyzer.UsageReported() && !rec.UsageInvalid {
				rec.Usage = sourceRec.Usage
				// Provider usage also owns the generation count: the
				// translated analyzer's chunk-derived fallback (tool-arg
				// chunks excluded) must not survive real accounting.
				rec.GenTokens = sourceRec.GenTokens
			}
			if sourceCostReported {
				rec.Cost = sourceCost
			} else if sourceRec.Cost != 0 {
				rec.Cost = sourceRec.Cost
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
func (s *Server) streamBodyTranslated(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record, flusher http.Flusher, inbound ir.Protocol) {
	if flusher == nil {
		if _, err := io.Copy(disconnectWriter{w, rec}, body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, sseScannerBufInit), translatedFrameBufMax)
	a := s.translatedAnalyzer()
	// Frames are accumulated until their blank-line terminator, then written
	// and flushed once: the transformer always emits "data: ...\n\n", so this
	// is one Write and one Flush per event instead of two of each, and the
	// buffer is reused across events with no per-line allocation.
	var frame []byte
	isTerminal := terminalFrameFor(inbound)
	terminal := false
	writeFrame := func() bool {
		if len(frame) == 0 {
			return true
		}
		// Capture the upstream-arrival timestamp before the client write:
		// a slow or backpressured client must not stretch TTFT or the
		// decode window. The direct relay path anchors its timestamps the
		// same way at the read site.
		now := time.Now()
		if _, err := writeCounted(w, rec, frame); err != nil {
			markClientGone(rec)
			// An in-band provider error that already reached the client is
			// the turn's outcome even though the following write failed; the
			// native streamBody twin fills it in clientGone. Without this the
			// 499 erases the provider classification.
			if a.HasInBandError() {
				fillRecord(&a, rec)
			}
			return false
		}
		// Feed the analyzer from the bytes actually delivered, in line order.
		rest := frame
		for len(rest) > 0 {
			line := rest
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				line, rest = rest[:i], rest[i+1:]
			} else {
				rest = nil
			}
			a.Feed(line, now)
		}
		terminal = isTerminal(frame)
		frame = frame[:0]
		flusher.Flush()
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			frame = append(frame, '\n')
			if !writeFrame() {
				return
			}
			if terminal {
				// The terminal [DONE] resolved the stream. Stop reading so
				// post-terminal debris cannot surface as a translation error
				// on a completed record; the caller closes the upstream body
				// and joins the translator.
				fillRecord(&a, rec)
				return
			}
			continue
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
	}
	if !writeFrame() {
		return
	}
	// A scanner error is the transformer's failure arriving through the pipe
	// (CloseWithError): classify it here, in the handler, so the classification
	// stays race-free with the deferred record finalize. markStreamErr keeps a
	// client-disconnect-induced pipe close out of the error fields.
	if err := scanner.Err(); err != nil {
		markStreamErr(ctx, rec, err)
	}
	fillRecord(&a, rec)
	if err := scanner.Err(); errors.Is(err, metrics.ErrMetricRange) {
		invalidateUsage(rec, err)
	}
}

// fillRecord is the one stream-side bridge from analyzer accounting to the
// record. The analyzer owns the source-usage validity signal because it sees
// sum overflows that a later chunk fallback would otherwise hide; invalidation
// runs after Fill so it clears any partial document Fill published.
func fillRecord(a *sse.Analyzer, rec *metrics.Record) {
	a.Fill(rec)
	if a.UsageInvalid() {
		invalidateUsage(rec, metrics.ErrMetricRange)
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

// endsWithUnframedDataLine reports whether b ends with a complete SSE data
// line and no blank separator after it. Framing the next data line as a new
// event prevents two payloads from concatenating into one event no spec
// parser can dispatch.
func endsWithUnframedDataLine(b []byte) bool {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return false
	}
	end := len(b) - 1
	start := bytes.LastIndexByte(b[:end], '\n') + 1
	line := bytes.TrimRight(b[start:end], "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	_, ok := sse.DataPayload(line)
	return ok
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
func (s *Server) streamBody(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record, allowTruncationRetry, allowThinkingRescue bool, uerr *upstreamErrorRescue, wire ir.Protocol) (rescue streamRescue) {
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
		// wireDataOpen records that the last bytes written to the client end
		// with a data line and no blank separator. It survives writeOut's
		// buffer reset, so a hold opened after an earlier flush still knows to
		// start a new event.
		wireDataOpen bool
		// eventName and eventBody buffer one Responses terminal-named event
		// until its blank-line boundary. The name alone cannot decide hold or
		// release: a multi-line data payload may only become a terminal
		// document once reassembled, and a name that contradicts that document
		// must be dropped together with it, never relayed ahead of a synthetic
		// failure. Non-terminal events keep the ordinary line path.
		eventName []byte
		eventBody []byte
		// pendingName is the terminal event name for an event that could not
		// be buffered as a unit: a late name after the event's data lines, or
		// a buffered event released verbatim at the hold bound. It classifies
		// the reassembled document at the real blank-line boundary.
		pendingName string
		// eventHasData records that the current SSE event already produced a
		// data: line, so a terminal name arriving for it belongs to the same
		// event and must keep its original position.
		eventHasData bool
		// deferredFinish keeps a terminal document's hold until its SSE
		// boundary: the event may still name itself after its data line, and
		// resolving at the data line would drop that late name.
		deferredFinish bool
		buf            [4096]byte
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
	a.SetWire(wire)
	// boundaryDrops reports whether finishHold will replace this attempt's
	// terminal region with the in-band degenerate error instead of writing the
	// held bytes. The tail-drop decision and finishHold share this predicate so
	// delivery and record accounting cannot drift.
	boundaryDrops := func(code string) bool {
		return code != "" && !a.HasInBandError() && !rec.ClientDisconnected &&
			a.ChatLike() && !holdAborted
	}
	// appendHeld frames one complete line into the withheld region. A second
	// data line in the same event would concatenate onto the first payload
	// (the SSE client joins data lines with a newline), so start a new event
	// before it. A field line stays with the data line it introduces.
	appendHeld := func(line []byte) {
		if _, isData := sse.DataPayload(line); isData && endsWithUnframedDataLine(hold) {
			hold = append(hold, '\n')
		}
		hold = append(hold, line...)
		hold = append(hold, '\n')
	}
	// lastLineIsData reports whether the next data line would join an open
	// event's existing data line, whether that line is still buffered in out
	// or was already flushed to the client.
	lastLineIsData := func() bool {
		if out.Len() > 0 {
			return endsWithUnframedDataLine(out.Bytes())
		}
		return wireDataOpen
	}
	// clientGone records a write-detected abort: the disconnect stamps
	// the 499, and the analyzer's in-band error - when one reached the
	// wire - still fills the record, so the abort counts like its 200
	// twin exactly as the read-detected paths do. Every caller returns
	// immediately, so the fill runs at most once; a stream without an
	// in-band error keeps the plain disconnect accounting.
	clientGone := func() {
		markClientGone(rec)
		if a.HasInBandError() {
			fillRecord(&a, rec)
		}
	}
	writeOut := func() bool {
		if out.Len() == 0 {
			return true
		}
		o := out.Bytes()
		if _, werr := writeCounted(w, rec, o); werr != nil {
			clientGone()
			return false
		}
		wireDataOpen = endsWithUnframedDataLine(o)
		if bytes.IndexByte(o, '\n') >= 0 {
			flusher.Flush()
		}
		out.Reset()
		return true
	}
	// writeRescueBoundary closes the client's pending SSE event before a rescue
	// return or the degenerate terminal error. Complete lines already in out are
	// written; the trailing partial line was never received by the client and is
	// dropped. One newline ends the open event (and is a harmless blank
	// separator between complete events), so the next data line cannot join an
	// existing payload. The withheld terminal region is discarded with the
	// attempt or replaced by the degenerate error.
	writeRescueBoundary := func() bool {
		o := out.Bytes()
		if n := bytes.LastIndexByte(o, '\n'); n >= 0 {
			o = o[:n+1]
		} else {
			o = nil
		}
		if len(o) > 0 {
			if _, werr := writeCounted(w, rec, o); werr != nil {
				clientGone()
				return false
			}
		}
		out.Reset()
		hold = hold[:0]
		holding = false
		if _, werr := writeCounted(w, rec, []byte("\n")); werr != nil {
			clientGone()
			return false
		}
		wireDataOpen = false
		flusher.Flush()
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
		if ctx.Err() == context.Canceled {
			// The request context only cancels when the LOCAL client is gone
			// (markStreamErr's classification) - and this resolution is
			// reachable with the client already gone: the upstream FIN can
			// beat the transport's own cancellation, handing the read loop a
			// CLEAN EOF for the abort. With out and hold drained no write
			// remains to observe the dead socket, and the only wire contact
			// left here is flusher.Flush, whose error net/http discards - so
			// the disconnect is stamped here (the write-detected abort's
			// exact accounting, in-band error kept) instead of finalizing the
			// abort away as a clean 200.
			clientGone()
			return false
		}
		code := a.OutcomeCode()
		// The stop rescue requires the terminal region still WITHHELD: once a
		// finishHold call released it the outcome was already signed on the
		// wire, and no re-send may append behind it. RetryableReasoningStop is
		// the analyzer's wire-safety gate (no refusal/function_call bytes, not
		// a Responses-shaped feed). Close any pending event before returning;
		// the held terminal bytes are discarded instead of written.
		if code != "" && !a.HasInBandError() && !rec.ClientDisconnected && a.ChatLike() &&
			holding && code == metrics.CodeReasoningOnly && allowThinkingRescue && a.RetryableReasoningStop() {
			if !writeRescueBoundary() {
				return false
			}
			rescueRequested = rescueReasoningStop
			return true
		}
		// A degenerate error is appended as a new SSE event. Close the
		// pending event first: a complete data line in out still needs its
		// blank separator, and a trailing partial line was never received and
		// must be dropped. Without this boundary the error becomes a second
		// data line in the open payload, or concatenates on the same line.
		degenerate := boundaryDrops(code)
		if degenerate {
			if !writeRescueBoundary() {
				return false
			}
		} else if !writeOut() {
			return false
		}
		if holdAborted {
			// Commit-to-verbatim mode: the overflow already released the
			// terminal bytes - nothing may be substituted, ever. Bytes held
			// after the overflow (a later marker opened a fresh hold) are
			// still part of the verbatim stream.
			if len(hold) > 0 {
				if _, werr := writeCounted(w, rec, hold); werr != nil {
					clientGone()
					return false
				}
				wireDataOpen = endsWithUnframedDataLine(hold)
				hold = hold[:0]
			}
			holding = false
			flusher.Flush()
			return true
		}
		if code != "" && !a.HasInBandError() && !rec.ClientDisconnected {
			if !a.ChatLike() {
				// A foreign wire such as Anthropic Messages has its own
				// terminal contract but no OpenAI degenerate substitution:
				// record the truncation without writing OpenAI frames into it.
				rec.ErrorType = sse.TypeUpstreamError
				rec.ErrorCode = code
				rec.ErrorMsg = metrics.DegenerateMessage(code)
			} else {
				if a.ResponsesFeed() {
					emitDegenerateResponses(w, &a, time.Now(), code, rec)
				} else {
					emitDegenerateSSE(w, &a, time.Now(), code, rec)
				}
			}
		} else if len(hold) > 0 {
			// A released terminal region is a complete SSE event: close the
			// final line and add the blank boundary a spec parser needs at
			// EOF. A held [DONE] or full terminal document already ends with
			// a blank separator and is unchanged.
			if !bytes.HasSuffix(hold, []byte("\n\n")) {
				if !bytes.HasSuffix(hold, []byte("\n")) {
					hold = append(hold, '\n')
				}
				hold = append(hold, '\n')
			}
			if _, werr := writeCounted(w, rec, hold); werr != nil {
				clientGone()
				return false
			}
			wireDataOpen = endsWithUnframedDataLine(hold)
		}
		hold = hold[:0]
		holding = false
		flusher.Flush()
		return true
	}
	// processEventBuffer resolves one complete terminal-named event. The SSE
	// name is kept only when it agrees with the document (or the document has
	// no type and the name is its only classifier); a contradicting name is
	// dropped so the client never receives an incomplete label over completed
	// data. A terminal document is held and resolved atomically with its
	// complete bytes. Returns true when the caller must leave streamBody;
	// rescueRequested then carries the re-send decision.
	processEventBuffer := func(now time.Time) bool {
		name := string(sse.EventName(eventName))
		data := sse.EventData(eventBody)
		typ, keepName := sse.ResolveResponsesTerminal(name, data)
		if typ != "" {
			a.FeedEvent(typ, data, now)
			event := make([]byte, 0, len(eventName)+len(eventBody))
			if keepName {
				event = append(event, eventName...)
			}
			event = append(event, eventBody...)
			holding = true
			hold = append(hold, event...)
			eventName = eventName[:0]
			eventBody = eventBody[:0]
			if !writeOut() {
				return true
			}
			if !finishHold() {
				return true
			}
			if rescueRequested != rescueNone {
				return true
			}
			fillRecord(&a, rec)
			return true
		}
		// The name announced a terminal event but the document is not one:
		// drop the name and relay the data as ordinary bytes. The analyzer
		// still sees the complete event exactly once.
		if len(data) > 0 {
			a.FeedEvent("", data, now)
		}
		out.Write(eventBody)
		eventName = eventName[:0]
		eventBody = eventBody[:0]
		return false
	}
	// abandonEventBuffer releases a buffered terminal-named event verbatim
	// when it outgrows the hold bound. Nothing may be substituted after that
	// commit, exactly like a hold overflow. The buffered prefix stays one
	// event for the analyzer: its reassembled data is preserved for the real
	// blank-line boundary, and the event name is remembered so a type-less
	// document can still be classified there. No artificial blank line is
	// fed at the overflow point - that would split the document mid-event.
	//
	// Known malformed-input residual (F5-7): after this commit, later bytes
	// of the same malformed event continue through the ordinary verbatim
	// relay. A delta, [DONE] or response.failed that follows an over-bound
	// terminal is therefore relayed after the completion, while the record
	// keeps the finish the terminal already signed. The normal path stops
	// reading post-terminal debris, but here the terminal was never held as
	// a unit and the relay has committed to byte preservation. Stopping or
	// withholding the tail would require rejecting bytes the client's SSE
	// parser may still accept, so this residual is documented rather than
	// changed.
	abandonEventBuffer := func(now time.Time) {
		raw := make([]byte, 0, len(eventName)+len(eventBody))
		raw = append(raw, eventName...)
		raw = append(raw, eventBody...)
		out.Write(raw)
		pendingName = string(sse.EventName(eventName))
		eventHasData = true
		a.FeedBufferedEvent(raw, now)
		eventName = eventName[:0]
		eventBody = eventBody[:0]
		holdAborted = true
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
				name := sse.EventName(line)
				terminalName := name != "" && sse.ResponsesTerminalEventName(name)
				// A terminal name that follows this event's data lines belongs
				// to the same event. Relay it in its original position and
				// classify the reassembled document at the blank boundary.
				if terminalName && len(eventName) == 0 && (holding || eventHasData) {
					pendingName = name
				}
				// Buffer a terminal-named event whole when its name opens the
				// event. Its document may only become parseable after several
				// data: lines, and its name must be kept or dropped together
				// with the document.
				if terminalName && len(eventName) == 0 && !holding && !eventHasData {
					eventName = append(eventName[:0], line...)
					eventName = append(eventName, '\n')
					lineBuf.Next(idx + 1)
					continue
				}
				if len(eventName) > 0 {
					if len(eventName)+len(eventBody)+len(line)+2 > terminalHoldMax {
						// Past the bound, commit to verbatim relay: feed and
						// release every buffered line, and never substitute.
						eventBody = append(eventBody, line...)
						eventBody = append(eventBody, '\n')
						abandonEventBuffer(now)
						lineBuf.Next(idx + 1)
						continue
					}
					eventBody = append(eventBody, line...)
					eventBody = append(eventBody, '\n')
					lineBuf.Next(idx + 1)
					if len(bytes.TrimSpace(line)) != 0 {
						continue
					}
					if processEventBuffer(now) {
						return rescueRequested
					}
					eventHasData = false
					if !writeOut() {
						return rescueNone
					}
					continue
				}
				blank := len(bytes.TrimSpace(line)) == 0
				prev := a.Terminated()
				// A [DONE] on a Responses feed is not its terminal marker, but
				// the line must still be withheld: a zero-event Responses body
				// replaces it with one response.failed at the end. Other wires
				// relay it as the foreign line it is.
				heldDone := sse.DoneLine(line) && a.ResponsesFeed()
				if blank {
					eventHasData = false
					if pendingName != "" {
						name := pendingName
						pendingName = ""
						a.FlushEventWithName(name, now)
					} else {
						a.Feed(line, now)
					}
					if !prev && a.Terminated() && !holding {
						// The boundary completed a terminal document that was
						// never buffered as a unit (multi-line data or a late
						// event name). Relay the boundary, resolve the record,
						// and stop reading post-terminal debris.
						out.Write(line)
						out.WriteByte('\n')
						if !writeOut() {
							return rescueNone
						}
						fillRecord(&a, rec)
						return rescueNone
					}
				} else {
					if _, isData := sse.DataPayload(line); isData {
						eventHasData = true
					}
					a.Feed(line, now)
				}
				switch {
				case holding:
					appendHeld(line)
					if sse.TerminalLine(line) && (a.DoneTerminates() || !sse.DoneLine(line)) {
						deferredFinish = true
					}
					if sse.DoneLine(line) && a.DoneTerminates() {
						// [DONE] ends the stream at the line itself; emit the
						// blank line its SSE block would have carried.
						hold = append(hold, '\n')
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
						fillRecord(&a, rec)
						return rescueNone
					}
					// A Responses/Anthropic terminal document stays held until
					// its blank-line boundary so a late event name can follow
					// in its original position. A standalone error event is a
					// terminal statement of its own and must not wait for EOF:
					// a provider may keep the connection open after it. This
					// release is scoped to an observed in-band error, so a
					// Chat finish-reason hold still waits for usage/[DONE].
					if blank && (deferredFinish || (!prev && a.Terminated()) || a.HasInBandError()) {
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
						fillRecord(&a, rec)
						return rescueNone
					}
				case !prev && (a.Terminated() || heldDone) && !blank:
					// This line opened the terminal region: withhold it (and
					// everything after) until the outcome is decided. A data
					// line already flushed in out is its own event; separate
					// it before the held event starts.
					holding = true
					if lastLineIsData() {
						hold = append(hold, '\n')
					}
					appendHeld(line)
					if sse.DoneLine(line) && a.DoneTerminates() {
						hold = append(hold, '\n')
					}
					if sse.TerminalLine(line) && (a.DoneTerminates() || !sse.DoneLine(line)) {
						deferredFinish = true
					}
					if !writeOut() {
						return rescueNone
					}
					if sse.DoneLine(line) && a.DoneTerminates() {
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
						fillRecord(&a, rec)
						return rescueNone
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
						if !writeRescueBoundary() {
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
					if _, werr := writeCounted(w, rec, hold); werr != nil {
						clientGone()
						return rescueNone
					}
					wireDataOpen = endsWithUnframedDataLine(hold)
					flusher.Flush()
					hold = hold[:0]
					holding = false
					holdAborted = true
				}
			} else if len(eventName) > 0 && lineBuf.Len() > sseScannerBufMax {
				// A newline-free giant inside a buffered terminal event:
				// commit its bytes verbatim, exactly like a hold overflow.
				eventBody = append(eventBody, lineBuf.Bytes()...)
				abandonEventBuffer(now)
				lineBuf.Reset()
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
			// A terminal-named event that ended at EOF without its blank-line
			// boundary still has all of its data lines; resolve it now rather
			// than leaving a stale name or dropping an authoritative terminal
			// document behind a synthetic failure.
			if len(eventName) > 0 {
				if lineBuf.Len() > 0 {
					eventBody = append(eventBody, lineBuf.Bytes()...)
					lineBuf.Reset()
				}
				if processEventBuffer(now) {
					return rescueRequested
				}
				if !writeOut() {
					return rescueNone
				}
			}
			// Save any trailing partial line without feeding it to the
			// analyzer yet. The line is never written by a rescue, so its
			// content, tool call and preview state must not veto the pre-content
			// rescues below. An unterminated final line can still carry a
			// terminal marker ([DONE] or a non-null finish_reason); the
			// analyzer's tail classifier owns that grammar. releaseTail feeds
			// and releases it only on the paths that actually deliver it.
			var tail []byte
			if lineBuf.Len() > 0 {
				tail = append(tail, lineBuf.Bytes()...)
				lineBuf.Reset()
			}
			releaseTail := func() {
				if len(tail) == 0 {
					return
				}
				a.Feed(tail, now)
				if holding {
					// Close any pending event, then write the tail as its own
					// event with a blank boundary. Without this framing a
					// finish line and a content tail would form one SSE payload
					// with two data lines, which no spec parser can dispatch at
					// EOF.
					if len(hold) > 0 && !bytes.HasSuffix(hold, []byte("\n\n")) {
						if !bytes.HasSuffix(hold, []byte("\n")) {
							hold = append(hold, '\n')
						}
						hold = append(hold, '\n')
					}
					hold = append(hold, tail...)
					hold = append(hold, '\n', '\n')
				} else {
					if _, isData := sse.DataPayload(tail); isData && lastLineIsData() {
						out.WriteByte('\n')
					}
					out.Write(tail)
				}
				tail = nil
			}
			// An event released at the hold bound may also end at EOF without
			// its blank-line boundary. The buffered prefix and every later
			// data line are already reassembled; apply the remembered event
			// name now so a type-less terminal document still classifies.
			if pendingName != "" {
				name := pendingName
				pendingName = ""
				a.FlushEventWithName(name, now)
			}
			// An unterminated final line can itself carry the terminal marker.
			// Decide before any pre-tail rescue so a complete answer is not
			// re-sent or discarded. The classifier never commits analyzer
			// state; the feed below is the only commit.
			tailTerminates := a.TailTerminates(tail)
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
				if allowThinkingRescue && ctx.Err() == nil && !tailTerminates &&
					!errors.Is(err, context.DeadlineExceeded) &&
					a.RetryableReasoningTruncation() && !rec.ClientDisconnected {
					if !writeRescueBoundary() {
						return rescueNone
					}
					return rescueReasoningTruncation
				}
				// A real read failure is the transport's own error - release
				// the held bytes verbatim (byte preservation) and never
				// substitute a synthetic outcome on top.
				markStreamErr(ctx, rec, err)
				releaseTail()
				if holding {
					if _, werr := writeCounted(w, rec, hold); werr != nil {
						clientGone()
						return rescueNone
					}
					wireDataOpen = endsWithUnframedDataLine(hold)
					hold = hold[:0]
				}
				if !writeOut() {
					return rescueNone
				}
				fillRecord(&a, rec)
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
			// holding implies a seen terminator. A committed verbatim relay
			// (holdAborted) must never be re-sent: the client already saw the
			// oversized bytes.
			if !tailTerminates && allowTruncationRetry && ctx.Err() == nil && !holdAborted && a.RetryableTruncation() && !rec.ClientDisconnected {
				if !writeRescueBoundary() {
					return rescueNone
				}
				return rescueEmptyTruncation
			}
			if !tailTerminates && allowThinkingRescue && ctx.Err() == nil && !holdAborted && a.RetryableReasoningTruncation() && !rec.ClientDisconnected {
				if !writeRescueBoundary() {
					return rescueNone
				}
				return rescueReasoningTruncation
			}
			// No rescue owns this attempt. finishHold either delivers the
			// trailing partial line through its verbatim branch or drops it at
			// the degenerate boundary. Decide that before feeding it: content
			// the boundary drops was never delivered, so it must not set the
			// answer content or first-answer time on the record. The
			// truncation class is skipped only when the analyzer's own tail
			// classifier says the line carries no completion terminal marker:
			// a final Chat [DONE] or non-null finish_reason is fed rather than
			// dropped. A fed tail is delivered only when it supplies a
			// complete answer or tool call; an empty or reasoning-only
			// completion resolves through the degenerate boundary, which
			// withholds or replaces the terminal bytes. The other degenerate
			// classes also run finishHold after the feed because the tail can
			// supply the missing output.
			if tailTerminates {
				// Feed the terminal tail into the withheld region and let
				// finishHold decide healthy, degenerate or reasoning-stop. It
				// is released as its own framed SSE event.
				holding = true
				releaseTail()
			} else {
				preTailCode := a.OutcomeCode()
				tailDropped := preTailCode == metrics.CodeTruncated && boundaryDrops(preTailCode)
				if !tailDropped {
					releaseTail()
				}
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
			fillRecord(&a, rec)
			return rescueNone
		}
	}
}

// emitDegenerateResponses replaces a withheld Responses terminal region with a
// Responses-shaped response.failed event. A Responses client has no [DONE]
// sentinel and no choices, so the Chat substitution would leave it without a
// terminal signal.
func emitDegenerateResponses(w http.ResponseWriter, a *sse.Analyzer, now time.Time, code string, rec *metrics.Record) {
	id := a.ResponseID()
	if id == "" {
		id = rec.ID
	}
	msg := metrics.DegenerateMessage(code)
	// The substitution is proxy-minted, not a provider statement: stamp the
	// local failure before feeding the synthetic frames so the standalone
	// error classifier cannot attribute it to the provider.
	a.SetLocalFailure(code, msg)
	// The free-form error event carries the specific internal code; the
	// response.failed envelope carries the closed ResponseErrorCode enum.
	// Feed the error event first so the analyzer's first-real-error rule keeps
	// the specific code on the record; the enum value is wire-only.
	if payload, frame, err := providerformat.ResponsesSyntheticEvent(
		"error", 0, id, rec.Model, code, msg); err == nil {
		a.Feed(append([]byte("data: "), payload...), now)
		a.Feed(nil, now)
		if _, werr := writeCounted(w, rec, frame); werr != nil {
			markClientGone(rec)
			return
		}
	}
	payload, frame, err := providerformat.ResponsesSyntheticEvent(
		"response.failed", 1, id, rec.Model, code, msg)
	if err != nil {
		return
	}
	a.Feed(append([]byte("data: "), payload...), now)
	if _, werr := writeCounted(w, rec, frame); werr != nil {
		markClientGone(rec)
		return
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
		// The substituted frames replace a terminal region the client never
		// saw, so writeCounted books the bytes the client received, never the
		// withheld originals.
		if _, werr := writeCounted(w, rec, line); werr != nil {
			markClientGone(rec)
			return
		}
	}
	a.Feed([]byte("data: [DONE]"), now)
	if _, werr := writeCounted(w, rec, []byte(sse.DoneFrame)); werr != nil {
		markClientGone(rec)
	}
}
