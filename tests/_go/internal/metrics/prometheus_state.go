package metrics

// The published exposition is retained per finalized ring revision, so every
// ring mutation that changes what the exposition would say must invalidate it.
// This test drives the three mutations that rewrite ring contents - full-ring
// eviction (pushLocked overwrites the oldest slot), a filtered purge
// (removeWhereLocked), a fence purge (the durable Clear path) and Reset - and
// asserts the served body is exactly the from-scratch fold over the ring's
// current contents.
//
// The counters here are still a from-scratch Aggregate fold, not incremental
// tallies: an incremental counter would have to be maintained on every push,
// eviction and removal (adding a second owner beside the fold) while the exact
// TTFT sample set still needs the full snapshot and sort on every changed
// revision, so it would not remove the scrape's dominant cost. This test is the
// guard that the retained body and the fold cannot disagree after a mutation.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// prometheusScrapeBody serves one scrape and returns the body.
func prometheusScrapeBody(t *testing.T, buf *Buffer) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	w := httptest.NewRecorder()
	HandlePrometheus(w, req, buf)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics/prometheus = %d, want 200: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// assertServedMatchesFold requires the cached scrape to equal a fresh fold over
// the ring's current records.
func assertServedMatchesFold(t *testing.T, buf *Buffer, step string) {
	t.Helper()
	want, err := renderPrometheusExposition(buf.Snapshot())
	if err != nil {
		t.Fatalf("%s: from-scratch fold: %v", step, err)
	}
	if got := prometheusScrapeBody(t, buf); got != want {
		t.Fatalf("%s: served exposition differs from the from-scratch fold\ngot:\n%s\nwant:\n%s", step, got, want)
	}
}

func prometheusStateRecord(provider string, status int, ttft int64) *Record {
	return &Record{
		Provider: provider, StatusCode: status, TTFTMs: ttft,
		Usage: Usage{InputTokens: 3, OutputTokens: 5, CacheReadTokens: 2},
	}
}

func TestPrometheusExpositionMatchesFromScratchAfterRingMutations(t *testing.T) {
	buf := NewBuffer(4)
	for i, provider := range []string{"alpha", "beta", "alpha", "gamma"} {
		buf.Record(prometheusStateRecord(provider, 200, int64(10*(i+1))))
	}
	// Warm the retained body, then prove the three content rewrites.
	first := prometheusScrapeBody(t, buf)
	assertServedMatchesFold(t, buf, "warm")

	// Eviction: a full ring overwrites its oldest slot (alpha/10), so the served
	// total stays 4 while the provider set and sample set move.
	buf.Record(prometheusStateRecord("delta", 500, 500))
	buf.Record(prometheusStateRecord("delta", 200, 0))
	evicted := prometheusScrapeBody(t, buf)
	if evicted == first {
		t.Fatal("eviction did not change the exposition (the retained body is stale)")
	}
	if want := "llm_proxy_requests_total 4"; !containsLine(evicted, want) {
		t.Fatalf("evicted exposition does not report the full ring:\n%s", evicted)
	}
	assertServedMatchesFold(t, buf, "eviction")

	// Filtered purge: remove one provider's records through the same owner a
	// durable filtered purge uses.
	if removed := buf.RemoveWhere(func(r *Record) bool { return r.Provider == "delta" }); removed != 2 {
		t.Fatalf("filtered purge removed %d records, want 2", removed)
	}
	purged := prometheusScrapeBody(t, buf)
	if purged == evicted {
		t.Fatal("filtered purge did not change the exposition (the retained body is stale)")
	}
	assertServedMatchesFold(t, buf, "filtered purge")

	// Fence purge: everything finalized up to the boundary goes, which is the
	// ring side of a durable Clear.
	boundary := buf.RemovalBoundary()
	buf.Record(prometheusStateRecord("epsilon", 200, 900))
	if removed := buf.RemoveThrough(boundary, nil); removed != 2 {
		t.Fatalf("fence purge removed %d records, want 2 (the one after the boundary survives)", removed)
	}
	assertServedMatchesFold(t, buf, "fence purge")

	// Reset empties the ring and the cumulative counters.
	buf.Reset()
	reset := prometheusScrapeBody(t, buf)
	if want := "llm_proxy_requests_total 0"; !containsLine(reset, want) {
		t.Fatalf("reset exposition does not report an empty ring:\n%s", reset)
	}
	if hasMetricLine(reset, "llm_proxy_ttft_seconds_count") {
		t.Fatalf("reset exposition still publishes TTFT samples:\n%s", reset)
	}
	assertServedMatchesFold(t, buf, "reset")
}

// containsLine reports whether body contains want as a complete line.
func containsLine(body, want string) bool {
	for _, line := range strings.Split(body, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// hasMetricLine reports whether the exposition publishes a sample line for the
// named metric (the HELP/TYPE preamble lines start with "#").
func hasMetricLine(body, metric string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metric) {
			return true
		}
	}
	return false
}
