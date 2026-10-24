package proxy

// Round 7 A3 regression: an unrepresentable user tool call or reasoning block
// must fail closed in translation_loss:reject mode before any upstream call,
// instead of reaching the Responses wire as a silently shortened message.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesUserToolCallLossRejectMode(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hi"},` +
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}},` +
		`{"type":"thinking","thinking":"why","signature":"sig"}]}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai-responses", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	for _, field := range []string{"content.tool_call", "content.reasoning"} {
		if !strings.Contains(w.Body.String(), field) {
			t.Fatalf("rejection does not name %s: %s", field, w.Body.String())
		}
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}
