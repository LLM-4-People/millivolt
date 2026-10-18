package proxy

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const debugSchema = "millivolt.debug/v1"

type harNV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type debugBody struct {
	Raw       string `json:"raw"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// sanitizeDebugBody builds the retained debug view of a captured body: the
// bytes are sanitized to valid UTF-8 for JSON display, and the caller's
// observed size and truncation flag ride along unchanged. Cap and truncation
// policies stay with the callers.
func sanitizeDebugBody(b []byte, size int, trunc bool) debugBody {
	raw := string(b)
	if !utf8.Valid(b) {
		raw = string(bytes.ToValidUTF8(b, []byte("\uFFFD")))
	}
	return debugBody{Raw: raw, Bytes: size, Truncated: trunc}
}

type cappedWriter struct {
	buf       []byte
	limit     int
	truncated bool
	seen      int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.seen += len(p)
	if c.limit <= 0 {
		c.truncated = true
		return len(p), nil
	}
	remain := c.limit - len(c.buf)
	if remain <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		c.buf = append(c.buf, p[:remain]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *cappedWriter) body() debugBody {
	return sanitizeDebugBody(c.buf, c.seen, c.truncated || c.seen > len(c.buf))
}

func capBytes(b []byte, limit int64) debugBody {
	n := len(b)
	trunc := false
	if limit > 0 && int64(n) > limit {
		b = b[:limit]
		trunc = true
	}
	return sanitizeDebugBody(b, n, trunc)
}

func redactHeaderName(name, authHeader string) bool {
	if authHeader != "" && strings.EqualFold(name, authHeader) {
		return true
	}
	n := strings.ToLower(name)
	switch n {
	case "authorization", "proxy-authorization", "x-api-key", "api-key",
		"cookie", "set-cookie", "x-proxy-refresh-token", "x-proxy-access-token",
		"x-proxy-key", "x-proxy-headers":
		return true
	}
	if strings.Contains(n, "authorization") || strings.Contains(n, "api-key") {
		return true
	}
	if strings.Contains(n, "secret") {
		return true
	}
	// "token" deny-by-default with explicit accounting exemptions: the
	// OpenAI x-ratelimit-* and Anthropic anthropic-ratelimit-* families
	// carry token-budget counters, not credentials, and the client queue
	// control headers x-proxy-limit-requests/-concurrency/-tokens are not
	// secrets. Every other token-bearing name stays redacted. The ratelimit
	// exemptions are prefix-wide, so a token-named header under them (e.g.
	// x-ratelimit-token) is retained; accepted residual, since the upstream
	// already holds the credential and any header name can carry data.
	if strings.Contains(n, "token") &&
		!strings.HasPrefix(n, "x-ratelimit") &&
		!strings.HasPrefix(n, "anthropic-ratelimit") &&
		n != "x-proxy-limit-tokens" {
		return true
	}
	return strings.Contains(n, "key") && strings.Contains(n, "api")
}

// captureHeaders is the record-retention boundary, separate from transparent
// forwarding. Neither ordinary metrics nor debug captures retain credentials.
// Values are capped at a generous header-sized bound: the inbound server cap
// (1 MiB) lets a hostile peer put a megabyte into one header that would then be
// retained by the ring, every absorbed attempt, the database row and the debug
// capture sidecar. Names are capped too and charged with their value in every
// branch, including redacted credential headers: an overlong name is skipped
// rather than truncated into a colliding map key, and the fixed redaction
// marker still spends the shared budget. The byte budget alone does not bound
// the map: thousands of short names fit it while the map's bucket array still
// scales with the upstream header count, so the retained entry count has its
// own cap.
func captureHeaders(h http.Header, authHeader string) http.Header {
	out := make(http.Header)
	// Deterministic total budget: names are visited in sorted order so which
	// oversized headers survive does not depend on map iteration order. A
	// header map otherwise bounded only per value could retain 64 KiB per name
	// across hundreds of names, multiplied by every absorbed attempt.
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	used := 0
	for _, name := range names {
		// Names are visited in sorted order, so the admitted set is
		// deterministic. Once the entry cap is reached no later name can be
		// added; stop rather than scan a hostile 60k-name map to the end.
		if len(out) >= retainedHeaderEntriesMax {
			break
		}
		// Judge the name before redaction: an overlong credential-bearing name
		// must not allocate a lowercase copy or reach the retained map.
		if len(name) > retainedHeaderNameMax {
			continue
		}
		values := h[name]
		if redactHeaderName(name, authHeader) {
			// The fixed marker is small, and retaining it keeps the fact that a
			// credential header was present visible to the operator. Its name
			// and marker share the ordinary-value budget like every other entry,
			// so a flood of redacted names cannot exceed the map total.
			const marker = "[REDACTED]"
			if used+len(name)+len(marker) > retainedHeadersMaxBytes {
				continue
			}
			used += len(name) + len(marker)
			out[name] = []string{marker}
			continue
		}
		capped := make([]string, 0, min(len(values), retainedHeaderValuesMax))
		for _, v := range values {
			if len(capped) == retainedHeaderValuesMax {
				break
			}
			v = retainedText(v, retainedHeaderValueMax)
			cost := len(name) + len(v)
			if used+cost > retainedHeadersMaxBytes {
				break
			}
			used += cost
			capped = append(capped, v)
		}
		if len(capped) > 0 {
			out[name] = capped
		}
	}
	return out
}

// Retained-text bounds for the record boundary. Internal guardrails, not
// user-tunable: they protect the ring, retry attempts and durable rows from
// client-controlled megabytes while leaving every real identifier intact.
const (
	retainedLabelMax         = 512 // model id
	retainedClientMax        = metrics.RetainedClientMax
	retainedClientLangMax    = metrics.RetainedClientLangMax
	retainedUserAgentMax     = metrics.RetainedUserAgentMax
	retainedClientIPMax      = metrics.RetainedClientIPMax
	retainedHeaderNameMax    = 512   // one retained header name; a longer name is skipped
	retainedHeaderValueMax   = 4096  // one retained header value
	retainedHeaderValuesMax  = 16    // values retained per header name
	retainedHeaderEntriesMax = 256   // names retained across one header map
	retainedHeadersMaxBytes  = 16384 // total retained bytes across one header map
	retainedTruncationSuffix = "…"
)

// retainedText truncates at a UTF-8 boundary and marks the cut.
func retainedText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(retainedTruncationSuffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + retainedTruncationSuffix
}

func headersToHAR(h http.Header, authHeader string) []harNV {
	if h == nil {
		return []harNV{}
	}
	out := make([]harNV, 0, len(h))
	for name, vs := range h {
		val := strings.Join(vs, ", ")
		if redactHeaderName(name, authHeader) {
			val = "[REDACTED]"
		}
		out = append(out, harNV{Name: name, Value: val})
	}
	return out
}

type debugTap struct {
	reqHdr   []harNV
	reqBody  debugBody
	respBody *cappedWriter
	upstream string
}

func (s *Server) beginDebugTap(r *http.Request, t *target, rec *metrics.Record, reqBody []byte) *debugTap {
	// A capture only exists in durable storage, so a process without a store
	// must not copy headers or bodies onto the request path: the record would
	// claim debug=true while no fetchable sidecar could ever exist. HandleDebug
	// refuses to create that state; this guard keeps the retention boundary
	// closed even for an in-memory session.
	if s.pause.persist == nil {
		return nil
	}
	d := s.matchDebug(rec.Client, rec.Provider, rec.Model)
	if d == nil {
		return nil
	}
	rec.Debug = true
	rec.DebugSessionID = d.ID
	limit := int(s.cfg().DebugCaptureMaxBytes)
	if limit < 0 {
		limit = 0
	}
	half := limit
	if half > 1 {
		half = limit / 2
	}
	return &debugTap{
		// Redact and bound first, through the same retention owner as the
		// recorded response headers, then convert the bounded map to the HAR
		// list the drawer consumes. The values are already redacted, so the
		// conversion cannot re-expand or re-expose a credential.
		reqHdr:   headersToHAR(captureHeaders(r.Header, t.authHeader), ""),
		reqBody:  capBytes(reqBody, int64(half)),
		respBody: &cappedWriter{limit: half},
		upstream: t.baseURL + rec.Path,
	}
}

func (t *debugTap) wrapWriter(w http.ResponseWriter) http.ResponseWriter {
	if t == nil || w == nil {
		return w
	}
	return &debugRW{ResponseWriter: w, tap: t}
}

type debugRW struct {
	http.ResponseWriter
	tap *debugTap
}

func (d *debugRW) Write(p []byte) (int, error) {
	d.tap.respBody.Write(p)
	return d.ResponseWriter.Write(p)
}

func (d *debugRW) Flush() {
	if f, ok := d.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (d *debugRW) Unwrap() http.ResponseWriter { return d.ResponseWriter }

var _ http.Flusher = (*debugRW)(nil)

// debugCaptureBufPool reuses the capture encoder's scratch buffer. JSON
// encoding runs on the response path after the client-facing write, so the
// allocation is worth keeping out of that path; the buffer is reset before it
// returns to the pool and its bytes are only used until SaveDebugCapture
// copies them into the store.
var debugCaptureBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// prepareDebugCapture owns the completion/sidecar seam. It registers the
// pending capture marker and returns the save closure; callers must publish the
// completed record (s.rec.Record) immediately after registration and then run
// the closure synchronously in the same goroutine. The TTL and captured-at
// instant are fixed here, at completion time, so the later sidecar write cannot
// move them. A request with no debug tap, an unmatched record, or no durable
// store returns nil without touching the pending owner or allocating a closure.
func (s *Server) prepareDebugCapture(tap *debugTap, rec *metrics.Record) func() {
	if tap == nil || rec == nil || !rec.Debug || s.pause.persist == nil {
		return nil
	}
	release := s.registerPendingCapture(rec.ID)
	now := time.Now()
	expires := now.Add(s.cfg().DebugCaptureTTL)
	return func() {
		// Resolve on every path, including encode and save failures: waiters
		// then proceed to the ordinary 404/500 storage result.
		defer release()
		s.saveDebugCapture(tap, rec, now, expires)
	}
}

// saveDebugCapture encodes and stores one prepared capture. It runs after the
// completed record is published, so its cost delays neither the live end nor
// durable accounting; the pending marker stays registered until it returns.
func (s *Server) saveDebugCapture(tap *debugTap, rec *metrics.Record, now, expires time.Time) {
	doc := map[string]any{
		"schema":           debugSchema,
		"id":               rec.ID,
		"captured_at":      now.UTC().Format(time.RFC3339Nano),
		"expires_at":       expires.UTC().Format(time.RFC3339Nano),
		"debug_session_id": rec.DebugSessionID,
		"match": map[string]any{
			"client":   rec.Client,
			"provider": rec.Provider,
			"model":    rec.Model,
		},
		"identity": map[string]any{
			"conversation_id": rec.ConversationID,
			"client":          rec.Client,
			"provider":        rec.Provider,
			"model":           rec.Model,
			"key_hash":        rec.KeyHash,
			"stream":          rec.Stream,
			"path":            rec.Path,
			"method":          rec.Method,
			"format":          rec.ClientMeta.Format,
		},
		"timing": map[string]any{
			"start":           rec.Start.UTC().Format(time.RFC3339Nano),
			"end":             rec.End.UTC().Format(time.RFC3339Nano),
			"duration_ms":     rec.DurationMs,
			"ttft_ms":         rec.TTFTMs,
			"queue_wait_ms":   rec.QueueWaitMs,
			"first_answer_ms": firstAnswerMs(rec),
		},
		"outcome": map[string]any{
			"status_code":         rec.StatusCode,
			"finish_reason":       rec.FinishReason,
			"error_type":          rec.ErrorType,
			"error_code":          rec.ErrorCode,
			"error_msg":           rec.ErrorMsg,
			"client_disconnected": rec.ClientDisconnected,
		},
		"usage": rec.Usage,
		"cost":  rec.Cost,
		"request": map[string]any{
			"headers": tap.reqHdr,
			"body":    tap.reqBody,
			"url":     tap.upstream,
		},
		"response": map[string]any{
			"headers": headersToHAR(headerFromMap(rec.ResponseHeaders), ""),
			"body":    tap.respBody.body(),
		},
		"attempts": rec.Attempts,
	}
	// Encode without HTML escaping: the default turns <, > and & into six-byte
	// \u00XX sequences, which inflates code and HTML prompts far beyond the
	// body cap's apparent budget without changing what a JSON decoder sees.
	// Control bytes keep their JSON escapes and sanitizeDebugBody still owns
	// invalid-UTF-8 replacement, so this changes bytes, not semantics.
	buf := debugCaptureBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		buf.Reset()
		debugCaptureBufPool.Put(buf)
	}()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		log.Printf("debug: encode capture %s: %v", rec.ID, err)
		return
	}
	// Encoder.Encode terminates the document with a newline; drop it so the
	// stored document stays the one-line JSON shape callers already parse.
	buf.Truncate(buf.Len() - 1)
	raw := buf.Bytes()
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if err := s.pause.persist.SaveDebugCapture(ctx, rec.ID, rec.DebugSessionID, now.UnixMilli(), expires.UnixMilli(), raw); err != nil {
		log.Printf("debug: save capture %s: %v", rec.ID, err)
	}
}

func firstAnswerMs(rec *metrics.Record) int64 {
	if rec.FirstAnswerAt.IsZero() || rec.Start.IsZero() {
		return 0
	}
	ms := rec.FirstAnswerAt.Sub(rec.Start).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

func headerFromMap(m map[string][]string) http.Header {
	if m == nil {
		return nil
	}
	return http.Header(m)
}
