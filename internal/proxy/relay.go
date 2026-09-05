package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// markStreamErr classifies an error from an upstream body copy/scan loop: when
// the LOCAL client disconnected (its request context canceled) the loop failed
// because the proxy's relay to the client died - that is the client's own
// cancellation, recorded as a client disconnect (the 499/cancel bucket when no
// error outcome was decided yet; a decided error status stays, per
// markClientGone) and NOT as an error. Anything else is genuine upstream
// trouble (or a translation failure) and gets the given error type stamped
// (the already-committed upstream status stays on the record). The single
// choke point for every relay loop; Cursor paths keep their own documented
// exception (TurnResult.ClientAbort in cursor_bidi.go - the upstream DID
// answer, so the 200 stays).
func markStreamErr(ctx context.Context, rec *metrics.Record, err error, errType string) {
	if ctx.Err() == context.Canceled {
		markClientGone(rec)
		return
	}
	rec.ErrorType = errType
	rec.ErrorMsg = err.Error()
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
				markStreamErr(ctx, rec, err, "stream_read_error")
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

// serveNonStreaming relays a non-streaming response with quality handling. A
// 200 whose body classifies degenerate (metrics.ClassifyNonStreamBody -
// same predicate as the streaming paths) triggers a transparent retry BEFORE
// the status line is written (bounded by quality_retries); past the budget the
// body is surfaced as an HTTP 502 with an OpenAI error envelope - a failure,
// never a silently empty success (the cursor empty_turn precedent, and
// 502/5xx is retryable in common API clients). In-band error
// envelopes on a 200 are NOT retried: they are the provider's own failure
// statement and are relayed verbatim. Translated (anthropic) non-streaming
// bodies are never quality-classified: Anthropic documents legitimate empty
// end_turn answers and refusals that must not be re-run (and re-running a
// server_tool_use turn would double-execute tools upstream).
func (s *Server) serveNonStreaming(ctx context.Context, w http.ResponseWriter, resp *http.Response, r *http.Request, t *target, key string, body []byte, rec *metrics.Record, groupKey string, hooks scheduler.WaiterHooks) {
	maxQuality := s.cfg().QualityRetries
	for qualityAttempt := 0; ; qualityAttempt++ {
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
					markStreamErr(ctx, rec, cerr, "stream_read_error")
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
			full, rerr := io.ReadAll(resp.Body)
			if rerr != nil {
				markStreamErr(ctx, rec, rerr, "stream_read_error")
				if rec.ClientDisconnected {
					return
				}
				rec.StatusCode = http.StatusBadGateway
				http.Error(w, errJSON("api_error", "upstream error: "+transportErrText(rerr)), http.StatusBadGateway)
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
				http.Error(w, errJSON("api_error", "translate upstream response: "+terr.Error()), http.StatusBadGateway)
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
			markStreamErr(ctx, rec, spoolErr, "stream_read_error")
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if _, werr := w.Write(spooled); werr != nil {
				markClientGone(rec)
			}
			resp.Body.Close()
			return
		}

		if metrics.HasErrorKey(spooled) {
			// The provider failed in-band on a 200: its own statement, never
			// quality-retried - relay verbatim and let the record carry it.
			// The cheap byte-scan gate is followed by the canonical decode: a
			// decoy key (null/empty/numeric error value - ParseErrorEnvelope
			// returns "" for those, the exact semantics the SSE analyzer uses)
			// is NOT a failure, so the body falls through to degenerate
			// classification instead of riding this verbatim path as a
			// silently-empty success.
			if typ, _, _ := metrics.ParseErrorEnvelope(spooled); typ != "" {
				copyResponseHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				if _, werr := w.Write(spooled); werr != nil {
					markClientGone(rec)
					return
				}
				analyzeNonStreamBytes(spooled, rec, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)
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
			if qualityAttempt < maxQuality {
				// Absorb the degenerate attempt and retry the SAME request -
				// nothing was written to the client, so this is a safe
				// pre-write retry like the 429/5xx attempts.
				rec.Attempts = append(rec.Attempts, metrics.RetryAttempt{
					StatusCode: resp.StatusCode,
					ErrorType:  code,
					ErrorMsg:   metrics.DegenerateMessage(code),
					At:         time.Now(),
				})
				rec.Retries++
				s.publishUpdate(rec)
				resp.Body.Close()
				// The re-send is a retry: its opening WaitSend honors operator
				// holds ("a retry is a new send and waits").
				next, nextCancel, err := s.doWithRetry(ctx, groupKey, r, t, key, body, rec, hooks, true)
				if err != nil {
					if r.Context().Err() == context.Canceled {
						markClientGone(rec)
						return
					}
					rec.StatusCode = http.StatusBadGateway
					rec.ErrorType = "upstream_unreachable"
					rec.ErrorMsg = transportErrText(err)
					http.Error(w, errJSON("api_error", "upstream error: "+transportErrText(err)), http.StatusBadGateway)
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
				rec.FinalAttemptAt = time.Now()
				continue
			}
			// Quality budget exhausted (or zero): surface a real failure.
			analyzeNonStreamBytes(spooled, rec, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)
			rec.ErrorType = code
			rec.ErrorCode = code
			rec.ErrorMsg = metrics.DegenerateMessage(code)
			rec.StatusCode = http.StatusBadGateway
			http.Error(w, errJSONCode("api_error", metrics.DegenerateMessage(code), code), http.StatusBadGateway)
			return
		}

		// Healthy: commit the spooled body verbatim.
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(spooled); werr != nil {
			markClientGone(rec)
			return
		}
		analyzeNonStreamBytes(spooled, rec, s.usageKeysFor(rec.Provider), s.costKeysFor(rec.Provider), s.cfg().CaptureBodyPreview)
		return
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
		// never sets FirstAnswerAt, so flag content directly. Content is either a
		// string or an array of parts - any non-null, non-empty value counts.
		contentStr := messageContentText(raw.Choices[0].Message.Content)
		if contentStr != "" {
			rec.HadAnswerContent = true
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
		// Response preview: a bounded rune-safe prefix of the text content,
		// stored only when capture_body_preview is enabled (off by default).
		if preview {
			if contentStr != "" && rec.ResponsePreview == "" {
				rec.ResponsePreview = metrics.TruncatePreview(contentStr)
			}
		}
	}
}

// messageContentText extracts text content from a chat message's content field,
// which OpenAI serializes either as a plain string or as an array of parts
// ([{"type":"text","text":"..."}]). Returns "" for null/empty. Used only to
// detect content presence and (when preview is enabled) build a bounded preview
// - never stores full content.
func messageContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Array-of-parts shape: concatenate the text parts.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
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

// doWithRetry executes the upstream request, transparently absorbing transient
// failures: 429 and any 5xx are retried with backoff while the client has not
// yet received any bytes, so the client only ever sees the final outcome. It
// parses provider retry hints (Retry-After, x-ratelimit-reset). Any retry
// (429, 5xx, transport) trips the provider+key group: that request owns the
// next send. After success or exhaust, exactly one sibling is the probe; a
// clean first-attempt success restores full concurrency, another retry keeps
// it at one-at-a-time. Exhausted retryable failures double a request-level
// backoff (base_backoff → max_backoff); a success resets it. WaitSend also
// honors operator pause on retries and remaining nextAllowedAt across pause.
// When retries are exhausted the last upstream response is returned as-is
// (failures are never masked). A 429 whose error envelope is a durable
// quota/billing condition (nonRetryableQuotaClasses) is never retried -
// waiting cannot clear it.
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
	for attempt := 0; ; attempt++ {
		var err error
		// The opening send honors holds when it is itself a retry (the
		// quality re-run); internal retries always do (attempt > 0).
		own, err = s.scheduler.WaitSend(ctx, groupKey, hooks, attempt > 0 || firstSendIsRetry, own)
		if err != nil {
			end(false)
			return nil, nil, err
		}
		// Stamp after WaitSend so TTFT is the winning send, not the backoff.
		attemptStart := time.Now()
		// Per-send deadline: X-Proxy-Timeout-Ms bounds this ONE send (the
		// Do below and the returned body), anchored here - a fresh budget per
		// attempt, never eaten by the waits above.
		attemptCtx, attemptCancel := withSendTimeout(ctx, t.timeout)
		// Rebuild the upstream request (body can't be reused across retries).
		upstream, err := s.buildUpstreamRequest(attemptCtx, r, t, key, body)
		if err != nil {
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
				end(false)
				return nil, nil, err
			}
			// Genuine transport failure (no response received). Retry the
			// transient ones (header timeout, conn reset/refused, EOF, DNS
			// timeout) transparently; give up on permanent ones (NXDOMAIN,
			// TLS errors) or when the retry budget is spent.
			if !isRetryableTransportErr(err) || attempt >= maxRetries {
				end(isRetryableTransportErr(err))
				return nil, nil, err
			}
			rec.Attempts = append(rec.Attempts, metrics.RetryAttempt{
				StatusCode: 0,
				ErrorType:  "transport",
				ErrorMsg:   transportErrText(err),
				At:         time.Now(),
			})
			rec.Retries++
			s.publishUpdate(rec)
			retried = true
			own = s.scheduler.Trip(groupKey, s.scheduler.BackoffFor(groupKey, 0)) || own
			continue
		}

		// A 429 is normally transient flow control, but a 429 can also be the
		// provider announcing a durable account condition (quota/credits/spend
		// limits) that waiting can never clear. Only the body's structured
		// error type/code tells them apart, so peek the bounded envelope
		// BEFORE the retry decision: a durable 429 is surfaced to the client
		// immediately - no retry budget burned, no pacing of the group, the
		// absorbed-attempt log stays clean (nothing was absorbed).
		//
		// A peek that OVERFLOWS the cap (len > maxErrBodyBytes) leaves the
		// body open: the final relay must serve prefix + live remainder
		// verbatim, and an oversized 429 keeps normal retry semantics. The
		// quota classification deliberately stays bounded by the cap - a
		// >64 KiB "quota envelope" is pathological (real quota envelopes are
		// tiny JSON).
		var errBody []byte
		peekLeftOpen := false
		if resp.StatusCode == http.StatusTooManyRequests {
			errBody, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes+1))
			if len(errBody) <= maxErrBodyBytes {
				resp.Body.Close()
				typ, code, _ := metrics.ParseErrorEnvelope(errBody)
				if isNonRetryableQuotaErr(typ, code) {
					if attempt > 0 {
						rec.FinalAttemptAt = attemptStart
					}
					end(false)
					// Re-serve the captured bytes so the client still gets the
					// full error body verbatim.
					resp.Body = io.NopCloser(bytes.NewReader(errBody))
					return resp, attemptCancel, nil
				}
			} else {
				peekLeftOpen = true
			}
		}

		// Only retry transient conditions: 429 (rate limit) and any 5xx
		// (upstream/server error). 4xx client errors are final. When retries
		// are exhausted the last upstream response is surfaced verbatim, so
		// the client sees the real error - retries are transparent, failures
		// are not masked.
		if !isRetryable(resp.StatusCode) || attempt >= maxRetries {
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

		// Parse provider retry hints for a smart delay.
		retryAfter := parseRetryAfter(resp)
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
		at := metrics.RetryAttempt{
			StatusCode:   resp.StatusCode,
			RetryAfterMs: int(retryAfter.Milliseconds()),
			At:           time.Now(),
		}
		at.ErrorType, at.ErrorCode, at.ErrorMsg = parseErrorBody(errBody)
		rec.Attempts = append(rec.Attempts, at)

		rec.Retries++
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
func parseRetryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Duration(secs) * time.Second
		}
		if ts, err := http.ParseTime(v); err == nil {
			return time.Until(ts)
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
				return d
			}
			if secs, err := strconv.ParseFloat(v, 64); err == nil {
				return time.Duration(secs * float64(time.Second))
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
			rec.FirstTokenAt = time.Time{}
			rec.LastTokenAt = time.Time{}
			rec.Usage = metrics.Usage{}
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
			markStreamErr(ctx, rec, err, "stream_read_error")
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
			markStreamErr(ctx, rec, err, "stream_read_error")
		}
		return
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, sseScannerBufInit), sseScannerBufMax)
	var a sse.Analyzer
	a.CostKeys = s.costKeysFor(rec.Provider)
	a.UsageKeys = s.usageKeysFor(rec.Provider)
	a.CapturePreview = s.cfg().CaptureBodyPreview
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
		markStreamErr(ctx, rec, err, "stream_read_error")
	}
	a.Fill(rec)
	if err := scanner.Err(); errors.Is(err, metrics.ErrMetricRange) {
		invalidateUsage(rec, err)
	}
}

// streamBody forwards an SSE body, preserving bytes exactly, and flushes after
// each event boundary so TTFT == upstream TTFT. Alongside the copy it scans
// for token timestamps and the final usage chunk using an SSE analyzer.
//
// The relay withholds the stream's terminal region - everything from the
// first event carrying a non-null finish_reason or a [DONE] - until the
// stream ends, then releases it verbatim. When the analyzer classifies the
// finished stream as degenerate (metrics.ClassifyOutcome: truncated /
// tool_calls-without-calls / empty stop), the held region is replaced with an
// in-band OpenAI error chunk the client treats as a retryable failure. The
// withholding is what makes that work: SDKs treat the finish chunk as the
// stream's end, so an error written after it would be ignored. The hold is
// bounded by terminalHoldMax (past it the stream commits to verbatim relay -
// no degenerate stream has a large "terminal" region), and every non-held
// byte is written through immediately, so the hot path only ever reserves
// one event's worth of bytes.
func (s *Server) streamBody(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		if _, err := io.Copy(disconnectWriter{w, rec}, body); err != nil {
			markStreamErr(ctx, rec, err, "stream_read_error")
		}
		return
	}
	var (
		a       sse.Analyzer
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
	)
	a.CostKeys = s.costKeysFor(rec.Provider)
	a.UsageKeys = s.usageKeysFor(rec.Provider)
	a.CapturePreview = s.cfg().CaptureBodyPreview
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
	// clean EOF for streams that end without those markers.
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
							return
						}
					}
				case !prev && a.Terminated():
					// This line opened the terminal region: withhold it (and
					// everything after) until the outcome is decided.
					holding = true
					hold = append(hold, line...)
					hold = append(hold, '\n')
					if !writeOut() {
						return
					}
					if sse.TerminalLine(line) {
						if !finishHold() {
							return
						}
					}
				default:
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
						return
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
				return
			}
		}
		if err != nil {
			now := time.Now()
			// Flush any trailing partial line into the analyzer. A partial
			// line can never be a terminal marker, so it is never withheld.
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
				// A real read failure is the transport's own error - release
				// the held bytes verbatim (byte preservation) and never
				// substitute a synthetic outcome on top.
				markStreamErr(ctx, rec, err, "stream_read_error")
				if holding {
					if _, werr := w.Write(hold); werr != nil {
						markClientGone(rec)
						return
					}
					hold = hold[:0]
				}
				if !writeOut() {
					return
				}
				a.Fill(rec)
				return
			}
			// Clean EOF: resolve the hold (truncation is decided here - a
			// stream without any terminal marker). A trailing partial line
			// released into out is written before any substituted chunk by
			// finishHold's writeOut-first ordering.
			if !finishHold() {
				return
			}
			a.Fill(rec)
			return
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
	obj := map[string]any{"error": map[string]any{
		"message": metrics.DegenerateMessage(code),
		"type":    "upstream_error",
		"param":   nil,
		"code":    code,
	}}
	if b, err := json.Marshal(obj); err == nil {
		line := append([]byte("data: "), b...)
		line = append(line, '\n', '\n')
		a.Feed(line[:len(line)-2], now)
		if _, werr := w.Write(line); werr != nil {
			markClientGone(rec)
			return
		}
	}
	a.Feed([]byte("data: [DONE]"), now)
	if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil {
		markClientGone(rec)
	}
}
