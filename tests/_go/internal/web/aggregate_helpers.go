package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func testStore(t *testing.T) *storage.Store { return testStoreWithQueryTimeout(t, time.Second) }

// testStoreWithQueryTimeout opens the shared aggregate fixture with an explicit
// storage query budget. storage.withQueryTimeout wraps the handler context
// unconditionally, so the effective SELECT deadline is the smaller of the
// store's QueryTimeout and the aggregate's own timeout: a test that passes a
// wider budget to NewAggAPI gets it only when this fixture carries the same
// budget.
func testStoreWithQueryTimeout(t *testing.T, queryTimeout time.Duration) *storage.Store {
	t.Helper()
	d := config.Default()
	s, err := storage.Open(filepath.Join(t.TempDir(), "agg.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond, // fast drains so tests don't sleep long
		QueryTimeout:  queryTimeout,
		WriteTrackCap: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The two identity widths the browser regexes assert (live.js
// DASHBOARD_VERSION_RE and explorer.js MODEL_REVISION_RE): 16 lowercase hex
// for the stamped asset version (web.go etagHex) and 64 for the model-canon
// content revision (aggregate_observer.go sha256).
var (
	hexRe16 = regexp.MustCompile(`^[a-f0-9]{16}$`)
	hexRe64 = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// waitTotals polls until the store's durable totals reach n requests.
func waitTotals(t *testing.T, s *storage.Store, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Totals().Requests == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("totals = %d, want %d", s.Totals().Requests, n)
}

func mkRec(id string, start time.Time, status int, errType string, attempts []metrics.RetryAttempt, ttft, dur int64, in, out, cacheR int64, cost float64) *metrics.Record {
	return &metrics.Record{
		ID: id, Provider: "p", Model: "m", KeyHash: "k", Client: "cli",
		StatusCode: status, Start: start, End: start.Add(time.Duration(dur) * time.Millisecond),
		TTFTMs: ttft, DurationMs: dur, Cost: cost,
		ErrorType: errType, Attempts: attempts,
		Usage: metrics.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cacheR},
	}
}

func get(t *testing.T, h http.Handler, target string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s: status %d body=%s", target, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	return out
}

// getKPI fetches the bootstrap endpoint and returns its embedded KPI section
// (the ONE KPI surface - the standalone /metrics/agg route folded in).
func getKPI(t *testing.T, h http.Handler, target string) map[string]any {
	t.Helper()
	p := get(t, h, target)
	k, ok := p["kpi"].(map[string]any)
	if !ok {
		t.Fatalf("%s: kpi section missing: %v", target, p)
	}
	return k
}

// getExplorerScope reads the full-filter footer counts from the same response
// that carries the cross-filter gallery - no separate history scan.
func getExplorerScope(t *testing.T, h http.Handler, target string) map[string]any {
	t.Helper()
	p := get(t, h, target)
	scope, ok := p["scope"].(map[string]any)
	if !ok {
		t.Fatalf("%s: scope section missing: %v", target, p)
	}
	return scope
}

// kpiWireKeys is the ONE test-side owner of the KPI wire key set.
// TestBootstrapWireKeysStrict asserts the served bootstrap kpi section
// equals it, and the live.js EMPTY_KPI mirror pin (js_mirrors.go) asserts
// the dashboard's pre-aggregation fallback carries the same set, so the
// served decoder and the JS mirror can never drift apart.
var kpiWireKeys = []string{
	"requests", "errors", "in_flight", "cost",
	"cost_per_req", "cost_per_mtok",
	"input_tokens", "output_tokens", "cache_read_tokens", "reasoning_tokens",
	"avg_ttft_ms", "avg_tps",
}
