package proxy

// Round 5 H5 regression: an Anthropic tool whose allowed_callers excludes direct
// fails at the request boundary with 400 before any upstream send.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicAllowedCallersRejectedBeforeUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"},"allowed_callers":["programmatic"]}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "allowed_callers") {
		t.Fatalf("rejection does not name allowed_callers: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}
