package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestExplorerScopeCounts(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("s1", now.Add(-2*time.Hour), 200, "", nil, 10, 100, 1, 1, 0, 0))
	s.Record(mkRec("s2", now.Add(-time.Hour), 500, "boom", nil, 10, 100, 1, 1, 0, 0))
	s.Record(mkRec("s3", now, 200, "", nil, 10, 100, 1, 1, 0, 0))
	waitTotals(t, s, 3)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	// No filters → every finalized request, including errors.
	p := getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")
	if p["matches"] != float64(3) {
		t.Fatalf("no-filter matches = %v, want 3", p["matches"])
	}
	if p["errors"] != float64(1) {
		t.Fatalf("no-filter errors = %v, want 1", p["errors"])
	}
	// Status class filter.
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider&f=status:5xx")
	if p["matches"] != float64(1) {
		t.Fatalf("status:5xx matches = %v, want 1", p["matches"])
	}
	// Error-signature filter (s2: error_type "boom", no error_code → code
	// "500", msg "" → key = "boom|500|").
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider&f=error:boom%7C500%7C")
	if p["matches"] != float64(1) {
		t.Fatalf("error signature matches = %v, want 1", p["matches"])
	}
	// AND across dims: provider p AND status 2xx.
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider&f=provider:p&f=status:2xx")
	if p["matches"] != float64(2) {
		t.Fatalf("provider+2xx matches = %v, want 2", p["matches"])
	}
	// Bad filter → 400.
	req := httptest.NewRequest(http.MethodGet, "/metrics/agg/explorer?dim=provider&f=nope:x", nil)
	rec := httptest.NewRecorder()
	http.HandlerFunc(api.HandleAggExplorer).ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad filter → %d, want 400", rec.Code)
	}
}

func TestExplorerScopeIncludesPending(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	now := time.Now()
	buf.Record(mkRec("f1", now, 200, "", nil, 0, 10, 1, 1, 0, 0))
	live := mkRec("live-1", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	live.Stream = true
	buf.PublishLive("begin", live)
	p := getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")
	if p["matches"] != float64(2) {
		t.Fatalf("matches = %v, want 2 (1 finalized + 1 pending)", p["matches"])
	}
}

// Footer counts keep every filter even when the gallery excludes its active
// dimension. Stored/ring overlap and an atomically finalized pending row must
// each count once, using their finalized error state.
func TestExplorerScopeCrossFilterAndDedupe(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(16)
	now := time.Now()
	stored := mkRec("stored", now, 200, "", nil, 0, 10, 1, 1, 0, 0)
	stored.Provider = "old.example"
	archived := mkRec("archived", now.Add(-24*time.Hour), 500, "boom", nil, 0, 10, 1, 1, 0, 0)
	archived.Provider = "new.example"
	unflushed := mkRec("unflushed", now, 502, "busy", nil, 0, 10, 1, 1, 0, 0)
	unflushed.Provider = "old.example"
	pending := mkRec("pending", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	pending.Provider, pending.Stream = "old.example", true
	buf.PublishLive("begin", pending)
	paused := mkRec("paused", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	paused.Provider, paused.Paused = "new.example", true
	buf.PublishLive("begin", paused)
	settling := mkRec("settling", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	settling.Provider, settling.Stream = "new.example", true
	buf.PublishLive("begin", settling)
	settling.StatusCode, settling.ErrorType = 500, "boom"
	// Production ownership order: ring retention runs before the durable
	// writer can read a record this test also stores.
	for _, rec := range []*metrics.Record{stored, unflushed, settling} {
		buf.Record(rec)
	}
	for _, rec := range []*metrics.Record{stored, archived, settling} {
		s.Record(rec)
	}
	waitTotals(t, s, 3)
	api := NewAggAPI(buf, s, time.Second)
	h := http.HandlerFunc(api.HandleAggExplorer)

	// Switching the breakdown dimension cannot change the full-scope footer.
	for dim := range validDims {
		t.Run(dim, func(t *testing.T) {
			p := get(t, h, "/metrics/agg/explorer?dim="+dim+"&f=provider:old.example")
			scope := p["scope"].(map[string]any)
			if scope["matches"] != float64(3) || scope["errors"] != float64(1) {
				t.Fatalf("scope = %v, want 3 matches / 1 error", scope)
			}
			wantTotal := float64(3)
			if dim == "provider" {
				wantTotal = 6 // only the gallery drops its active provider filter
			}
			if p["total"] != wantTotal {
				t.Fatalf("gallery total = %v, want %v", p["total"], wantTotal)
			}
		})
	}
	for _, tc := range []struct {
		query   string
		matches float64
		errors  float64
	}{
		{"dim=provider", 6, 3},
		{"dim=provider&f=provider:new.example", 3, 2},
		{"dim=status&f=status:5xx", 3, 3},
		{"dim=status&f=status:5xx&f=provider:old.example", 1, 1},
		{"dim=status&s=200", 1, 0},
		{"dim=status&s=500", 2, 2},
		{"dim=status&s=streaming", 1, 0},
		{"dim=status&s=paused", 1, 0},
		{"dim=error&f=error:boom%7C500%7C", 2, 2},
		{"dim=provider&f=provider:missing.example", 0, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			p := getExplorerScope(t, h, "/metrics/agg/explorer?"+tc.query)
			if p["matches"] != tc.matches || p["errors"] != tc.errors {
				t.Fatalf("scope = %v, want %v matches / %v errors", p, tc.matches, tc.errors)
			}
		})
	}

	// A flush only moves the durable ownership boundary, not the answer:
	// both before and after, the footer sees the same six rows. Buffer.Record
	// already retired the finalized row's pending state atomically.
	s.Record(unflushed)
	waitTotals(t, s, 4)
	p := getExplorerScope(t, h, "/metrics/agg/explorer?dim=provider")
	if p["matches"] != float64(6) || p["errors"] != float64(3) {
		t.Fatalf("post-flush scope = %v, want 6 matches / 3 errors", p)
	}
}

// The Requests-card dropdown can filter to live pills (s=streaming / paused)
// as well as exact HTTP codes. Named live filters must be accepted (not 400)
// and match only in-flight rows of that class.
func TestExplorerScopeLiveStatusFilter(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	now := time.Now()
	buf.Record(mkRec("f1", now, 200, "", nil, 0, 10, 1, 1, 0, 0))
	paused := mkRec("live-p", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	paused.Stream = true
	paused.Paused = true
	buf.PublishLive("begin", paused)
	stream := mkRec("live-s", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	stream.Stream = true
	buf.PublishLive("begin", stream)
	throttled := mkRec("live-t", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	throttled.Stream = true
	throttled.Throttled = true
	buf.PublishLive("begin", throttled)
	pending := mkRec("live-w", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	buf.PublishLive("begin", pending)

	p := getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=streaming")
	if p["matches"] != float64(1) {
		t.Fatalf("s=streaming matches = %v, want 1", p["matches"])
	}
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=paused")
	if p["matches"] != float64(1) {
		t.Fatalf("s=paused matches = %v, want 1", p["matches"])
	}
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=throttled")
	if p["matches"] != float64(1) {
		t.Fatalf("s=throttled matches = %v, want 1 (throttled outranks streaming, so the streaming row must not leak in)", p["matches"])
	}
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=pending")
	if p["matches"] != float64(1) {
		t.Fatalf("s=pending matches = %v, want 1 (a queued-not-streaming row is its own live class)", p["matches"])
	}
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=200")
	if p["matches"] != float64(1) {
		t.Fatalf("s=200 matches = %v, want 1", p["matches"])
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics/agg/explorer?dim=status&s=nope", nil)
	rec := httptest.NewRecorder()
	http.HandlerFunc(api.HandleAggExplorer).ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("s=nope → %d, want 400", rec.Code)
	}
}

// Regression: a RESTARTED process backfills the ring from the store with an
// EMPTY written set - unless primed, the aggregates double-count the whole
// ring. MarkWritten primes it (cmd layer calls this with the backfilled ids).
func TestBackfilledRingNotDoubleCounted(t *testing.T) {
	buf := metrics.NewBuffer(16)
	now := time.Now()
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		r := mkRec(string(rune('r'+i)), now.Add(-time.Duration(i+1)*time.Hour), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
		ids = append(ids, r.ID)
		buf.Record(r) // ring backfill, as cmd/proxy does at startup
	}
	s := testStore(t)
	api := NewAggAPI(buf, s, time.Second)
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if p["requests"] != float64(3) {
		t.Fatalf("ring-only = %v, want 3", p["requests"])
	}

	// Now make them durable + prime the written set (post-restart shape).
	for i := 0; i < 3; i++ {
		r := mkRec(string(rune('r'+i)), now.Add(-time.Duration(i+1)*time.Hour), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
		s.Record(r)
	}
	waitTotals(t, s, 3)
	s.MarkWritten(ids)
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if p["requests"] != float64(3) {
		t.Fatalf("store+primed-ring = %v, want 3 (no double count)", p["requests"])
	}
}

// Regression: All time must span the SCOPED history, not the ring -
// a minimal-column min scan once matched no explorer filters and the chart
// silently lost everything older than the ring.
func TestChartAllMinIsScoped(t *testing.T) {
	buf := metrics.NewBuffer(16)
	now := time.Now()
	// 3 provider-B rows in the recent window; 1 provider-A row 50h back.
	for i := 0; i < 3; i++ {
		r := mkRec(string(rune('b'+i)), now.Add(-time.Duration(i+1)*time.Minute), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
		r.Provider = "b"
		buf.Record(r)
	}
	ra := mkRec("a0", now.Add(-50*time.Hour), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
	ra.Provider = "a"
	buf.Record(ra)
	api := NewAggAPI(buf, nil, time.Second)
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=all&f=provider:b")
	from := int64(p["from_ms"].(float64))
	bucketMs := int64(p["bucket_ms"].(float64))
	if bucketMs == 0 {
		t.Fatal("bucket_ms missing/0")
	}
	if from%bucketMs != 0 {
		t.Fatalf("from_ms = %d not aligned to bucket_ms %d", from, bucketMs)
	}
	if from < now.Add(-10*time.Minute).UnixMilli() { // scoped min ≈ 3 min ago, never 50h
		t.Fatalf("scoped from_ms = %v, want ~now (provider A must be out of scope)", from)
	}
	// from = the min IN-SCOPE start floored onto a step boundary: never newer
	// than that start (flooring only moves it earlier).
	if from > now.Add(-3*time.Minute).UnixMilli() {
		t.Fatalf("from_ms = %d, want ≤ the oldest in-scope start (now-3m)", from)
	}
	var sum float64
	for _, b := range p["buckets"].([]any) {
		sum += b.(map[string]any)["req"].(float64)
	}
	if sum != 3 {
		t.Fatalf("scoped chart sum = %v, want 3", sum)
	}
}
