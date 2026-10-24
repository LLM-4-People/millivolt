package proxy

// Round 3 batch 8 regressions (F2): a native Responses or Anthropic 2xx body
// is decoded through the canonical codec before commit so the record carries
// its finish reason, tool calls, answer presence, preview and folded usage.
// The default native passthrough carries no X-Proxy-Format header, so every
// case runs both on that path and with the redundant declared format.

import (
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestNativeResponsesFunctionCallStructure(t *testing.T) {
	const body = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}],` +
		`"usage":{"input_tokens":10,"output_tokens":50,"total_tokens":60}}`
	for _, tc := range []struct {
		name   string
		format string
	}{
		{"native path", ""},
		{"declared format", "openai-responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/responses", tc.format, `{"model":"m","input":"hi"}`)
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
			if rec.ToolCalls != 1 || len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
				t.Fatalf("tools = %d names %v, want 1 lookup", rec.ToolCalls, rec.ToolNames)
			}
			if rec.HadAnswerContent {
				t.Fatal("a function-call-only response reported answer content")
			}
			if rec.AnswerTokens != 0 || rec.GenTokens != 0 {
				t.Fatalf("answer/gen = %d/%d, want 0/0 for a tool-only response", rec.AnswerTokens, rec.GenTokens)
			}
			if rec.Usage.InputTokens != 10 || rec.Usage.OutputTokens != 50 || rec.Usage.TotalTokens != 60 {
				t.Fatalf("usage = %d/%d/%d, want 10/50/60", rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.Usage.TotalTokens)
			}
			if rec.ProviderRequestID != "resp_1" || rec.ProviderModel != "m" {
				t.Fatalf("identity = id %q model %q, want resp_1/m", rec.ProviderRequestID, rec.ProviderModel)
			}
		})
	}
}

func TestNativeAnthropicCacheUsage(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"cache hit"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":20,"output_tokens":50}}`
	for _, tc := range []struct {
		name   string
		format string
	}{
		{"native path", ""},
		{"declared format", "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
			buf := metrics.NewBuffer(10)
			cfg := config.Default()
			cfg.CaptureBodyPreview = true
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/messages", tc.format, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			want := metrics.Usage{InputTokens: 170, OutputTokens: 50, TotalTokens: 220, CacheReadTokens: 50, CacheWrite: 20}
			if rec.Usage != want {
				t.Fatalf("usage = %+v, want %+v", rec.Usage, want)
			}
			if rec.ResponsePreview != "cache hit" {
				t.Fatalf("preview = %q, want cache hit", rec.ResponsePreview)
			}
			if rec.FinishReason != "stop" || !rec.HadAnswerContent {
				t.Fatalf("finish = %q hadAnswer = %v, want stop/true", rec.FinishReason, rec.HadAnswerContent)
			}
		})
	}
}

func TestNativeResponsesTextPreview(t *testing.T) {
	const body = `{"id":"resp_2","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello preview"}]}],` +
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`
	for _, tc := range []struct {
		name   string
		format string
	}{
		{"native path", ""},
		{"declared format", "openai-responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
			buf := metrics.NewBuffer(10)
			cfg := config.Default()
			cfg.CaptureBodyPreview = true
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/responses", tc.format, `{"model":"m","input":"hi"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ResponsePreview != "hello preview" {
				t.Fatalf("preview = %q, want hello preview", rec.ResponsePreview)
			}
			if !rec.HadAnswerContent || rec.FinishReason != "stop" {
				t.Fatalf("structure = preview %q hadAnswer %v finish %q", rec.ResponsePreview, rec.HadAnswerContent, rec.FinishReason)
			}
			if rec.AnswerTokens != 2 || rec.GenTokens != 2 {
				t.Fatalf("answer/gen = %d/%d, want 2/2", rec.AnswerTokens, rec.GenTokens)
			}
		})
	}
}
