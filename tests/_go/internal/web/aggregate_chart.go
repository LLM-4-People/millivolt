package web

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"fmt"
	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestChartBucketsAndPercentiles(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	// 4 requests 30 minutes ago, each with a known ttft (10..40ms) so the
	// bucket's p50 = 25, p95 = 38.5, p99 = 39.7 (linear interp). Identical
	// starts keep all four in ONE bucket wherever clock alignment puts the
	// edges; 30m back is inside the 60m window for any aligned from. Each
	// carries 6 cached-token reads → the bucket's cache fold = 24. Each also
	// streams a decode window of 100..400ms (first to last token) → the
	// bucket's dec p50 = 250, p95 = 385, p99 = 397.
	for i, v := range []int64{10, 20, 30, 40} {
		r := mkRec(string(rune('t'+i)), now.Add(-30*time.Minute), 200, "", nil, v, 200, 10, 10, 6, 0)
		r.DecodeTPS = float64(v) + 0.5 // the one TPS metric, floats survive storage
		r.FirstTokenAt = r.Start.Add(time.Duration(v) * time.Millisecond)
		r.LastTokenAt = r.FirstTokenAt.Add(time.Duration(100*(i+1)) * time.Millisecond)
		s.Record(r)
	}
	// 1 error 10 minutes ago: its bucket has 1 sample < pctMinSamples → the
	// ttft/tps triples must be all-null (suppression gate).
	errRec := mkRec("err1", now.Add(-10*time.Minute), 500, "boom", nil, 5, 100, 5, 0, 0, 0)
	errRec.DecodeTPS = 5.5 // the one TPS sample on the error record
	s.Record(errRec)
	// 1 recovered 429 10 minutes ago: final 200 with an ABSORBED 429 attempt.
	// Health counts are distinct affected requests: it folds into the chart's
	// rate-limit bucket only - never into err (429 alone is not an error).
	rlRec := mkRec("rl1", now.Add(-10*time.Minute), 200, "", []metrics.RetryAttempt{
		{StatusCode: 429, ErrorType: "rate_limit", At: now.Add(-10 * time.Minute)},
	}, 0, 100, 0, 0, 0, 0)
	s.Record(rlRec)
	waitTotals(t, s, 6)
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
	var req, err, rl float64
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
		rl += m["rl"].(float64)
		if m["req"].(float64) == 4 {
			rich = m
		} else if m["req"].(float64) > 0 {
			// A <pctMinSamples-sample bucket suppresses percentiles entirely.
			for _, k := range []string{"ttft", "tps", "dec"} {
				for _, x := range m[k].([]any) {
					if x != nil {
						t.Fatalf("bucket with %v requests has non-null %s: %v", m["req"], k, m[k])
					}
				}
			}
		}
	}
	if req != 6 || err != 1 {
		t.Fatalf("chart totals req=%v err=%v, want 6/1", req, err)
	}
	if rl != 1 {
		t.Fatalf("chart totals rl=%v, want 1 (recovered 429 folds into rate limit only)", rl)
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
	// decode window pct of [100,200,300,400]: p50 250, p95 385, p99 397
	// (linear interp, same R7 machinery; the error and 429 fixtures carry no
	// token timestamps so they contribute no dec samples here).
	dc := rich["dec"].([]any)
	if dc[0] == nil || dc[0].(float64) != 250 || dc[1].(float64) != 385 ||
		math.Abs(dc[2].(float64)-397) > 1e-9 {
		t.Fatalf("dec pct = %v, want [250 385 397]", dc)
	}
	if _, exists := rich["dur"]; exists {
		t.Fatal("removed duration series still serialized")
	}
	// cached prompt tokens fold per bucket (4 records × 6 reads = 24).
	if rich["cache"].(float64) != 24 {
		t.Fatalf("bucket cache = %v, want 24", rich["cache"])
	}
	if rich["rl"].(float64) != 0 {
		t.Fatalf("rich bucket rl = %v, want 0", rich["rl"])
	}
	// The recovered-429 bucket folds both health counts side by side: one
	// final error and one rate-limited request, two distinct affected requests.
	var rlBucket map[string]any
	for _, b := range buckets {
		m := b.(map[string]any)
		if m["rl"].(float64) == 1 {
			rlBucket = m
		}
	}
	if rlBucket == nil || rlBucket["err"].(float64) != 1 || rlBucket["req"].(float64) != 2 {
		t.Fatalf("recovered-429 bucket = %v, want req=2 err=1 rl=1", rlBucket)
	}
	// Period-wide percentile triples (the totals strip): all 5 samples clear
	// the ≥4 gate - ttft [5,10,20,30,40] → p50 20, p95 38, p99 39.6;
	// tps [5.5,10.5,20.5,30.5,40.5] → p50 20.5, p95 38.5, p99 40.1. The
	// decode window only has the 4 streamed samples [100,200,300,400] - the
	// ttft-only error and 429 fixtures contribute none - so dec_p is the
	// same [250, 385, 397] as the rich bucket.
	tp := p["ttft_p"].([]any)
	if tp[0] == nil || tp[0].(float64) != 20 || tp[1].(float64) != 38 ||
		math.Abs(tp[2].(float64)-39.6) > 1e-9 {
		t.Fatalf("ttft_p = %v, want [20 38 39.6]", tp)
	}
	sp := p["tps_p"].([]any)
	if sp[0] == nil || sp[0].(float64) != 20.5 || sp[1].(float64) != 38.5 || math.Abs(sp[2].(float64)-40.1) > 1e-9 {
		t.Fatalf("tps_p = %v, want [20.5 38.5 40.1]", sp)
	}
	dp := p["dec_p"].([]any)
	if dp[0] == nil || dp[0].(float64) != 250 || dp[1].(float64) != 385 || math.Abs(dp[2].(float64)-397) > 1e-9 {
		t.Fatalf("dec_p = %v, want [250 385 397] (only the streamed requests sample)", dp)
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
		q, err := adminjson.StrictQuery(r)
		if err != nil {
			t.Fatalf("window %q: strict parse: %v", v, err)
		}
		got, ok := windowParam(q)
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

// TestAggregateRoutesAdoptStrictQuery pins the read surfaces' strict-query
// adoption: the chart, explorer, log page and bootstrap parse the raw query
// string, so a malformed pair can never be silently dropped into a broader
// scope (s=%zz must not become "no scope"; a limit spelled twice must not
// first-win its valid copy through the duplicate gate) and a repeated
// consumed key is denied, never first-wins. The explorer row pairs a valid
// dim with the malformed s pair: dim=%zz alone self-heals through the
// invalid-dim gate under either parse, so the dim+s shape is the one that
// discriminates a lenient revert. Every row also message-pins its 400
// through the flat error transport; those wording pins and the strict
// adoption are freeze rows (already-correct behavior being pinned: a lenient
// revert or a duplicate-wording drift must redden, not silently pass). The
// legitimately repeated f= list key keeps its standing grammar.
func TestAggregateRoutesAdoptStrictQuery(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	for _, tc := range []struct {
		name   string
		handle http.HandlerFunc
		target string
		want   string
	}{
		{"chart: malformed s pair must not silently broaden the scope", api.HandleAggChart, "/metrics/agg/chart?window=15&s=%zz", "invalid query"},
		{"chart: repeated window is denied, never first-wins", api.HandleAggChart, "/metrics/agg/chart?window=15&window=60", "duplicate window"},
		{"chart: repeated s is denied, never first-wins", api.HandleAggChart, "/metrics/agg/chart?window=15&s=200&s=404", "duplicate s"},
		{"explorer: malformed s pair must not silently broaden the scope", api.HandleAggExplorer, "/metrics/agg/explorer?dim=provider&s=%zz", "invalid query"},
		{"explorer: repeated dim is denied, never first-wins", api.HandleAggExplorer, "/metrics/agg/explorer?dim=provider&dim=status", "duplicate dim"},
		{"log page: a valid and a malformed limit answers 400 at the parse", api.HandleLogPage, "/metrics/agg/log?limit=50&limit=%zz", "invalid query"},
		{"log page: repeated s is denied, never first-wins", api.HandleLogPage, "/metrics/agg/log?limit=50&s=streaming&s=paused", "duplicate s"},
		{"bootstrap: malformed feed pin must not trust the cursor", api.HandleBootstrap, "/metrics/bootstrap?feed=%zz&since=1", "invalid query"},
		{"bootstrap: repeated feed pin is denied, never first-wins", api.HandleBootstrap, "/metrics/bootstrap?feed=a&feed=b", "duplicate feed"},
		{"bootstrap: repeated since is denied, never first-wins", api.HandleBootstrap, "/metrics/bootstrap?since=1&since=2", "duplicate since"},
	} {
		w := httptest.NewRecorder()
		tc.handle(w, httptest.NewRequest(http.MethodGet, tc.target, nil))
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: body is not the flat error shape: %v (%q)", tc.name, err, w.Body.String())
			continue
		}
		if w.Code != http.StatusBadRequest || body.Error != tc.want {
			t.Errorf("%s: status=%d error=%q, want 400 %q", tc.name, w.Code, body.Error, tc.want)
		}
	}
	// The repeated f= list grammar is untouched: two well-formed filters
	// still scope the chart exactly as before the adoption.
	w := httptest.NewRecorder()
	api.HandleAggChart(w, httptest.NewRequest(http.MethodGet, "/metrics/agg/chart?window=15&f=client:a&f=status:2xx", nil))
	if w.Code != 200 {
		t.Errorf("two f= filters: status = %d, want the standing 200", w.Code)
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
	// The unflushed ring record recovered from an absorbed 429: it must fold
	// into the chart's rate-limit count through fromRecord (the ring path)
	// without ever counting as an error or a second request.
	live := mkRec("not-flushed", now.Add(-time.Minute), 200, "", []metrics.RetryAttempt{
		{StatusCode: 429, ErrorType: "rate_limit", At: now.Add(-time.Minute)},
	}, 80, 400, 40, 20, 0, 0.04)
	live.Provider = old.Provider
	buf.Record(live)
	api := NewAggAPI(buf, s, time.Second)
	for _, c := range []struct {
		query   string
		oldest  time.Time
		want    float64
		wantRL  float64
		wantErr float64
	}{
		{"window=all", other.Start, 4, 1, 1},
		{"window=all&f=provider:history.example", old.Start, 3, 1, 0},
		{"window=525600", now.Add(-365 * 24 * time.Hour), 2, 1, 0},
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
		var sum, rl, errSum float64
		for _, b := range buckets {
			m := b.(map[string]any)
			sum += m["req"].(float64)
			rl += m["rl"].(float64)
			errSum += m["err"].(float64)
		}
		if sum != c.want {
			t.Fatalf("%s: req sum=%v, want %v", c.query, sum, c.want)
		}
		// The recovered 429 rides the ring path into rl exactly once, and
		// never into err - the two health counts stay distinct affected
		// requests across both ingest paths.
		if rl != c.wantRL || errSum != c.wantErr {
			t.Fatalf("%s: rl=%v err=%v, want rl=%v err=%v (ring 429 folded once, never an error)", c.query, rl, errSum, c.wantRL, c.wantErr)
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
			if m["req"].(float64) != 0 || m["err"].(float64) != 0 || m["rl"].(float64) != 0 || m["cost"].(float64) != 0 {
				t.Fatalf("window %s: zero-traffic bucket %d is not zero-filled: %v", win, i, m)
			}
			for _, k := range []string{"ttft", "tps", "dec"} {
				for _, x := range m[k].([]any) {
					if x != nil {
						t.Fatalf("window %s: zero-traffic bucket %d has non-null %s", win, i, k)
					}
				}
			}
		}
	}
	// The empty-store payload carries the blended price as null - unmeasured,
	// never a fabricated zero - mirroring the KPI band's absent figure.
	for _, win := range []string{"all", "15"} {
		p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window="+win)
		if p["cost_per_mtok"] != nil {
			t.Fatalf("window %s: zero-traffic cost_per_mtok = %v, want nil", win, p["cost_per_mtok"])
		}
		if p["ttft_stat"] != nil || p["tps_stat"] != nil {
			t.Fatalf("window %s: zero-traffic timing stats = %v / %v, want nil", win, p["ttft_stat"], p["tps_stat"])
		}
	}
}

// The chart's period timing stats are the average, minimum and maximum over
// the same occurrence-weighted sample population the period percentiles
// rank: every captured ttft/tps sample in the window, unmeasured records
// excluded, and a scoped-out window reads null.
func TestChartTimingStats(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	mk := func(id string, at time.Time, ttft int64, tps float64) *metrics.Record {
		r := mkRec(id, at, 200, "", nil, ttft, 1000, 10, 10, 0, 0)
		r.DecodeTPS = tps
		return r
	}
	for _, r := range []*metrics.Record{
		mk("a", now.Add(-4*time.Minute), 100, 50),
		mk("b", now.Add(-3*time.Minute), 200, 60),
		mk("c", now.Add(-2*time.Minute), 300, 70),
		mk("unmeasured", now.Add(-time.Minute), 0, 0),
	} {
		s.Record(r)
	}
	waitTotals(t, s, 4)
	api := NewAggAPI(metrics.NewBuffer(4), s, time.Second)
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=15")
	want := func(key string, avg, lo, hi float64) {
		t.Helper()
		v, ok := p[key].([]any)
		if !ok || len(v) != 3 {
			t.Fatalf("%s = %v, want [avg min max]", key, p[key])
		}
		for i, w := range []float64{avg, lo, hi} {
			if math.Abs(v[i].(float64)-w) > 1e-9 {
				t.Fatalf("%s = %v, want [%v %v %v]", key, p[key], avg, lo, hi)
			}
		}
	}
	want("ttft_stat", 200, 100, 300)
	want("tps_stat", 60, 50, 70)
	p = get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=15&f=client:missing")
	if p["ttft_stat"] != nil || p["tps_stat"] != nil {
		t.Fatalf("out-of-scope timing stats = %v / %v, want nil", p["ttft_stat"], p["tps_stat"])
	}
}

// TestChartDecodeWindow pins the decode-time series end to end: the sample
// domain is last_token_at - first_token_at (exactly the gen_tps measurement
// window), so streamed requests that finished producing tokens contribute
// while non-streaming requests, aborted streams (first token, no last) and
// zero-width windows (first token == last token) never do - independently of
// ttft, which needs only the first token. Bucket and period triples use the
// same R7 machinery and the >= pctMinSamples suppression gate as ttft/tps,
// on the durable scan path; the ring path (fromRecord) is pinned by parity.
// A destructive purge must drop every decode sample with the rebuilt
// projection's fresh metric orders.
func TestChartDecodeWindow(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	// Bucket A (30m back): five streamed requests with decode windows
	// 100..500ms → dec p50 300, p95 480, p99 496 (linear interp).
	streamed := func(id string, at time.Time, window time.Duration) *metrics.Record {
		r := mkRec(id, at, 200, "", nil, 10, 400, 10, 10, 0, 0)
		r.FirstTokenAt = r.Start.Add(10 * time.Millisecond)
		r.LastTokenAt = r.FirstTokenAt.Add(window)
		return r
	}
	for i := range 5 {
		s.Record(streamed(fmt.Sprintf("stream-%d", i), now.Add(-30*time.Minute), time.Duration(100*(i+1))*time.Millisecond))
	}
	// Bucket B (10m back): six requests, only three with a real decode
	// window. ttft sees five samples (the three streamed + the aborted
	// stream + the zero-width stream, all of which reached a first token)
	// → a non-null ttft triple; dec sees three → below pctMinSamples, so
	// the dec triple must stay all-null beside ttft's live one.
	s.Record(streamed("b-1", now.Add(-10*time.Minute), 50*time.Millisecond))
	s.Record(streamed("b-2", now.Add(-10*time.Minute), 60*time.Millisecond))
	s.Record(streamed("b-3", now.Add(-10*time.Minute), 70*time.Millisecond))
	aborted := mkRec("b-aborted", now.Add(-10*time.Minute), 200, "", nil, 10, 400, 10, 10, 0, 0)
	aborted.FirstTokenAt = aborted.Start.Add(10 * time.Millisecond) // stream never produced a last token
	s.Record(aborted)
	s.Record(streamed("b-zero-width", now.Add(-10*time.Minute), 0)) // first token == last token
	s.Record(mkRec("b-non-stream", now.Add(-10*time.Minute), 200, "", nil, 0, 400, 10, 10, 0, 0))
	waitTotals(t, s, 11)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=60")
	var rich, mixed map[string]any
	for _, b := range p["buckets"].([]any) {
		m := b.(map[string]any)
		switch m["req"].(float64) {
		case 5:
			rich = m
		case 6:
			mixed = m
		}
	}
	if rich == nil || mixed == nil {
		t.Fatalf("fixture buckets missing: rich=%v mixed=%v", rich, mixed)
	}
	dc := rich["dec"].([]any)
	if dc[0] == nil || dc[0].(float64) != 300 || dc[1].(float64) != 480 || math.Abs(dc[2].(float64)-496) > 1e-9 {
		t.Fatalf("streamed bucket dec = %v, want [300 480 496]", dc)
	}
	for _, x := range mixed["dec"].([]any) {
		if x != nil {
			t.Fatalf("mixed bucket dec = %v, want all-null (3 samples < pctMinSamples while ttft stays live: %v)", mixed["dec"], mixed["ttft"])
		}
	}
	if tt := mixed["ttft"].([]any); tt[0] == nil {
		t.Fatalf("mixed bucket ttft = %v, want the live 5-sample triple beside a suppressed dec", mixed["ttft"])
	}
	// Period dec_p ranks every streamed window: [50,60,70,100,200,300,400,500]
	// → p50 150, p95 465, p99 493 (interpolation float dust tolerated).
	dp := p["dec_p"].([]any)
	if dp[0] == nil || dp[0].(float64) != 150 || math.Abs(dp[1].(float64)-465) > 1e-9 || math.Abs(dp[2].(float64)-493) > 1e-9 {
		t.Fatalf("dec_p = %v, want [150 465 493]", dp)
	}

	// Ring parity: the same streamed records folded through fromRecord (a
	// store-less aggregate) land the same bucket triple.
	buf := metrics.NewBuffer(8)
	for i := range 5 {
		buf.Record(streamed(fmt.Sprintf("ring-%d", i), now.Add(-30*time.Minute), time.Duration(100*(i+1))*time.Millisecond))
	}
	ringAPI := NewAggAPI(buf, nil, time.Second)
	rp := get(t, http.HandlerFunc(ringAPI.HandleAggChart), "/metrics/agg/chart?window=60")
	var ringRich map[string]any
	for _, b := range rp["buckets"].([]any) {
		m := b.(map[string]any)
		if m["req"].(float64) == 5 {
			ringRich = m
		}
	}
	if ringRich == nil {
		t.Fatal("ring path lost the five streamed requests")
	}
	dec := ringRich["dec"].([]any)
	if dec[0] == nil || dec[0].(float64) != 300 || dec[1].(float64) != 480 || math.Abs(dec[2].(float64)-496) > 1e-9 {
		t.Fatalf("ring bucket dec = %v, want the durable path's [300 480 496]", dec)
	}

	// Destructive invalidation: a full purge rebuilds the projection from an
	// empty history - the third metric order must not survive it.
	if _, err := s.Clear(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	p = get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=60")
	var req float64
	for _, b := range p["buckets"].([]any) {
		m := b.(map[string]any)
		req += m["req"].(float64)
		for _, x := range m["dec"].([]any) {
			if x != nil {
				t.Fatal("post-purge bucket carries a decode sample")
			}
		}
	}
	if req != 0 {
		t.Fatalf("post-purge req = %v, want 0", req)
	}
	for _, x := range p["dec_p"].([]any) {
		if x != nil {
			t.Fatal("post-purge dec_p carries a decode sample")
		}
	}
}

// The chart's period cost_per_mtok reuses the KPI band's blended rule: only
// cost-REPORTING requests contribute the token denominator, so a mixed
// window prices over the reporter's tokens alone and a zero-cost window
// reads null.
func TestChartBlendedCostPerMTok(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	reporter := mkRec("reporter", now.Add(-2*time.Minute), 200, "", nil, 10, 50, 100, 50, 0, 0.25)
	plain := mkRec("plain", now.Add(-time.Minute), 200, "", nil, 10, 50, 900, 900, 0, 0)
	for _, r := range []*metrics.Record{reporter, plain} {
		s.Record(r)
	}
	waitTotals(t, s, 2)
	api := NewAggAPI(metrics.NewBuffer(4), s, time.Second)
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=15")
	v, ok := p["cost_per_mtok"].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatalf("cost_per_mtok = %v, want the blended price over the reporter's tokens only", p["cost_per_mtok"])
	}
	// 0.25 over the reporter's 150 tokens: 0.25/150*1e6 - the plain request's
	// 1800 tokens must not dilute the price.
	if want := 0.25 / 150 * 1e6; math.Abs(v-want) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", v, want)
	}
	// Scoping away the reporter leaves no price: null, never zero.
	p = get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=15&f=client:missing")
	if p["cost_per_mtok"] != nil {
		t.Fatalf("out-of-scope cost_per_mtok = %v, want nil", p["cost_per_mtok"])
	}
}
