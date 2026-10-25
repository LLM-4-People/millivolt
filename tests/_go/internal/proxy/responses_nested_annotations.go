package proxy

// Round 3 R3-2 proxy regressions: one Responses annotation array keeps the
// codec's per-array limit and every annotation member shares the aggregate
// limit, all before translation or an upstream send.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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

func TestResponsesNestedAnnotationArrayBoundary(t *testing.T) {
	t.Run("at limit", func(t *testing.T) {
		up, s := nestedFixtureServer(t)
		w := proxyRequest(t, s, "/v1/responses", "openai", responsesNestedAnnotationsBody([]int{metrics.ContentPartMax}))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if up.calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
		}
	})

	t.Run("over array limit", func(t *testing.T) {
		up, s := nestedFixtureServer(t)
		w := proxyRequest(t, s, "/v1/responses", "openai", responsesNestedAnnotationsBody([]int{metrics.ContentPartMax + 1}))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		want := fmt.Sprintf("request annotation array exceeds %d", metrics.ContentPartMax)
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("rejection does not name the per-array annotation budget: %s", w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream was called %d times before the annotation rejection", up.calls.Load())
		}
	})
}

func TestResponsesNestedAnnotationsAggregateBoundary(t *testing.T) {
	atLimit := []int{4096, 4096, 1808}
	if total := 4096 + 4096 + 1808; total != providerformat.WireNestedCollectionMax {
		t.Fatalf("fixture total = %d, want the shared aggregate %d", total, providerformat.WireNestedCollectionMax)
	}
	up, s := nestedFixtureServer(t)
	w := proxyRequest(t, s, "/v1/responses", "openai", responsesNestedAnnotationsBody(atLimit))
	if w.Code != http.StatusOK {
		t.Fatalf("at-limit status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("at-limit upstream calls = %d, want 1", up.calls.Load())
	}

	over := []int{4096, 4096, 1809}
	if total := 4096 + 4096 + 1809; total != providerformat.WireNestedCollectionMax+1 {
		t.Fatalf("fixture total = %d, want one over the shared aggregate %d", total, providerformat.WireNestedCollectionMax)
	}
	up, s = nestedFixtureServer(t)
	w = proxyRequest(t, s, "/v1/responses", "openai", responsesNestedAnnotationsBody(over))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over-limit status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	want := fmt.Sprintf("request annotations exceed %d", providerformat.WireNestedCollectionMax)
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("rejection does not name the aggregate annotation budget: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("over-limit upstream calls = %d, want 0", up.calls.Load())
	}
}
