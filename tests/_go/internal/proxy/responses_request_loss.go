package proxy

// Round 4 batch 6 regressions (A1, A2, A5): a dropped Responses request field
// under translation_loss:reject fails before any upstream call, and a
// programmatic-only tool is rejected even in warn mode.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesAudioLossRejectMode(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}},` +
		`{"type":"text","text":"listen"}]}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "content.input_audio") {
		t.Fatalf("rejection does not name the dropped audio part: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}

func TestResponsesToolKnobLossRejectMode(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","input":"hi","tools":[` +
		`{"type":"function","name":"lookup","parameters":{"type":"object"},"defer_loading":true}]}`
	w := proxyRequest(t, s, "/v1/responses", "openai", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tool.defer_loading") {
		t.Fatalf("rejection does not name the dropped tool knob: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}

func TestResponsesAccessProgramsLossRejectMode(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","input":"hi","access_programs":{"cyber":true}}`
	w := proxyRequest(t, s, "/v1/responses", "openai", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access_programs") {
		t.Fatalf("rejection does not name access_programs: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}

func TestResponsesProgrammaticOnlyToolRejectedBeforeUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","input":"hi","tools":[` +
		`{"type":"function","name":"lookup","parameters":{"type":"object"},"allowed_callers":["programmatic"]}]}`
	w := proxyRequest(t, s, "/v1/responses", "openai", body)
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
