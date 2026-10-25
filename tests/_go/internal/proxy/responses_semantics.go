package proxy

// Round 3 batch 14 regression (A3): a Chat max_tokens below the Responses
// minimum is clamped with a warning, and reject-mode translation fails closed.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesMaxOutputTokensClamped(t *testing.T) {
	const response = `{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	up := newFixtureUpstream(t, "application/json", http.StatusOK, response)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	body := `{"model":"m","max_tokens":15,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(string(up.lastBody()), `"max_output_tokens":16`) {
		t.Fatalf("upstream body did not clamp the limit: %s", up.lastBody())
	}
	if got := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(got, "max_output_tokens") {
		t.Fatalf("warnings = %q, want max_output_tokens", got)
	}
}

func TestResponsesMaxOutputTokensRejectMode(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	body := `{"model":"m","max_tokens":15,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "max_output_tokens") {
		t.Fatalf("rejection does not name the clamped field: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}
