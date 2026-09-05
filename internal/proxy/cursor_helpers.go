package proxy

// Helpers for the resumable Cursor Run path (cursor_bidi.go): request parsing
// (tool results) and the OpenAI SSE/JSON emitters that render a turn's deltas.

import (
	"encoding/json"
	"fmt"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"io"
	"net/http"
	"strings"
	"time"
)

// cursorToolResult is one role:"tool" message from a client request.
type cursorToolResult struct {
	toolCallID string
	name       string
	content    string
	isError    bool
}

// extractToolResults pulls the role:"tool" messages out of an OpenAI request
// body (the client's answers to a prior tool_calls turn).
func extractToolResults(body []byte) []cursorToolResult {
	var in struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			Name       string          `json:"name"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &in) != nil {
		return nil
	}
	var out []cursorToolResult
	for _, m := range in.Messages {
		if m.Role != "tool" {
			continue
		}
		tr := cursorToolResult{toolCallID: m.ToolCallID, name: m.Name}
		// content may be a string or an array of {type:"text",text} parts.
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			tr.content = s
		} else {
			var parts []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(m.Content, &parts) == nil {
				var sb strings.Builder
				for _, p := range parts {
					sb.WriteString(p.Text)
				}
				tr.content = sb.String()
			}
		}
		if tr.toolCallID != "" {
			out = append(out, tr)
		}
	}
	return out
}

// toolResultIDs returns the tool_call_ids of a request's role:"tool" messages.
func toolResultIDs(results []cursorToolResult) []string {
	var ids []string
	for _, tr := range results {
		ids = append(ids, tr.toolCallID)
	}
	return ids
}

// writeSSEHeaders writes the SSE response headers + status for a cursor stream.
func writeSSEHeaders(w http.ResponseWriter, status int) {
	if status == 0 {
		status = http.StatusOK
	}
	hdr := make(http.Header, 3)
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	if p := pacerOf(w); p != nil {
		p.applyUpstream(hdr, status)
		return
	}
	copyResponseHeaders(w.Header(), hdr)
	w.WriteHeader(status)
}

// sseEmitter returns a function that emits one OpenAI chunk (delta + optional
// finish_reason) as an SSE frame to w, flushing after each. model stamps the
// request model on every chunk (OpenAI echoes it; clients may log it).
func sseEmitter(w http.ResponseWriter, id, model string, flusher http.Flusher) func(delta map[string]any, finish any) error {
	return func(delta map[string]any, finish any) error {
		obj := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
}

// emitErrorSSE emits an OpenAI error object in-band on an already-committed
// SSE stream, then [DONE] so SDKs stop waiting. typ/msg are public strings
// (never a raw *url.Error - that embeds the upstream URL). The returned
// error is the failed client write, if any: the caller owns the record and
// marks the disconnect per its own path's contract - cursor keeps its
// documented committed-200 exception (rec.ClientDisconnected only, never
// markClientGone's 499), the passthrough paths markClientGone.
func emitErrorSSE(w http.ResponseWriter, id, typ, msg string) error {
	if typ == "" {
		typ = "upstream_error"
	}
	obj := map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": nil,
	}}
	if id != "" {
		obj["id"] = id
	}
	var wErr error
	if b, mErr := json.Marshal(obj); mErr == nil {
		if _, e := fmt.Fprintf(w, "data: %s\n\n", b); e != nil {
			wErr = e
		}
	}
	if _, e := io.WriteString(w, "data: [DONE]\n\n"); e != nil && wErr == nil {
		wErr = e
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return wErr
}

func pacerOf(w http.ResponseWriter) *ssePacer {
	for w != nil {
		if p, ok := w.(*ssePacer); ok {
			return p
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	return nil
}

// writeClientError writes a JSON HTTP error if headers are still open, or
// an in-band SSE error+[DONE] if the idle pacer already committed 200.
func writeClientError(w http.ResponseWriter, rec *metrics.Record, typ, msg string, code int) {
	writeClientErrorHdr(w, rec, typ, msg, code, nil)
}

func writeClientErrorHdr(w http.ResponseWriter, rec *metrics.Record, typ, msg string, code int, extra http.Header) {
	if rec != nil {
		if rec.StatusCode == 0 {
			rec.StatusCode = code
		}
		if rec.ErrorType == "" {
			rec.ErrorType = typ
		}
		if rec.ErrorMsg == "" {
			rec.ErrorMsg = msg
		}
	}
	id := ""
	if rec != nil {
		id = rec.ID
	}
	if p := pacerOf(w); p != nil {
		if p.commitError(extra, code, errJSON(typ, msg)) {
			if err := emitErrorSSE(w, id, typ, msg); err != nil && rec != nil {
				// The in-band error hit a dead socket: the LOCAL client is
				// gone. Safe for every caller of this helper - the status is
				// the just-stamped error code (≥400), so markClientGone only
				// adds the disconnect flag, never a cursor-200.
				markClientGone(rec)
			}
		}
		return
	}
	if extra != nil {
		copyResponseHeaders(w.Header(), extra)
	}
	http.Error(w, errJSON(typ, msg), code)
}

// cursorAgentContextBaseTokens is the fixed agent-context overhead Cursor's
// agent.v1 wrapper adds to every grok prompt (~2.7k tokens measured with a
// 5-token user message). It's folded into the parked-turn input estimate so
// the client's accounting stays close to what the upstream actually bills.
// Internal, measured protocol constant - not user-tunable.
const cursorAgentContextBaseTokens = 2700

// estimateInputTokens approximates the input tokens a cursor turn consumes,
// from the request's message payload alone. It's used ONLY when Cursor's exact
// usage summary hasn't arrived yet (a parked tool-call turn ends before the
// summary lands - the real number arrives on the resume turn's summary). A
// char/4 estimate keeps per-turn billing monotonic with what the client sent,
// so tools that drive context management off reported usage
// don't think every tool turn is free.
func estimateInputTokens(body []byte) int64 {
	var in struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &in) != nil {
		return cursorAgentContextBaseTokens
	}
	var chars int64
	for _, m := range in.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			chars += int64(len(s))
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				chars += int64(len(p.Text))
			}
		}
	}
	return int64(len(in.Messages)) + chars/4 + cursorAgentContextBaseTokens
}

// accumulateJSONDelta folds a streamed delta into a non-streaming completion.
func accumulateJSONDelta(content *strings.Builder, toolCalls *[]map[string]any, delta map[string]any) {
	if c, ok := delta["content"].(string); ok {
		content.WriteString(c)
	}
	if tcs, ok := delta["tool_calls"].([]map[string]any); ok {
		*toolCalls = append(*toolCalls, tcs...)
	}
}
