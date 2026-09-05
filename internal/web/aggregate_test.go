package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fmt"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	d := config.Default()
	s, err := storage.Open(filepath.Join(t.TempDir(), "agg.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond, // fast drains so tests don't sleep long
		QueryTimeout:  time.Second,
		WriteTrackCap: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

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
		t.Fatalf("%s: status %d", target, rec.Code)
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

func TestKPIStoredPlusRingMerge(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("a", now.Add(-2*time.Hour), 200, "", nil, 500, 1000, 100, 50, 10, 0.02))
	s.Record(mkRec("b", now.Add(-time.Hour), 502, "api_error", nil, 0, 800, 200, 0, 0, 0.01))
	s.Record(mkRec("c", now.Add(-time.Minute), 200, "", []metrics.RetryAttempt{{StatusCode: 503}}, 300, 900, 150, 80, 150, 0.03))
	waitTotals(t, s, 3)

	buf := metrics.NewBuffer(16)
	// "d" lives only in the ring (never recorded to the store).
	buf.Record(mkRec("d", now, 200, "", nil, 100, 500, 50, 30, 0, 0.001))
	// "e" is in BOTH ring and store - must be counted exactly once.
	ee := mkRec("e", now.Add(-3*time.Hour), 200, "", nil, 600, 1200, 300, 200, 300, 0.05)
	s.Record(ee)
	buf.Record(ee)
	waitTotals(t, s, 4)

	api := NewAggAPI(buf, s, time.Second)
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	// Stored 4 (a,b,c,e) + ring-extra d; e deduped via the written set.
	if p["requests"] != float64(5) {
		t.Fatalf("requests = %v, want 5", p["requests"])
	}
	// Errors: b (502) + c (absorbed 503) - canonical IsError semantics.
	if p["errors"] != float64(2) {
		t.Fatalf("errors = %v, want 2", p["errors"])
	}
	if p["input_tokens"] != float64(800) { // 100+200+150+300+50
		t.Fatalf("input = %v, want 800", p["input_tokens"])
	}
	if p["output_tokens"] != float64(360) { // 50+0+80+30+200
		t.Fatalf("output = %v, want 360", p["output_tokens"])
	}
	if p["cache_read_tokens"] != float64(460) { // 10+0+150+0+300
		t.Fatalf("cache = %v, want 460", p["cache_read_tokens"])
	}
	if p["avg_ttft_ms"] != float64(375) { // 500+300+100+600 = 1500/4 (b has 0 → skipped)
		t.Fatalf("avg_ttft = %v, want 375", p["avg_ttft_ms"])
	}
	if p["in_flight"] != float64(0) {
		t.Fatalf("in_flight = %v, want 0", p["in_flight"])
	}
	// All 5 records reported cost → blended denominators = all requests and
	// their tokens: 0.111 over 5 reqs / 1160 in+out tokens.
	if got := p["cost_per_req"].(float64); math.Abs(got-0.111/5) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want %v", got, 0.111/5)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.111/1160*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.111/1160*1e6)
	}
}

// The durable fold (Totals.add via the writer, and the computeTotals rebuild
// it shares) must exclude 0-cost records from the blended denominators exactly
// like the ring fold does - an unpriced provider's tokens never dilute.
func TestKPIBlendedCostIgnoresUnpricedOnDurablePath(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("p1", now.Add(-time.Hour), 200, "", nil, 10, 100, 100, 50, 0, 0.02))
	s.Record(mkRec("u1", now.Add(-time.Minute), 200, "", nil, 10, 100, 1000, 50, 0, 0))
	waitTotals(t, s, 2)

	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, s, time.Second)
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.02) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want 0.02 (unpriced request must not enter the denominator)", got)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.02/150*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v (unpriced tokens must not enter the denominator)", got, 0.02/150*1e6)
	}
	// Raw totals still absorbed everything.
	if p["requests"] != float64(2) || p["input_tokens"] != float64(1100) {
		t.Fatalf("requests/input = %v/%v, want 2/1100", p["requests"], p["input_tokens"])
	}
}

func TestKPIInFlightLive(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	buf.PublishLive("begin", &metrics.Record{ID: "first"})
	buf.PublishLive("begin", &metrics.Record{ID: "second"})
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if p["in_flight"] != float64(2) {
		t.Fatalf("in_flight = %v, want 2", p["in_flight"])
	}
}

// The KPI band's unit-economics fields are server-derived from the folded
// totals: blended $/req and $/Mtok over the tokens of cost-REPORTING requests
// only - a 0-cost record (provider reported no price) must never dilute the
// rate. Null ("-") on a zero denominator, never a fake 0.
func TestKPICostUnitEconomics(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	now := time.Now()

	// Empty history: both denominators are 0 → null/null.
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if p["cost_per_req"] != nil || p["cost_per_mtok"] != nil {
		t.Fatalf("empty cost ratios = %v/%v, want null/null", p["cost_per_req"], p["cost_per_mtok"])
	}

	// $0.03 over 2 requests and 350 in+out tokens.
	buf.Record(mkRec("c1", now, 200, "", nil, 10, 100, 100, 50, 0, 0.02))
	buf.Record(mkRec("c2", now, 200, "", nil, 10, 100, 200, 0, 0, 0.01))
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.015) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want 0.015", got)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.03/350*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.03/350*1e6)
	}

	// A zero-token record adds cost but no tokens: cost_per_req re-blends,
	// cost_per_mtok keeps the 350-token denominator.
	buf.Record(mkRec("c3", now, 500, "boom", nil, 0, 0, 0, 0, 0, 0.5))
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.53/3) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want %v", got, 0.53/3)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.53/350*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.53/350*1e6)
	}

	// A zero-COST record (unpriced provider) adds requests and tokens but no
	// cost: it must stay out of BOTH blended denominators - $/req stays
	// 0.53/3 over the three cost-reporting requests, $/Mtok keeps their
	// 350-token denominator despite 1050 more tokens flowing through.
	buf.Record(mkRec("c4", now, 200, "", nil, 10, 100, 1000, 50, 0, 0))
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.53/3) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want %v (0-cost record must not enter the denominator)", got, 0.53/3)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.53/350*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v (0-cost tokens must not enter the denominator)", got, 0.53/350*1e6)
	}

	// All-history sanity: the raw totals DID absorb c4 (its 1050 tokens).
	if p["input_tokens"] != float64(100+200+1000) {
		t.Fatalf("input_tokens = %v, want 1300 (0-cost tokens still counted in raw totals)", p["input_tokens"])
	}

	// A history with ZERO tokens in total (single zero-token record, cost
	// still counts) keeps cost_per_req but nulls cost_per_mtok.
	buf2 := metrics.NewBuffer(16)
	api2 := NewAggAPI(buf2, nil, time.Second)
	buf2.Record(mkRec("z1", now, 500, "boom", nil, 0, 0, 0, 0, 0, 0.5))
	p = getKPI(t, http.HandlerFunc(api2.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want 0.5", got)
	}
	if p["cost_per_mtok"] != nil {
		t.Fatalf("cost_per_mtok = %v, want null (zero in+out tokens)", p["cost_per_mtok"])
	}
}

func TestChartBucketsAndPercentiles(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	// 4 requests 30 minutes ago, each with a known ttft (10..40ms) so the
	// bucket's p50 = 25, p95 = 38.5, p99 = 39.7 (linear interp). Identical
	// starts keep all four in ONE bucket wherever clock alignment puts the
	// edges; 30m back is inside the 60m window for any aligned from. Each
	// carries 6 cached-token reads → the bucket's cache fold = 24.
	for i, v := range []int64{10, 20, 30, 40} {
		r := mkRec(string(rune('t'+i)), now.Add(-30*time.Minute), 200, "", nil, v, 200, 10, 10, 6, 0)
		r.OverallTPS, r.DecodeTPS = float64(v)+0.5, 900 // overall wins, floats survive storage
		s.Record(r)
	}
	// 1 error 10 minutes ago: its bucket has 1 sample < pctMinSamples → the
	// ttft/tps triples must be all-null (suppression gate).
	errRec := mkRec("err1", now.Add(-10*time.Minute), 500, "boom", nil, 5, 100, 5, 0, 0, 0)
	errRec.DecodeTPS = 5.5 // absent overall speed uses the canonical decode fallback
	s.Record(errRec)
	waitTotals(t, s, 5)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	// with clock alignment `from` derives from the handler's time.Now(), so
	// assert RELATIVE invariants, never exact bucket indexes.
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=60")
	from := int64(p["from_ms"].(float64))
	bucketMs := int64(p["bucket_ms"].(float64))
	if bucketMs != (2 * time.Minute).Milliseconds() {
		t.Fatalf("bucket_ms = %d, want 120000 (60m window → 2m ladder step)", bucketMs)
	}
	if from%bucketMs != 0 {
		t.Fatalf("from_ms = %d not aligned to bucket_ms %d", from, bucketMs)
	}
	buckets := p["buckets"].([]any)
	if len(buckets) > chartMaxBuckets+1 {
		t.Fatalf("buckets = %d, want ≤ %d (cap + one alignment partial)", len(buckets), chartMaxBuckets+1)
	}
	var req, err float64
	var rich map[string]any // the bucket holding the 4 same-start records
	for i, b := range buckets {
		m := b.(map[string]any)
		// t values are bucket START edges: from_ms + i*bucket_ms.
		if int64(m["t"].(float64)) != from+int64(i)*bucketMs {
			t.Fatalf("bucket %d t = %v, want start edge %d", i, m["t"], from+int64(i)*bucketMs)
		}
		if _, ok := m["err_rate"]; ok {
			t.Fatal("err_rate must be gone from the chart payload (client recomputes)")
		}
		req += m["req"].(float64)
		err += m["err"].(float64)
		if m["req"].(float64) == 4 {
			rich = m
		} else if m["req"].(float64) > 0 {
			// A <pctMinSamples-sample bucket suppresses percentiles entirely.
			for _, k := range []string{"ttft", "tps"} {
				for _, x := range m[k].([]any) {
					if x != nil {
						t.Fatalf("bucket with %v requests has non-null %s: %v", m["req"], k, m[k])
					}
				}
			}
		}
	}
	if req != 5 || err != 1 {
		t.Fatalf("chart totals req=%v err=%v, want 5/1", req, err)
	}
	if rich == nil {
		t.Fatal("no bucket holds the 4 same-start records")
	}
	// p50 of [10,20,30,40] = 25; p95 = 38.5; p99 = 39.7 (linear interp).
	tt := rich["ttft"].([]any)
	if tt[0] == nil || tt[0].(float64) != 25 || tt[1].(float64) != 38.5 ||
		math.Abs(tt[2].(float64)-39.7) > 1e-9 {
		t.Fatalf("ttft pct = %v, want [25 38.5 39.7]", tt)
	}
	speed := rich["tps"].([]any)
	if speed[0] == nil || speed[0].(float64) != 25.5 || speed[1].(float64) != 39 || math.Abs(speed[2].(float64)-40.2) > 1e-9 {
		t.Fatalf("tps pct = %v, want [25.5 39 40.2]", speed)
	}
	if _, exists := rich["dur"]; exists {
		t.Fatal("removed duration series still serialized")
	}
	// cached prompt tokens fold per bucket (4 records × 6 reads = 24).
	if rich["cache"].(float64) != 24 {
		t.Fatalf("bucket cache = %v, want 24", rich["cache"])
	}
	// Period-wide percentile triples (the totals strip): all 5 samples clear
	// the ≥4 gate - ttft [5,10,20,30,40] → p50 20, p95 38, p99 39.6;
	// tps [5.5,10.5,20.5,30.5,40.5] → p50 20.5, p95 38.5, p99 40.1.
	tp := p["ttft_p"].([]any)
	if tp[0] == nil || tp[0].(float64) != 20 || tp[1].(float64) != 38 ||
		math.Abs(tp[2].(float64)-39.6) > 1e-9 {
		t.Fatalf("ttft_p = %v, want [20 38 39.6]", tp)
	}
	sp := p["tps_p"].([]any)
	if sp[0] == nil || sp[0].(float64) != 20.5 || sp[1].(float64) != 38.5 || math.Abs(sp[2].(float64)-40.1) > 1e-9 {
		t.Fatalf("tps_p = %v, want [20.5 38.5 40.1]", sp)
	}
	if _, exists := p["dur_p"]; exists {
		t.Fatal("removed period duration still serialized")
	}

	// Malformed windows are rejected at the API boundary.
	reqBad := httptest.NewRequest(http.MethodGet, "/metrics/agg/chart?window=1.5", nil)
	recBad := httptest.NewRecorder()
	http.HandlerFunc(api.HandleAggChart).ServeHTTP(recBad, reqBad)
	if recBad.Code != 400 {
		t.Fatalf("window 1.5 → %d, want 400", recBad.Code)
	}
}

// The chart bucket width comes from the nice clock-aligned ladder: the window
// start is floored onto a step boundary (from_ms % bucket_ms == 0), canonical
// windows map to their documented steps (60m → 2m, 24h → 1h), and spans
// beyond the fixed ladder grow whole-week steps to stay within the same cap.
func TestChartClockAlignedLadder(t *testing.T) {
	cases := []struct {
		span time.Duration
		want time.Duration
	}{
		{30 * time.Minute, time.Minute},
		{60 * time.Minute, 2 * time.Minute},
		{6 * time.Hour, 15 * time.Minute},
		{24 * time.Hour, time.Hour},
		{400 * 24 * time.Hour, 14 * 24 * time.Hour},
		{10 * 365 * 24 * time.Hour, 224 * 24 * time.Hour},
	}
	for _, c := range cases {
		got := time.Duration(chartStepMs(int64(c.span)/int64(time.Millisecond))) * time.Millisecond
		if got != c.want {
			t.Errorf("chartStepMs(%v) = %v, want %v", c.span, got, c.want)
		}
	}

	api := NewAggAPI(metrics.NewBuffer(16), nil, time.Second)
	for _, win := range []string{"15", "60", "360", "1440", "10080", "43200", "129600", "525600", strconv.FormatInt(chartMaxWindowMinutes, 10)} {
		p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window="+win)
		from := int64(p["from_ms"].(float64))
		bucketMs := int64(p["bucket_ms"].(float64))
		if bucketMs == 0 {
			t.Fatalf("window %s: bucket_ms missing/0", win)
		}
		if from%bucketMs != 0 {
			t.Fatalf("window %s: from_ms %d not aligned to bucket_ms %d", win, from, bucketMs)
		}
		if n := len(p["buckets"].([]any)); n > chartMaxBuckets+1 {
			t.Fatalf("window %s: %d buckets, want ≤ %d", win, n, chartMaxBuckets+1)
		}
		for i, b := range p["buckets"].([]any) {
			if int64(b.(map[string]any)["t"].(float64)) != from+int64(i)*bucketMs {
				t.Fatalf("window %s: bucket %d t is not a start edge", win, i)
			}
		}
	}
	if p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=60"); int64(p["bucket_ms"].(float64)) != (2 * time.Minute).Milliseconds() {
		t.Fatalf("60m bucket_ms = %v, want 2m", p["bucket_ms"])
	}
	if p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=1440"); int64(p["bucket_ms"].(float64)) != time.Hour.Milliseconds() {
		t.Fatalf("24h bucket_ms = %v, want 1h", p["bucket_ms"])
	}
}

// The API validates its numeric domain independently of the curated select.
// A valid custom minute range (90) is allowed without adding a second list;
// legacy labels, non-integers and unsafe arithmetic fail closed.
func TestChartWindowParamDomain(t *testing.T) {
	for _, v := range []string{"", "all", "1", "15", "90", "525600", strconv.FormatInt(chartMaxWindowMinutes, 10)} {
		r := httptest.NewRequest(http.MethodGet, "/metrics/agg/chart?window="+v, nil)
		got, ok := windowParam(r)
		if !ok || ((v == "" || v == "all") && got != 0) || (v != "" && v != "all" && strconv.Itoa(got) != v) {
			t.Errorf("windowParam(%q) = %d, %v", v, got, ok)
		}
	}
	api := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	for _, v := range []string{"session", "0", "-1", "1.5", "1e3", "015", "%2B15", "15%20", "ALL", "18446744073709551615", strconv.FormatInt(chartMaxWindowMinutes+1, 10)} {
		r := httptest.NewRequest(http.MethodGet, "/metrics/agg/chart?window="+v, nil)
		w := httptest.NewRecorder()
		api.HandleAggChart(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("window %q status = %d, want 400", v, w.Code)
		}
	}
}

// All reaches durable rows outside both the recent ring and a one-year
// range. Scoping and exact store/ring dedupe must survive the wider buckets.
func TestChartAllDurableHistoryBounded(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(4)
	now := time.Now()
	old := mkRec("old-in-scope", now.AddDate(-3, 0, 0), 200, "", nil, 20, 100, 10, 5, 0, 0.01)
	old.Provider = "history.example"
	other := mkRec("older-out-of-scope", now.AddDate(-10, 0, 0), 500, "failed", nil, 40, 200, 20, 10, 0, 0.02)
	other.Provider = "other.example"
	recent := mkRec("recent-stored", now.Add(-time.Hour), 200, "", nil, 60, 300, 30, 15, 0, 0.03)
	recent.Provider = old.Provider
	for _, r := range []*metrics.Record{old, other, recent} {
		s.Record(r)
	}
	waitTotals(t, s, 3)
	buf.Record(recent) // the ring copy is not a fourth durable contribution
	live := mkRec("not-flushed", now.Add(-time.Minute), 200, "", nil, 80, 400, 40, 20, 0, 0.04)
	live.Provider = old.Provider
	buf.Record(live)
	api := NewAggAPI(buf, s, time.Second)
	for _, c := range []struct {
		query  string
		oldest time.Time
		want   float64
	}{
		{"window=all", other.Start, 4},
		{"window=all&f=provider:history.example", old.Start, 3},
		{"window=525600", now.Add(-365 * 24 * time.Hour), 2},
	} {
		p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?"+c.query)
		from, step := int64(p["from_ms"].(float64)), int64(p["bucket_ms"].(float64))
		buckets := p["buckets"].([]any)
		if len(buckets) < 1 || len(buckets) > chartMaxBuckets+1 || from%step != 0 {
			t.Fatalf("%s: unbounded or unaligned geometry: from=%d step=%d count=%d", c.query, from, step, len(buckets))
		}
		if from > c.oldest.UnixMilli() || c.oldest.UnixMilli()-from >= step {
			t.Fatalf("%s: from=%d does not start with oldest scoped history %d", c.query, from, c.oldest.UnixMilli())
		}
		var sum float64
		for _, b := range buckets {
			sum += b.(map[string]any)["req"].(float64)
		}
		if sum != c.want {
			t.Fatalf("%s: req sum=%v, want %v", c.query, sum, c.want)
		}
	}
}

// A zero-traffic chart (nil store + empty ring - a fresh instance) still
// serves a CLOCK-ALIGNED ZERO-FILLED buckets array, never an empty one:
// newChartFold always allocates count ≥ 1 buckets and payload() emits every
// one. The client's blank-state gate (chartIsEmpty sums req across buckets)
// must treat that shape as empty, so the exact wire contract is pinned here -
// TestChartClockAlignedLadder only asserts upper bounds, which an empty
// buckets array would also satisfy.
func TestChartZeroTrafficPayloadShape(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(16), nil, time.Second)
	for _, win := range []string{"all", "15", "60", "360", "1440", "10080", "43200", "129600", "525600"} {
		p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window="+win)
		buckets := p["buckets"].([]any)
		if len(buckets) < 1 {
			t.Fatalf("window %s: zero-traffic buckets = %d, want >= 1 (zero-filled, never empty)", win, len(buckets))
		}
		if len(buckets) > chartMaxBuckets+1 {
			t.Fatalf("window %s: %d buckets, want <= %d", win, len(buckets), chartMaxBuckets+1)
		}
		from := int64(p["from_ms"].(float64))
		bucketMs := int64(p["bucket_ms"].(float64))
		if bucketMs == 0 || from%bucketMs != 0 {
			t.Fatalf("window %s: from_ms %d not aligned to bucket_ms %d", win, from, bucketMs)
		}
		for i, b := range buckets {
			m := b.(map[string]any)
			if int64(m["t"].(float64)) != from+int64(i)*bucketMs {
				t.Fatalf("window %s: bucket %d t is not a start edge", win, i)
			}
			if m["req"].(float64) != 0 || m["err"].(float64) != 0 || m["cost"].(float64) != 0 {
				t.Fatalf("window %s: zero-traffic bucket %d is not zero-filled: %v", win, i, m)
			}
			for _, k := range []string{"ttft", "tps"} {
				for _, x := range m[k].([]any) {
					if x != nil {
						t.Fatalf("window %s: zero-traffic bucket %d has non-null %s", win, i, k)
					}
				}
			}
		}
	}
}

func TestExplorerBreakdown(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("p1", now.Add(-2*time.Hour), 200, "", nil, 100, 500, 10, 5, 0, 0.01))
	s.Record(mkRec("p2", now.Add(-time.Hour), 200, "", nil, 200, 600, 20, 10, 0, 0.02))
	s.Record(mkRec("p3", now.Add(-time.Minute), 500, "boom", nil, 0, 700, 30, 15, 0, 0.03))
	s.Record(mkRec("p4", now, 200, "", []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "x"}}, 50, 800, 40, 20, 0, 0.04))
	waitTotals(t, s, 4)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")
	if p["total"] != float64(4) {
		t.Fatalf("total = %v, want 4", p["total"])
	}
	groups := p["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["name"] != "p" {
		t.Fatalf("provider groups = %v", groups)
	}
	g := groups[0].(map[string]any)
	if g["n"] != float64(4) || g["err_final"] != float64(2) { // p3 final 500 + p4 absorbed 502
		t.Fatalf("provider agg = %v", g)
	}
	// Per-entity blended $/Mtok (cost-reporting tokens only; all 4 records
	// report cost): $0.10 over 150 in+out tokens.
	if got := g["cost_per_mtok"].(float64); math.Abs(got-0.10/150*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.10/150*1e6)
	}
	rail := p["rail"].(map[string]any)
	if rail["provider"] != float64(1) || rail["status"] != float64(2) { // 2xx + 5xx
		t.Fatalf("rail = %v", rail)
	}

	// Error dimension: p3's final boom + p4's absorbed "x" are two signatures.
	pe := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=error")
	egroups := pe["groups"].([]any)
	if len(egroups) != 2 {
		t.Fatalf("error groups = %d, want 2", len(egroups))
	}
	for _, e := range egroups {
		m := e.(map[string]any)
		if m["err_events"] != float64(1) || m["n"] != float64(1) {
			t.Fatalf("error group = %v", m)
		}
	}

	// Status dimension classes.
	ps := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status")
	names := map[string]float64{}
	for _, e := range ps["groups"].([]any) {
		m := e.(map[string]any)
		names[m["name"].(string)] = m["n"].(float64)
	}
	if names["2xx"] != 3 || names["5xx"] != 1 {
		t.Fatalf("status groups = %v", names)
	}

	// Cross-filter: selecting Status→2xx must NOT collapse the gallery.
	px := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&f=status:2xx")
	if px["total"] != float64(4) {
		t.Fatalf("status self-filter total = %v, want 4 (all records)", px["total"])
	}
	xnames := map[string]float64{}
	for _, e := range px["groups"].([]any) {
		m := e.(map[string]any)
		xnames[m["name"].(string)] = m["n"].(float64)
	}
	if xnames["2xx"] != 3 || xnames["5xx"] != 1 {
		t.Fatalf("status self-filter groups = %v, want 2xx=3 5xx=1", xnames)
	}
	xrail := px["rail"].(map[string]any)
	if xrail["status"] != float64(1) {
		t.Fatalf("rail status after 2xx filter = %v, want 1 (full filter)", xrail)
	}
}

// The per-entity blended $/Mtok must exclude 0-cost records exactly like the
// KPI band's blended denominators do - an entity mixing priced and unpriced
// traffic (a client hitting both a priced provider and runinfra) must not
// have its rate diluted by the unpriced tokens.
func TestExplorerBlendedCostIgnoresUnpriced(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("pr", now.Add(-time.Hour), 200, "", nil, 10, 100, 100, 50, 0, 0.02))
	s.Record(mkRec("un", now.Add(-time.Minute), 200, "", nil, 10, 100, 1000, 50, 0, 0))
	waitTotals(t, s, 2)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=client")
	groups := p["groups"].([]any)
	if len(groups) != 1 { // mkRec stamps Client "cli" on both
		t.Fatalf("client groups = %d, want 1", len(groups))
	}
	g := groups[0].(map[string]any)
	if g["n"] != float64(2) || g["in"] != float64(1100) {
		t.Fatalf("client agg = %v (raw totals must absorb the unpriced tokens)", g)
	}
	// Denominator = the priced record's 150 tokens only.
	if got := g["cost_per_mtok"].(float64); math.Abs(got-0.02/150*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v (unpriced tokens must not dilute)", got, 0.02/150*1e6)
	}

	// An entity with NO cost-reporting requests at all → null, never a fake 0.
	s2 := testStore(t)
	s2.Record(mkRec("u1", now, 200, "", nil, 10, 100, 100, 50, 0, 0))
	waitTotals(t, s2, 1)
	api2 := NewAggAPI(metrics.NewBuffer(16), s2, time.Second)
	p2 := get(t, http.HandlerFunc(api2.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")
	g2 := p2["groups"].([]any)[0].(map[string]any)
	if g2["cost_per_mtok"] != nil {
		t.Fatalf("cost_per_mtok = %v, want null (no cost-reporting requests)", g2["cost_per_mtok"])
	}
}

func TestExplorerRejectsBadDim(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(8), nil, time.Second)
	req := httptest.NewRequest(http.MethodGet, "/metrics/agg/explorer?dim=provider;DROP", nil)
	rec := httptest.NewRecorder()
	http.HandlerFunc(api.HandleAggExplorer).ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestStatusClassAndBuckets(t *testing.T) {
	cases := map[int]string{200: "2xx", 299: "2xx", 499: "cancel", 429: "4xx", 404: "4xx", 500: "5xx", 503: "5xx", 0: "err", 302: "err"}
	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
	sat := time.Date(2026, 8, 22, 12, 0, 0, 0, time.Local) // Saturday
	if got := metrics.TimeBucket(sat); got != "weekend" {
		t.Errorf("Saturday → %q, want weekend", got)
	}
	wed := time.Date(2026, 8, 12, 9, 0, 0, 0, time.Local)
	if got := metrics.TimeBucket(wed); got != "work" {
		t.Errorf("Wed 09:00 → %q, want work", got)
	}
	if got := metrics.TimeBucket(wed.Add(8 * time.Hour)); got != "evening" {
		t.Errorf("Wed 17:00 → %q, want evening", got)
	}
	if got := metrics.TimeBucket(wed.Add(-3 * time.Hour)); got != "night" {
		t.Errorf("Wed 06:00 → %q, want night", got)
	}
	if got := contribStatusClass(&contrib{live: true, paused: true, stream: true}); got != "paused" {
		t.Errorf("live paused = %q, want paused", got)
	}
	if got := contribStatusClass(&contrib{live: true, stream: true}); got != "streaming" {
		t.Errorf("live stream = %q, want streaming", got)
	}
	if got := contribStatusClass(&contrib{live: true}); got != "pending" {
		t.Errorf("live pending = %q, want pending", got)
	}
	if got := contribStatusClass(&contrib{status: 0, stream: true}); got != "err" {
		t.Errorf("finalized stream status 0 = %q, want err (not streaming)", got)
	}
}

func TestExplorerLiveStatusClasses(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	now := time.Now()
	paused := mkRec("live-p", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	paused.Stream = true
	paused.Paused = true
	buf.PublishLive("begin", paused)
	stream := mkRec("live-s", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	stream.Stream = true
	buf.PublishLive("begin", stream)

	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status")
	names := map[string]float64{}
	for _, e := range p["groups"].([]any) {
		m := e.(map[string]any)
		names[m["name"].(string)] = m["n"].(float64)
	}
	if names["paused"] != 1 || names["streaming"] != 1 {
		t.Fatalf("status groups = %v, want paused=1 streaming=1", names)
	}
	if p["error_total"] != float64(0) {
		t.Fatalf("error_total = %v, want 0 (live rows are not errors)", p["error_total"])
	}
}

func TestErrorEntriesSemantics(t *testing.T) {
	// 429 final = no error entry (flow control); 499 = none (client cancel).
	r := &metrics.Record{StatusCode: 499, Start: time.Now()}
	if n := len(errorEntries(r)); n != 0 {
		t.Fatalf("499 → %d entries, want 0", n)
	}
	// A provider in-band error on 200 IS an entry.
	r = &metrics.Record{StatusCode: 200, ErrorType: "provider_overloaded", ErrorMsg: "x", Start: time.Now()}
	ents := errorEntries(r)
	if len(ents) != 1 || ents[0].typ != "provider_overloaded" {
		t.Fatalf("in-band → %v", ents)
	}
	// Absorbed 429 skipped; a typed transport attempt kept; a bare <400 attempt
	// skipped; a 401 and a typed 5xx kept (client recordErrorEntries parity).
	r = &metrics.Record{StatusCode: 200, Attempts: []metrics.RetryAttempt{
		{StatusCode: 429},
		{StatusCode: 502, ErrorType: "upstream"},
		{StatusCode: 0, ErrorType: "dial_failed"},
		{StatusCode: 401},
		{StatusCode: 302}, // bare <400 → skipped
	}, Start: time.Now()}
	ents = errorEntries(r)
	if len(ents) != 3 || !ents[0].absorbed {
		t.Fatalf("absorbed → %v", ents)
	}
	for i, wantType := range []string{"upstream", "dial_failed", "http_401"} {
		if ents[i].typ != wantType {
			t.Fatalf("ents[%d].typ = %q, want %q (%v)", i, ents[i].typ, wantType, ents)
		}
	}
	// 5xx final status.
	r = &metrics.Record{StatusCode: 500, Start: time.Now()}
	ents = errorEntries(r)
	if len(ents) != 1 || ents[0].typ != "http_500" {
		t.Fatalf("final 5xx → %v", ents)
	}
}

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
	for _, rec := range []*metrics.Record{stored, archived, settling} {
		s.Record(rec)
	}
	for _, rec := range []*metrics.Record{stored, unflushed, settling} {
		buf.Record(rec)
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

	p := getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=streaming")
	if p["matches"] != float64(1) {
		t.Fatalf("s=streaming matches = %v, want 1", p["matches"])
	}
	p = getExplorerScope(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&s=paused")
	if p["matches"] != float64(1) {
		t.Fatalf("s=paused matches = %v, want 1", p["matches"])
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

func TestChartAllMinStoredAndUnflushed(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	for _, c := range []struct {
		id       string
		age      time.Duration
		provider string
	}{
		{"newest", 10 * time.Minute, "history.example"},
		{"unmatched", 3 * time.Hour, "other.example"},
		{"oldest-match", time.Hour, "history.example"},
	} {
		r := mkRec(c.id, now.Add(-c.age), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
		r.Provider = c.provider
		s.Record(r)
	}
	waitTotals(t, s, 3)
	buf := metrics.NewBuffer(4)
	api := NewAggAPI(buf, s, time.Second)
	fs := []scopeFilter{{dim: "provider", id: "history.example"}}
	assertMin := func(want int64) {
		t.Helper()
		got, err := minStartScoped(t.Context(), api, api.canonizer(), fs, "", now.UnixMilli())
		if err != nil || got != want {
			t.Fatalf("minStartScoped = %d, %v; want %d", got, err, want)
		}
	}
	assertMin(now.Add(-time.Hour).UnixMilli())
	for _, age := range []time.Duration{time.Minute, 2 * time.Hour} {
		r := mkRec(age.String(), now.Add(-age), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
		r.Provider = "history.example"
		buf.Record(r)
	}
	assertMin(now.Add(-2 * time.Hour).UnixMilli())
	missing := []scopeFilter{{dim: "provider", id: "missing.example"}}
	if got, err := minStartScoped(t.Context(), api, api.canonizer(), missing, "", now.UnixMilli()); err != nil || got != now.UnixMilli() {
		t.Fatalf("empty scope min = %d, %v; want now", got, err)
	}
	// A failed min lookup must never become a successful recent-only chart.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, filters := range [][]scopeFilter{nil, fs} {
		if _, err := minStartScoped(ctx, api, api.canonizer(), filters, "", now.UnixMilli()); err == nil {
			t.Fatal("canceled min lookup lost its query error")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/metrics/agg/chart?window=all&f=provider:history.example", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	api.HandleAggChart(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("canceled chart = %d, want 500", w.Code)
	}
}

func TestPurgeRebuildsTotals(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("p1", now, 200, "", nil, 100, 500, 10, 5, 0, 0.01))
	s.Record(mkRec("p2", now, 500, "boom", nil, 100, 500, 10, 5, 0, 0.01))
	waitTotals(t, s, 2)
	if s.Totals().Errors != 1 {
		t.Fatalf("errors = %d, want 1", s.Totals().Errors)
	}
	if _, err := s.PurgeWhere(context.Background(), storage.PurgeFilter{Provider: "p"}); err != nil {
		t.Fatal(err)
	}
	if s.Totals().Requests != 0 || s.Totals().Errors != 0 {
		t.Fatalf("post-purge totals = %+v, want zero", s.Totals())
	}
}

func TestHandleLogPage(t *testing.T) {
	s := testStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	for i := 0; i < 25; i++ {
		r := mkRec("l"+strconv.Itoa(i), base.Add(time.Duration(i)*time.Second), 200, "", nil, 10, 100, 1, 1, 0, 0)
		if i%5 == 0 {
			r.Provider = "other"
		}
		s.Record(r)
	}
	waitTotals(t, s, 25)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)
	h := http.HandlerFunc(api.HandleLogPage)

	code := func(t *testing.T, method, target string) int {
		t.Helper()
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := code(t, http.MethodGet, "/metrics/agg/log"); got != 400 {
		t.Fatalf("missing before_ms → %d, want 400", got)
	}
	if got := code(t, http.MethodGet, "/metrics/agg/log?before_ms=nope"); got != 400 {
		t.Fatalf("bad before_ms → %d, want 400", got)
	}
	if got := code(t, http.MethodGet, "/metrics/agg/log?before_ms=0"); got != 400 {
		t.Fatalf("before_ms=0 → %d, want 400", got)
	}
	if got := code(t, http.MethodGet, "/metrics/agg/log?before_ms=1"); got != 400 {
		t.Fatalf("missing limit → %d, want 400", got)
	}
	if got := code(t, http.MethodGet, "/metrics/agg/log?before_ms=1&limit=5"); got != 400 {
		t.Fatalf("limit 5 → %d, want 400", got)
	}
	post := httptest.NewRequest(http.MethodPost, "/metrics/agg/log?before_ms=1", nil)
	postRec := httptest.NewRecorder()
	h.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST → %d, want 405", postRec.Code)
	}
	if postRec.Header().Get("Allow") != aggAllow {
		t.Fatalf("POST Allow = %q, want %q", postRec.Header().Get("Allow"), aggAllow)
	}
	kpiPost := httptest.NewRecorder()
	http.HandlerFunc(api.HandleBootstrap).ServeHTTP(kpiPost, httptest.NewRequest(http.MethodPost, "/metrics/bootstrap", nil))
	if kpiPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics/bootstrap → %d, want 405", kpiPost.Code)
	}
	if kpiPost.Header().Get("Allow") != aggAllow {
		t.Fatalf("POST /metrics/bootstrap Allow = %q, want %q", kpiPost.Header().Get("Allow"), aggAllow)
	}

	// 24 rows older than l24; first page is newest-first and exclusive of the cursor.
	before := base.Add(24 * time.Second).UnixMilli()
	p := get(t, h, "/metrics/agg/log?before_ms="+strconv.FormatInt(before, 10)+"&before_id=&limit=10")
	recs, _ := p["records"].([]any)
	if len(recs) != 10 {
		t.Fatalf("page len=%d, want 10", len(recs))
	}
	if recs[0].(map[string]any)["id"] != "l23" {
		t.Fatalf("first id=%v, want l23", recs[0].(map[string]any)["id"])
	}
	if recs[9].(map[string]any)["id"] != "l14" {
		t.Fatalf("last id=%v, want l14", recs[9].(map[string]any)["id"])
	}
	if p["more"] != true {
		t.Fatalf("more=%v, want true", p["more"])
	}

	cursor := int64(p["cursor_ms"].(float64))
	p2 := get(t, h, "/metrics/agg/log?before_ms="+strconv.FormatInt(cursor, 10)+"&before_id="+p["cursor_id"].(string)+"&limit=10")
	recs2 := p2["records"].([]any)
	if recs2[0].(map[string]any)["id"] != "l13" {
		t.Fatalf("page2 first id=%v, want l13 (no overlap)", recs2[0].(map[string]any)["id"])
	}

	// Scope: every 5th row is provider "other"; the page must only contain p.
	p3 := get(t, h, "/metrics/agg/log?before_ms="+strconv.FormatInt(base.Add(25*time.Second).UnixMilli(), 10)+"&before_id=&limit=10&f=provider:p")
	got := p3["records"].([]any)
	if len(got) != 10 {
		t.Fatalf("scoped page=%d, want 10", len(got))
	}
	for _, row := range got {
		if row.(map[string]any)["provider"] != "p" {
			t.Fatalf("scoped row provider=%v", row.(map[string]any)["provider"])
		}
	}

	api2 := NewAggAPI(metrics.NewBuffer(16), nil, time.Second)
	p4 := get(t, http.HandlerFunc(api2.HandleLogPage), "/metrics/agg/log?limit=10")
	if recs, _ := p4["records"].([]any); len(recs) != 0 {
		t.Fatalf("nil store records=%v", p4["records"])
	}
	if p4["more"] != false {
		t.Fatalf("nil store more=%v", p4["more"])
	}
}

// TestBootstrapSnapshotAndState: the boot payload carries the ring snapshot
// (capped to the newest window when the snapshot limit is set; a cursor
// resume yields only the delta; a foreign feed degrades to full), the global
// KPI aggregate, and omits the optional operator sections when no provider
// is wired. The log scope is client-side - the endpoint takes no scope
// parameters, so a filter never trims the record set.
func TestBootstrapSnapshotAndState(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(64)
	now := time.Now()
	for _, rec := range []*metrics.Record{
		mkRec("a", now.Add(-3*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0.5),
		mkRec("b", now.Add(-2*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0.5),
		mkRec("c", now.Add(-1*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0.5),
	} {
		s.Record(rec)
		buf.Record(rec)
	}
	waitTotals(t, s, 3)
	agg := NewAggAPI(buf, s, time.Second)
	// Cap the full snapshot (main.go derives 8×dash_log_rows; this pins the
	// cap mechanics to a tiny ring).
	buf.SetSnapshotLimit(2)

	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	index := httptest.NewRecorder()
	Handler(agg).ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if p["dashboard_version"] != dashboardVersion || !strings.Contains(index.Body.String(), `<meta name="dashboard-version" content="`+dashboardVersion+`">`) {
		t.Fatalf("full bootstrap asset version=%v does not match served HTML", p["dashboard_version"])
	}
	snap, ok := p["records"].([]any)
	if !ok {
		t.Fatalf("records missing: %v", p)
	}
	if len(snap) != 2 {
		t.Fatalf("records = %d, want 2 (capped to the newest window)", len(snap))
	}
	if _, ok := p["kpi"]; !ok {
		t.Fatal("kpi section missing")
	}
	if v, ok := p["kpi"].(map[string]any); !ok || v["requests"].(float64) != 3 {
		t.Fatalf("kpi = %v, want global 3 requests", p["kpi"])
	}
	for _, key := range []string{"dash", "pause", "throttle", "debug"} {
		if _, ok := p[key]; ok {
			t.Errorf("%s section present with no provider wired, want omitted", key)
		}
	}
	if p["feed_id"] == "" || p["seq"].(float64) != 3 {
		t.Fatalf("cursor metadata missing: seq=%v feed=%v", p["seq"], p["feed_id"])
	}

	// Wired providers ride the payload.
	agg.Dash = func() any { return map[string]any{"dash_log_rows": 10} }
	agg.Pause = func() any { return map[string]any{"paused": false} }
	agg.Throttle = func() any { return map[string]any{"active": false} }
	agg.Debug = func() any { return map[string]any{"enabled": false} }
	p2 := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	for _, key := range []string{"dash", "pause", "throttle", "debug"} {
		if _, ok := p2[key]; !ok {
			t.Errorf("%s section missing with a provider wired", key)
		}
	}
	agg.Dash, agg.Pause, agg.Throttle, agg.Debug = nil, nil, nil, nil

	// Cursor resume: ?since=2&feed=<own> yields only the delta.
	p3 := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap?since=2&feed="+buf.FeedID())
	if p3["dashboard_version"] != p["dashboard_version"] {
		t.Fatalf("resume asset version=%v, full=%v", p3["dashboard_version"], p["dashboard_version"])
	}
	if p3["incremental"] != true {
		t.Fatalf("resume payload incremental = %v, want true", p3["incremental"])
	}
	if n := len(p3["records"].([]any)); n != 1 {
		t.Fatalf("resume records = %d, want 1 (c)", n)
	}

	// Foreign feed forces a full snapshot (deny by default), uncapped delta
	// semantics intact.
	p4 := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap?since=2&feed=deadbeef")
	if p4["incremental"] == true {
		t.Fatal("foreign feed yielded an incremental payload, want full")
	}

	// Unknown query params are ignored (the endpoint takes no scope): a bogus
	// filter value must neither 4xx nor trim the snapshot.
	p5 := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap?f=bogus:x&s=99999")
	if n := len(p5["records"].([]any)); n != 2 {
		t.Fatalf("bogus-scope records = %d, want 2 (unscoped)", n)
	}
}

// TestBootstrapUnscopedShowsEverything: without filters the snapshot is the
// uncapped-when-unlimited, unscoped record window.
func TestBootstrapUnscopedShowsEverything(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(64)
	now := time.Now()
	for _, rec := range []*metrics.Record{
		mkRec("a", now.Add(-2*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
		mkRec("b", now.Add(-1*time.Second), 500, "upstream", nil, 10, 100, 10, 5, 0, 0),
	} {
		s.Record(rec)
		buf.Record(rec)
	}
	waitTotals(t, s, 2)
	agg := NewAggAPI(buf, s, time.Second)

	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	if len(p["records"].([]any)) != 2 {
		t.Fatalf("records = %d, want 2", len(p["records"].([]any)))
	}
}

// TestExplorerModelDimensionCanonicalized: the model dimension groups by the
// CANONICAL spelling (config.CanonicalModel applied at the contrib choke
// point), and a canonical model filter matches every raw variant - the
// server-side half of the contract the client mirrors in recordMatchesDim.
func TestExplorerModelDimensionCanonicalized(t *testing.T) {
	buf := metrics.NewBuffer(16)
	now := time.Now()
	for _, rec := range []*metrics.Record{
		mkRec("a", now.Add(-3*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
		mkRec("b", now.Add(-2*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
		mkRec("c", now.Add(-1*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
	} {
		buf.Record(rec)
	}
	// Three raw spellings of two models: glm-5.3 twice + glm-5-3 once (same
	// canonical group), moonshotai/kimi-k3:nube once.
	buf.Snapshot()[0].Model = "glm-5.3"
	buf.Snapshot()[1].Model = "glm-5.3"
	buf.Snapshot()[2].Model = "glm-5-3"

	agg := NewAggAPI(buf, nil, time.Second)
	agg.ModelCanon = func() config.ModelCanon { return config.Default().ModelCanon() }

	p := get(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model")
	groups := p["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("model groups = %d, want 1 (all three spellings canonicalize to glm-5-3)", len(groups))
	}
	g := groups[0].(map[string]any)
	if g["name"] != "glm-5-3" || g["n"] != float64(3) {
		t.Fatalf("group = %v, want name=glm-5-3 n=3", g)
	}
	if rail := p["rail"].(map[string]any); rail["model"] != float64(1) {
		t.Fatalf("rail model count = %v, want 1 canonical group", rail["model"])
	}

	// A canonical model filter matches every raw variant of the group.
	ps := getExplorerScope(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model&f=model:glm-5-3")
	if ps["matches"] != float64(3) {
		t.Fatalf("scoped matches = %v, want 3 (both raw spellings match the canonical filter)", ps["matches"])
	}
}

// TestBootstrapCarriesModelCanon: the bootstrap payload ships the effective
// rules and server-derived name dictionary so clients need no regex mirror.
// An absent provider explicitly supplies identity semantics.
func TestBootstrapCarriesModelCanon(t *testing.T) {
	buf := metrics.NewBuffer(4)
	agg := NewAggAPI(buf, nil, time.Second)
	agg.ModelCanon = func() config.ModelCanon {
		return config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	}
	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	mc, ok := p["model_canon"].(map[string]any)
	if !ok {
		t.Fatalf("model_canon section = %v, want present", p["model_canon"])
	}
	rules, ok := mc["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("model_canon rules = %v, want the provider's single lower rule", mc["rules"])
	}
	if rule := rules[0].(map[string]any); rule["mode"] != "lower" {
		t.Fatalf("model_canon rules = %v, want mode lower", rules)
	}

	agg2 := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	p2 := get(t, http.HandlerFunc(agg2.HandleBootstrap), "/metrics/bootstrap")
	if mc, ok := p2["model_canon"].(map[string]any); !ok || mc["revision"] == "" {
		t.Fatal("raw grouping requires explicit observer identity")
	}
}

// TestCanonicalModelScopeAppliesEverywhere: the canonical model name is
// applied at the LOWEST server read choke point - contrib construction
// (fromRecord/fromPending/scanContrib all run the modelCanonizer) - so
// every grouped/matched surface downstream of it agrees: a scope filter
// carrying the CANONICAL spelling (what a node-card or saved hash sends)
// must match records stored under RAW spellings. This pins the
// STORE-backed paths (scanContrib): explorer grouping over the store,
// scope counts, chart buckets, and log paging - the ring path
// (fromRecord) is pinned by TestExplorerModelDimensionCanonicalized.
func TestCanonicalModelScopeAppliesEverywhere(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(16)
	// Scoping covers the whole requested window, not just the newest bucket.
	// Keep this fixture away from the current bucket deterministically.
	now := time.Now().Add(-5 * time.Minute)
	raw := []string{"glm-5.3", "glm-5-3", "GLM-5.3", "grok-4.6"}
	for i, m := range raw {
		rec := mkRec(fmt.Sprintf("r%d", i), now.Add(-time.Duration(i+1)*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0)
		rec.Model = m
		s.Record(rec)
		buf.Record(rec)
	}
	waitTotals(t, s, 4)
	agg := NewAggAPI(buf, s, time.Second)
	agg.ModelCanon = func() config.ModelCanon { return config.Default().ModelCanon() }

	// The store scan itself groups canonically: one glm-5-3 card, not three.
	p := get(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model")
	groups := p["groups"].([]any)
	if len(groups) != 2 || groups[0].(map[string]any)["name"] != "glm-5-3" {
		t.Fatalf("store explorer groups = %v, want 2 with glm-5-3 merged first", groups)
	}

	// Scope: canonical filter matches all three raw spellings (scanContrib).
	ps := getExplorerScope(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model&f=model:glm-5-3")
	if ps["matches"] != float64(3) {
		t.Fatalf("scoped matches = %v, want 3 over the store", ps["matches"])
	}

	// Chart: the scoped bucket count folds the raw-spelled rows.
	pc := get(t, http.HandlerFunc(agg.HandleAggChart), "/metrics/agg/chart?window=60&f=model:glm-5-3")
	buckets, ok := pc["buckets"].([]any)
	if !ok || len(buckets) == 0 {
		t.Fatalf("chart buckets missing: %v", pc["buckets"])
	}
	var req float64
	for _, bucket := range buckets {
		req += bucket.(map[string]any)["req"].(float64)
	}
	if req != 3 {
		t.Fatalf("scoped chart req = %v, want 3", req)
	}

	// Log paging: the scoped archive returns the raw-spelled rows.
	before := now.Add(time.Millisecond).UnixMilli()
	pl := get(t, http.HandlerFunc(agg.HandleLogPage), fmt.Sprintf("/metrics/agg/log?before_ms=%d&before_id=&limit=10&s=&f=model:glm-5-3", before))
	rows, ok := pl["records"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("scoped log rows = %d, want 3 (raw spellings matched by the canonical filter)", pl["records"])
	}
}
