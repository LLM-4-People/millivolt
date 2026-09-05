package proxy

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
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
	raw := string(c.buf)
	if !utf8.Valid(c.buf) {
		raw = string(bytes.ToValidUTF8(c.buf, []byte("\uFFFD")))
	}
	return debugBody{Raw: raw, Bytes: c.seen, Truncated: c.truncated || c.seen > len(c.buf)}
}

func capBytes(b []byte, limit int64) debugBody {
	n := len(b)
	trunc := false
	if limit > 0 && int64(n) > limit {
		b = b[:limit]
		trunc = true
	}
	raw := string(b)
	if !utf8.Valid(b) {
		raw = string(bytes.ToValidUTF8(b, []byte("\uFFFD")))
	}
	return debugBody{Raw: raw, Bytes: n, Truncated: trunc}
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
	if strings.Contains(n, "token") && !strings.HasPrefix(n, "x-ratelimit") {
		return true
	}
	return strings.Contains(n, "key") && strings.Contains(n, "api")
}

// captureHeaders is the record-retention boundary, separate from transparent
// forwarding. Neither ordinary metrics nor debug captures retain credentials.
func captureHeaders(h http.Header, authHeader string) http.Header {
	out := make(http.Header, len(h))
	for name, values := range h {
		if redactHeaderName(name, authHeader) {
			out[name] = []string{"[REDACTED]"}
		} else {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
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
		reqHdr:   headersToHAR(r.Header, t.authHeader),
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

func (s *Server) finishDebugTap(tap *debugTap, rec *metrics.Record) {
	if tap == nil || rec == nil || !rec.Debug {
		return
	}
	if s.pause.persist == nil {
		return
	}
	ttl := s.cfg().DebugCaptureTTL
	now := time.Now()
	expires := now.Add(ttl)
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
		"redaction": []string{
			"Authorization", "X-Api-Key", "Cookie", "Set-Cookie",
			"X-Proxy-Refresh-Token", "X-Proxy-Access-Token",
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		log.Printf("debug: marshal capture %s: %v", rec.ID, err)
		return
	}
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
