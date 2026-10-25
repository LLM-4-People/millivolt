package proxy

// Round 7 A4 regression: an Anthropic block field the canonical model cannot
// carry must fail closed in translation_loss:reject mode before any upstream
// call, instead of being silently discarded.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicBlockFieldLossRejectMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{
			name: "image transformations",
			body: `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="},` +
				`"transformations":{"oversized_image":"error"}}]}]}`,
			field: "image.transformations",
		},
		{
			name: "tool_use caller",
			body: `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{},"caller":{"type":"server"}}]}]}`,
			field: "tool_use.caller",
		},
		{
			name: "tool_use toolset name",
			body: `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{},"toolset_name":"ts"}]}]}`,
			field: "tool_use.toolset_name",
		},
		{
			name: "tool_result toolset name",
			body: `{"model":"m","max_tokens":16,"messages":[` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"x","toolset_name":"ts"}]}]}`,
			field: "tool_result.toolset_name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
			cfg := config.Default()
			cfg.TranslationLoss = config.TranslationLossReject
			s := New(cfg, metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/messages", "openai", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.field) {
				t.Fatalf("rejection does not name %s: %s", tc.field, w.Body.String())
			}
			if up.calls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
			}
		})
	}
}
