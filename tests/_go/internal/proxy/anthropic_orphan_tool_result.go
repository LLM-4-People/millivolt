package proxy

// Round 7 verifier NEW-2 regression: the orphan result drop is visible in the
// warning header in warn mode and fails closed in reject mode, before any
// upstream call.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicOrphanToolResultBody = `{"model":"m","messages":[` +
	`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
	`{"role":"tool","tool_call_id":"call_1","content":"result"},` +
	`{"role":"user","content":"next"}]}`

func TestAnthropicOrphanToolResultPolicy(t *testing.T) {
	t.Run("warn mode drops and reports", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, translatedAnthropicCompletion)
		cfg := config.Default()
		cfg.QualityRetries = 0
		cfg.ThinkingRetries = 0
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", anthropicOrphanToolResultBody)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if got := string(up.lastBody()); strings.Contains(got, `"tool_use_id"`) || !strings.Contains(got, `"next"`) {
			t.Fatalf("upstream body = %s, want the surviving user turn without the orphan result", got)
		}
		if got := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(got, "messages.orphan_tool_result") {
			t.Fatalf("warnings header = %q, want the orphan drop", got)
		}
	})

	t.Run("reject mode fails before upstream", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", http.StatusOK, translatedAnthropicCompletion)
		cfg := config.Default()
		cfg.TranslationLoss = config.TranslationLossReject
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", anthropicOrphanToolResultBody)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "messages.orphan_tool_result") {
			t.Fatalf("rejection does not name the orphan drop: %s", w.Body.String())
		}
		if got := up.calls.Load(); got != 0 {
			t.Fatalf("upstream calls = %d, want 0", got)
		}
	})
}
