package proxy

// Round 5 A1 regression: an unsupported Chat response_format is a translation
// loss, so reject mode fails before any upstream send and warn mode surfaces the
// warning header.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const chatGrammarFormatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"grammar"}}`

func TestChatResponseFormatRejectModeFailsClosed(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", chatGrammarFormatBody)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "response_format") {
		t.Fatalf("rejection does not name response_format: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}

func TestChatResponseFormatWarnModeSurfacesHeader(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK,
		`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", chatGrammarFormatBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(got, "response_format") {
		t.Fatalf("response_format warning missing from the header: %q", got)
	}
}
