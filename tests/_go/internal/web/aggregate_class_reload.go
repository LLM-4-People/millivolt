package web

import (
	"net/http"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestChartReclassifiesAfterRateLimitExtensionReload pins the reload witness:
// has429 derives from the installed classification vocabulary, not from a
// stored column, so a hot reload that adds a rate-limit spelling must rebuild
// the durable projection and invalidate the aggregate memo even though the
// database version did not change. A cache keyed only on the data version
// would keep serving the pre-reload RL series.
func TestChartReclassifiesAfterRateLimitExtensionReload(t *testing.T) {
	metrics.InstallClassExtensions(metrics.ClassExtensions{})
	t.Cleanup(func() { metrics.InstallClassExtensions(metrics.ClassExtensions{}) })

	s := testStore(t)
	now := time.Now()
	// A non-429 status carrying an operator-defined transient spelling. It is
	// not rate-limit classified until the extension is installed.
	s.Record(mkRec("ext1", now.Add(-10*time.Minute), 400, "gateway_throttle", nil, 0, 100, 0, 0, 0, 0))
	waitTotals(t, s, 1)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	rl := func() float64 {
		t.Helper()
		p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=60")
		var total float64
		for _, b := range p["buckets"].([]any) {
			total += b.(map[string]any)["rl"].(float64)
		}
		return total
	}

	if got := rl(); got != 0 {
		t.Fatalf("RL before the extension = %v, want 0", got)
	}
	// Warm the memo so the post-reload read cannot be explained by a cold fold.
	if got := rl(); got != 0 {
		t.Fatalf("warm RL before the extension = %v, want 0", got)
	}

	metrics.InstallClassExtensions(metrics.ClassExtensions{RateLimit: []string{"gateway_throttle"}})
	if got := rl(); got != 1 {
		t.Fatalf("RL after installing the extension = %v, want 1 (stale projection or memo)", got)
	}

	// Removing the extension is the same witness in the other direction.
	metrics.InstallClassExtensions(metrics.ClassExtensions{})
	if got := rl(); got != 0 {
		t.Fatalf("RL after removing the extension = %v, want 0 (stale projection or memo)", got)
	}
}
