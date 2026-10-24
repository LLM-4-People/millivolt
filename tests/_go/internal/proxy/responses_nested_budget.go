package proxy

// Round 2 correction NEW-1 proxy regressions: the Responses input preflight
// charges nested content/output parts and their annotations before the codec
// materializes them, so the r8j/r8l amplification bodies fail closed with no
// upstream send.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func responsesNestedPartsBody(items, parts int) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[`)
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"message","role":"user","content":[`)
		for j := 0; j < parts; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"input_text","text":"x"}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func responsesNestedAnnotationsBody(parts []int) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[`)
	for i, count := range parts {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"input_text","text":"hi","annotations":[`)
		for j := 0; j < count; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"url_citation","url":"https://example.com","title":"t","start_index":0,"end_index":1}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

func TestResponsesNestedContentPartsRejectedBeforeTranslation(t *testing.T) {
	body := responsesNestedPartsBody(5, 4096)
	if len(body) > 8<<20 {
		t.Fatalf("fixture is %d bytes, want a small bounded body", len(body))
	}
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "openai", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "request content parts exceed") {
		t.Fatalf("rejection does not name the part budget: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream was called %d times before the nested-part rejection", up.calls.Load())
	}
}

func TestResponsesNestedContentPartsAtLimitTranslate(t *testing.T) {
	body := responsesNestedPartsBody(2, 4096)
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "openai", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
	sent := string(up.lastBody())
	if !strings.Contains(sent, `"role":"user"`) {
		t.Fatalf("translated request is missing the user message: %s", sent[:min(len(sent), 200)])
	}
}

func TestResponsesNestedAnnotationsRejectedBeforeTranslation(t *testing.T) {
	body := responsesNestedAnnotationsBody([]int{4000, 4000, 4000})
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "openai", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "request annotations exceed") {
		t.Fatalf("rejection does not name the annotation budget: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream was called %d times before the annotation rejection", up.calls.Load())
	}
}
