package proxy

// Round 4 batch 5 regressions (F2, F5): Responses provider-executed call
// items are tool activity on both the non-stream decoder and the streaming
// analyzer, and one MCP call is counted once across its announcement and the
// terminal document.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func nativeResponsesCallBody(typ string) string {
	return fmt.Sprintf(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",`+
		`"output":[{"type":%q,"id":"item_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}],`+
		`"usage":{"input_tokens":10,"output_tokens":50,"total_tokens":60}}`, typ)
}

func TestNativeResponsesProviderToolCallRecorded(t *testing.T) {
	for _, typ := range []string{"custom_tool_call", "local_shell_call", "mcp_call", "computer_call", "function_call"} {
		t.Run(typ, func(t *testing.T) {
			body := nativeResponsesCallBody(typ)
			up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","input":"hi"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if w.Body.String() != body {
				t.Fatalf("native body was not relayed verbatim: %s", w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.FinishReason != "tool_calls" {
				t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
			}
			if rec.ToolCalls != 1 {
				t.Fatalf("tools = %d, want 1", rec.ToolCalls)
			}
			if rec.HadAnswerContent || rec.AnswerTokens != 0 || rec.GenTokens != 0 {
				t.Fatalf("tool-only output = hadAnswer %v answer %d gen %d, want false/0/0", rec.HadAnswerContent, rec.AnswerTokens, rec.GenTokens)
			}
		})
	}
}

func TestResponsesProviderToolWarningReachesTranslatedClient(t *testing.T) {
	body := nativeResponsesCallBody("custom_tool_call")
	up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(got, "output.custom_tool_call") {
		t.Fatalf("dropped provider call warning missing from the translated client: %q", got)
	}
}

func TestResponsesMCPCallCountedOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		added    string
		terminal string
	}{
		{
			name:     "mcp added then terminal",
			added:    `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"mcp_call","id":"mcp_1","name":"lookup"}}` + "\n\n",
			terminal: `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"mcp_call","id":"mcp_1","name":"lookup"}]}}` + "\n\n",
		},
		{
			name:     "mcp terminal only",
			terminal: `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"mcp_call","id":"mcp_1","name":"lookup"}]}}` + "\n\n",
		},
		{
			name:     "function call with both identities",
			added:    `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}` + "\n\n",
			terminal: `data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}}` + "\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := streamUpstream(t, tc.added, tc.terminal)
			defer upstream.Close()
			buf := metrics.NewBuffer(10)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := streamRequestTo(t, srv, upstream, "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", status, got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ToolCalls != 1 {
				t.Fatalf("tools = %d, want 1 (record %+v, body %q)", rec.ToolCalls, rec, got)
			}
			if rec.FinishReason != "tool_calls" {
				t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
			}
		})
	}
}
