package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// modelsBudgetPageBytes is large enough that two discovery pages cannot both
// fit the minimum configured response budget (1 MiB), while either page alone
// does. That makes a missing pre-fetch release fail admission on page two
// instead of merely accumulating a reservation the final defer would clear.
const modelsBudgetPageBytes = 600 << 10

func modelsBudgetPage(body []byte) []byte {
	if len(body) >= modelsBudgetPageBytes {
		return body
	}
	return append(body, bytes.Repeat([]byte{' '}, modelsBudgetPageBytes-len(body))...)
}

// TestModelsDiscoveryResponseBudgetReleasedThroughServeModels drives the real
// serveModels path (not a manual acquire/release) for one page and for a
// two-page discovery that exactly fits one page in the process-wide budget:
//
//   - the normal page must return 200 and leave respBudget at zero, so removing
//     serveModels' deferred final release fails;
//   - the second page must be admitted only because fetchModelsResponse
//     releases the previous page before fetching, so removing that early
//     release turns the two-page request into a retryable 503 instead of a 200.
func TestModelsDiscoveryResponseBudgetReleasedThroughServeModels(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		body := modelsBudgetPage([]byte(`{"data":[{"id":"model-one"}]}`))
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Write(body)
		}))
		t.Cleanup(upstream.Close)

		cfg := config.Default()
		cfg.MaxInflightResponseBytes = config.ByteSize(1 << 20)
		p := New(cfg, metrics.Noop{})
		req := modelsLimitRequest("openai")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "model-one") {
			t.Fatalf("response lost the model entry: %s", w.Body.String())
		}
		if got := p.respBudget.bytes.Load(); got != 0 {
			t.Fatalf("response budget after normal discovery = %d, want 0", got)
		}
	})

	t.Run("two pages finite budget", func(t *testing.T) {
		first := modelsBudgetPage([]byte(`{"data":[{"id":"model-one"}],"has_more":true,"last_id":"page-two"}`))
		second := modelsBudgetPage([]byte(`{"data":[{"id":"model-two"}],"has_more":false}`))
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("after_id") == "" {
				w.Header().Set("Content-Length", strconv.Itoa(len(first)))
				w.Write(first)
				return
			}
			if r.URL.Query().Get("after_id") != "page-two" {
				t.Errorf("unexpected after_id %q", r.URL.Query().Get("after_id"))
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(second)))
			w.Write(second)
		}))
		t.Cleanup(upstream.Close)

		cfg := config.Default()
		cfg.MaxInflightResponseBytes = config.ByteSize(1 << 20)
		cfg.ModelsDiscoveryMaxPages = 2
		p := New(cfg, metrics.Noop{})
		req := modelsLimitRequest("anthropic")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("two-page discovery status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if calls != 2 {
			t.Fatalf("upstream calls = %d, want 2", calls)
		}
		for _, want := range []string{"model-one", "model-two"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("two-page response lost %q: %s", want, w.Body.String())
			}
		}
		if got := p.respBudget.bytes.Load(); got != 0 {
			t.Fatalf("response budget after two-page discovery = %d, want 0", got)
		}
	})
}
