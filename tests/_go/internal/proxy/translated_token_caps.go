package proxy

// Round 7 verifier D1: accepted integral cap spellings must work on the
// translation path too. The format decoders now share jsonnum, so 1e6,
// 1000000.0 and 1E+6 reach the upstream as the canonical integer and the record
// keeps the reservation ceiling instead of the request failing before any send.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const translatedAnthropicCompletion = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

const translatedChatCompletion = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

func TestTranslatedTokenCapSpellingsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		format   string
		body     string
		response string
	}{
		{
			name:     "chat 1e6",
			path:     "/v1/chat/completions",
			format:   "anthropic",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1e6}`,
			response: translatedAnthropicCompletion,
		},
		{
			name:     "chat 1000000.0",
			path:     "/v1/chat/completions",
			format:   "anthropic",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000.0}`,
			response: translatedAnthropicCompletion,
		},
		{
			name:     "chat 1E+6",
			path:     "/v1/chat/completions",
			format:   "anthropic",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1E+6}`,
			response: translatedAnthropicCompletion,
		},
		{
			name:     "chat max_completion_tokens 1e6",
			path:     "/v1/chat/completions",
			format:   "anthropic",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1e6}`,
			response: translatedAnthropicCompletion,
		},
		{
			name:     "responses max_output_tokens 1e6",
			path:     "/v1/responses",
			format:   "anthropic",
			body:     `{"model":"m","input":"hi","max_output_tokens":1e6}`,
			response: translatedAnthropicCompletion,
		},
		{
			name:     "anthropic max_tokens 1e6",
			path:     "/v1/messages",
			format:   "openai",
			body:     `{"model":"m","max_tokens":1e6,"messages":[{"role":"user","content":"hi"}]}`,
			response: translatedChatCompletion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, tc.response)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
			if got := string(up.lastBody()); !strings.Contains(got, `"max_tokens":1000000`) {
				t.Fatalf("upstream body = %s, want the canonical integer cap", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			if recs[0].ReqMaxTokens == nil || *recs[0].ReqMaxTokens != 1000000 {
				t.Fatalf("record cap = %v, want 1000000", recs[0].ReqMaxTokens)
			}
		})
	}
}

// The translated path still rejects a fractional or hostile cap before any
// upstream send; the accepted spellings must not widen that boundary.
func TestTranslatedTokenCapMalformedStillRejected(t *testing.T) {
	for _, literal := range []string{"1.5", "1e9", `"1000000"`} {
		t.Run(literal, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, translatedAnthropicCompletion)
			cfg := config.Default()
			s := New(cfg, metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":` + literal + `}`
			w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if got := up.calls.Load(); got != 0 {
				t.Fatalf("upstream calls = %d, want 0", got)
			}
		})
	}
}
