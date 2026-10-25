package proxy

// Round 7 verifier NEW-3 regression: a Chat refusal history translated to a
// Responses request sends the refusal text as input_text, reports the loss in
// warn mode and fails closed in reject mode before any upstream call.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const chatResponseWithRefusal = `{"model":"m","messages":[` +
	`{"role":"user","content":"hi"},` +
	`{"role":"assistant","content":"","refusal":"I cannot help with that"},` +
	`{"role":"user","content":"next"}]}`

const responsesCompletedAnswer = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`

func TestResponsesRequestRefusalPolicy(t *testing.T) {
	t.Run("warn mode sends input text", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, responsesCompletedAnswer)
		cfg := config.Default()
		cfg.QualityRetries = 0
		cfg.ThinkingRetries = 0
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", chatResponseWithRefusal)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		got := string(up.lastBody())
		if strings.Contains(got, `"type":"refusal"`) {
			t.Fatalf("upstream request contains the invalid refusal part: %s", got)
		}
		if !strings.Contains(got, `"type":"input_text"`) || !strings.Contains(got, "I cannot help with that") {
			t.Fatalf("upstream request lost the refusal text: %s", got)
		}
		if header := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(header, "content.refusal") {
			t.Fatalf("warnings header = %q, want content.refusal", header)
		}
	})

	t.Run("reject mode fails before upstream", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, responsesCompletedAnswer)
		cfg := config.Default()
		cfg.TranslationLoss = config.TranslationLossReject
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", chatResponseWithRefusal)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "content.refusal") {
			t.Fatalf("rejection does not name the refusal part: %s", w.Body.String())
		}
		if got := up.calls.Load(); got != 0 {
			t.Fatalf("upstream calls = %d, want 0", got)
		}
	})
}
