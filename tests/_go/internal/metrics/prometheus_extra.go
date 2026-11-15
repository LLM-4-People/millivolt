package metrics

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ring body is memoized by finalized revision, but plenty of state moves
// without a record (queue depth, drops, flush failures). The extra block must
// render on every scrape, after the ring body, while the ring body itself stays
// byte-identical: neither half may freeze the other.
func TestHandlePrometheusRendersExtraPerScrape(t *testing.T) {
	buf := NewBuffer(8)
	buf.Record(&Record{Provider: "alpha", StatusCode: 200})

	depth := 0
	extra := func(w io.Writer) {
		depth++
		fmt.Fprintf(w, "llm_proxy_storage_queue_depth %d\n", depth)
	}
	scrape := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
		w := httptest.NewRecorder()
		HandlePrometheus(w, req, buf, extra)
		if got, want := w.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d\n%s", got, want, w.Body.String())
		}
		return w.Body.String()
	}

	first := scrape()
	second := scrape()
	for i, body := range []string{first, second} {
		want := fmt.Sprintf("llm_proxy_storage_queue_depth %d\n", i+1)
		if !strings.Contains(body, want) {
			t.Fatalf("scrape %d is stale, missing %q:\n%s", i+1, want, body)
		}
		// The storage block is appended after the whole ring block.
		if strings.Index(body, "llm_proxy_requests_total 1\n") > strings.Index(body, want) {
			t.Fatalf("scrape %d put the storage block before the ring body:\n%s", i+1, body)
		}
	}
	// The ring half is still memoized: only the appended lines differ.
	cut := func(body string) string {
		i := strings.Index(body, "\nllm_proxy_storage_queue_depth")
		if i < 0 {
			t.Fatalf("missing storage separator:\n%s", body)
		}
		return body[:i]
	}
	if cut(first) != cut(second) {
		t.Fatalf("ring body rebuilt between unchanged scrapes:\nfirst:\n%s\nsecond:\n%s", cut(first), cut(second))
	}

	// A nil provider appends nothing (no dangling separator).
	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	w := httptest.NewRecorder()
	HandlePrometheus(w, req, buf, nil)
	if strings.HasSuffix(w.Body.String(), "\n\n") || strings.Contains(w.Body.String(), "llm_proxy_storage_") {
		t.Fatalf("nil provider changed the body:\n%q", w.Body.String())
	}
}
