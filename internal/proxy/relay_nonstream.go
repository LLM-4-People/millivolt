package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
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

// statusAllowsBody reports whether an HTTP status permits a response body.
// net/http refuses one for every 1xx, 204 and 304, so a translated non-200
// with one of those statuses cannot carry the protocol error envelope it
// promises; the caller fails closed with 502 instead.
func statusAllowsBody(status int) bool {
	if status >= 100 && status < 200 {
		return false
	}
	return status != http.StatusNoContent && status != http.StatusNotModified
}

// stampCommittedErrorStatus records the outcome of an in-band error written
// after a keepalive committed the success status line: the client saw 200, so
// the record reports 200. A 429 is also retained in the attempt history as one
// in-band entry, because HasRateLimit and its durable SQL mirror are defined by
// the upstream HTTP status and the committed 200 must not erase the distinct
// rate-limit count. It is health evidence, not a retry: Retries stays
// unchanged, retry disclosures exclude it, and the final error entry already
// owns the failure classification for every other status.
func stampCommittedErrorStatus(rec *metrics.Record, upstreamStatus int) {
	rec.StatusCode = http.StatusOK
	if upstreamStatus != http.StatusTooManyRequests {
		return
	}
	rec.Attempts = append(rec.Attempts, metrics.RetryAttempt{
		StatusCode: upstreamStatus,
		InBand:     true,
		At:         time.Now(),
	})
}

// relayNonCompletionStatus relays a non-completion status verbatim with the
// upstream headers. It is the one writer for 202/204/205 on the native and
// non-streaming translated paths, so neither translation nor completion
// classification can replace the upstream status. When a streaming pacer has
// not committed yet, applyStatus owns the commit so the body mode comes from
// Content-Type: a JSON acknowledgement stays quiet while an event-stream body
// may receive keepalives. A 202 arriving after the pacer already committed a
// keepalive 200 cannot be represented as the status, so it fails closed in
// band instead of corrupting the committed stream.
func (s *Server) relayNonCompletionStatus(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *metrics.Record, inbound ir.Protocol) {
	if p := pacerOf(w); p != nil {
		if p.applyStatus(resp.Header, resp.StatusCode) {
			s.relayNonCompletionBody(ctx, w, resp, rec)
			return
		}
		if p.statusCommitted() {
			typ, msg := upstreamErrorFields(rec, resp.StatusCode)
			// The pacer's kept-alive status line is the success 200; the
			// record must describe the status the client actually saw.
			stampCommittedErrorStatus(rec, resp.StatusCode)
			rec.ErrorType, rec.ErrorMsg = typ, msg
			if werr := writeProtocolStreamError(w, rec, inbound, rec.ID, rec.Model, typ, "", msg); werr != nil {
				markClientGone(rec)
			}
			return
		}
	}
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
