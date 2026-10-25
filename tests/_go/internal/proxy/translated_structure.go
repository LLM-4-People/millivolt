package proxy

// Round 12 F3 regression: a provider-executed Responses item is dropped from
// the translated wire (no target block), but it is still model tool activity.
// The Chat-shaped non-stream analysis and the translated stream analysis both
// counted zero, leaving finish_reason tool_calls with ToolCalls 0.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func translatedStreamRequest(t *testing.T, srv, upstream *httptest.Server, path, format, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", format)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTranslatedProviderToolActivity(t *testing.T) {
	t.Run("non-stream to chat", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, nativeResponsesCallBody("custom_tool_call"))
		buf := metrics.NewBuffer(8)
		s := New(config.Default(), buf)
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) {
			t.Fatalf("Chat body lost the tool finish: %s", w.Body.String())
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.ToolCalls != 1 {
			t.Fatalf("tools = %d, want the provider call counted once: %+v", rec.ToolCalls, rec)
		}
		if rec.FinishReason != "tool_calls" {
			t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
		}
	})

	t.Run("non-stream to anthropic", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, nativeResponsesCallBody("custom_tool_call"))
		buf := metrics.NewBuffer(8)
		s := New(config.Default(), buf)
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/messages", "openai-responses",
			`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.ToolCalls != 1 || rec.FinishReason != "tool_calls" {
			t.Fatalf("record tools/finish = %d/%q, want 1/tool_calls", rec.ToolCalls, rec.FinishReason)
		}
	})

	providerStream := []string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_1","name":"do"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"custom_tool_call","call_id":"call_1","name":"do"}]}}` + "\n\n",
	}
	for _, tc := range []struct {
		name   string
		path   string
		format string
		body   string
	}{
		{
			name:   "stream to chat",
			path:   "/v1/chat/completions",
			format: "openai-responses",
			body:   `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "stream to anthropic",
			path:   "/v1/messages",
			format: "openai-responses",
			body:   `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := streamUpstream(t, providerStream...)
			defer upstream.Close()
			buf := metrics.NewBuffer(8)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := translatedStreamRequest(t, srv, upstream, tc.path, tc.format, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", status, got)
			}
			rec := waitForRecord(t, buf, 1)[0]
			if rec.ToolCalls != 1 {
				t.Fatalf("tools = %d, want the provider call counted once (body %s)", rec.ToolCalls, got)
			}
			if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "do" {
				t.Fatalf("retained tool names = %v, want [do]", rec.ToolNames)
			}
			if rec.FinishReason != "tool_calls" {
				t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
			}
		})
	}

	t.Run("native function call stream control", func(t *testing.T) {
		upstream := streamUpstream(t,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`+"\n\n",
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"call_1","delta":"{\"q\":1}"}`+"\n\n",
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]}}`+"\n\n",
		)
		defer upstream.Close()
		buf := metrics.NewBuffer(8)
		srv := httptest.NewServer(New(config.Default(), buf))
		defer srv.Close()

		status, got := translatedStreamRequest(t, srv, upstream, "/v1/chat/completions", "openai-responses",
			`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", status, got)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.ToolCalls != 1 {
			t.Fatalf("tools = %d, want exactly 1 (no double count): %+v", rec.ToolCalls, rec)
		}
		if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
			t.Fatalf("retained tool names = %v, want [lookup]", rec.ToolNames)
		}
	})

	t.Run("native function call non-stream control", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, nativeResponsesCallBody("function_call"))
		buf := metrics.NewBuffer(8)
		s := New(config.Default(), buf)
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if got := strings.Count(w.Body.String(), `"id":"call_1"`); got != 1 {
			t.Fatalf("Chat body call id count = %d, want 1: %s", got, w.Body.String())
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.ToolCalls != 1 {
			t.Fatalf("tools = %d, want exactly 1 (no double count): %+v", rec.ToolCalls, rec)
		}
	})
}
