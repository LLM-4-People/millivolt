package proxy

// Round 2 correction NEW-1 and round 3 R3-1 proxy regressions: the Responses
// input preflight charges nested content/output parts before the codec
// materializes them, at exactly the shared aggregate boundary.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func responsesNestedPartsBody(counts []int) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[`)
	for i, parts := range counts {
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

func responsesNestedOutputPartsBody(counts []int) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[`)
	for i, parts := range counts {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"function_call_output","call_id":"c1","output":[`)
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

func nestedFixtureServer(t *testing.T) (*fixtureUpstream, *Server) {
	t.Helper()
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	return up, s
}

func TestResponsesNestedContentPartsAtLimitTranslate(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "message content", body: responsesNestedPartsBody([]int{4096, 4096, 4096, 4096})},
		{name: "function call output", body: responsesNestedOutputPartsBody([]int{4096, 4096, 4096, 4096})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if total := 4 * 4096; total != providerformat.WireContentPartsMax {
				t.Fatalf("fixture total = %d, want the shared limit %d", total, providerformat.WireContentPartsMax)
			}
			up, s := nestedFixtureServer(t)
			w := proxyRequest(t, s, "/v1/responses", "openai", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if up.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
			}
		})
	}
}

func TestResponsesNestedContentPartsOverLimitRejectedBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "message content", body: responsesNestedPartsBody([]int{4096, 4096, 4096, 4096, 1})},
		{name: "function call output", body: responsesNestedOutputPartsBody([]int{4096, 4096, 4096, 4096, 1})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, s := nestedFixtureServer(t)
			var w *httptest.ResponseRecorder
			alloc := proxyAllocDelta(func() {
				w = proxyRequest(t, s, "/v1/responses", "openai", tc.body)
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			want := fmt.Sprintf("request content parts exceed %d", providerformat.WireContentPartsMax)
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("rejection does not name the shared part budget: %s", w.Body.String())
			}
			if up.calls.Load() != 0 {
				t.Fatalf("upstream was called %d times before the nested-part rejection", up.calls.Load())
			}
			if alloc > 64<<20 {
				t.Fatalf("over-limit request allocated %d bytes, want a bounded <64 MiB", alloc)
			}
		})
	}
}
