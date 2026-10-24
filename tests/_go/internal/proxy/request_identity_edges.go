package proxy

// Round 4 batch 10 regressions (A3, A-U1, A-U2): an empty Chat target message
// list and an invalid response_format name fail before any upstream call, and
// a Responses instructions-only request still translates.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesEmptyInputRejectedByChatTarget(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	for _, input := range []string{`[]`, `""`} {
		w := proxyRequest(t, s, "/v1/responses", "openai", `{"model":"m","input":`+input+`}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("input %s status = %d, want 400 (body %s)", input, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "chat target requires") {
			t.Fatalf("input %s rejection does not name the empty messages: %s", input, w.Body.String())
		}
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}

func TestResponsesInstructionsOnlyProxy(t *testing.T) {
	const response = `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	up := newFixtureUpstream(t, "application/json", http.StatusOK, response)
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "openai", `{"model":"m","instructions":"be nice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	body := string(up.lastBody())
	if !strings.Contains(body, `"role":"system"`) || !strings.Contains(body, `"content":"be nice"`) {
		t.Fatalf("instructions did not become a Chat system message: %s", body)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("translated Responses body missing the answer: %s", w.Body.String())
	}
}

func TestResponseFormatNameRejectedBeforeUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	name := strings.Repeat("a", 65)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"` + name + `","schema":{"type":"object"}}}}`
	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "response_format.name") {
		t.Fatalf("rejection does not name response_format.name: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
}
