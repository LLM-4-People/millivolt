package proxy

// Round 8 A5 proxy regression: an overlong Anthropic document title is a
// recorded translation loss, so reject mode answers 400 before any upstream
// send and names document.title.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicOverlongDocumentTitleRejected(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, anthropicStopFixtureResponse)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},` +
		`{"type":"file","file":{"filename":"` + strings.Repeat("a", 501) +
		`","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "document.title") {
		t.Fatalf("rejection does not name document.title: %s", w.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Fatalf("rejected title reached the upstream %d times", n)
	}
}
