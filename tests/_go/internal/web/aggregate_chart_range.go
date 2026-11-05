package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// chartCode runs one chart request through the production handler and returns
// the status plus the decoded payload for 200 answers.
func chartCode(t *testing.T, h http.Handler, target string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: decode: %v", target, err)
	}
	return rec.Code, out
}

// TestChartRangeParamValidation pins the explicit-range trust boundary: both
// edges arrive together as canonical decimal milliseconds ordered before any
// clamp, window is mutually exclusive with either edge, and the right edge
// then clamps to the server clock so client skew can never 400 a "to now"
// request while a wholly future range still refuses.
func TestChartRangeParamValidation(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	h := http.HandlerFunc(api.HandleAggChart)
	now := time.Now().UnixMilli()
	min := time.Minute.Milliseconds()
	pastFrom := strconv.FormatInt(now-2*min, 10)
	futureFrom := strconv.FormatInt(now+min, 10)
	futureTo := strconv.FormatInt(now+time.Hour.Milliseconds(), 10)
	valid := "from_ms=" + pastFrom + "&to_ms=" + strconv.FormatInt(now-min, 10)

	for _, tc := range []struct {
		name   string
		query  string
		status int
	}{
		{"past range", valid, http.StatusOK},
		{"future to_ms clamps to the server clock", "from_ms=" + pastFrom + "&to_ms=" + futureTo, http.StatusOK},
		{"fully future range", "from_ms=" + futureFrom + "&to_ms=" + futureTo, http.StatusBadRequest},
		{"from only", "from_ms=" + pastFrom, http.StatusBadRequest},
		{"to only", "to_ms=" + futureTo, http.StatusBadRequest},
		{"from zero", "from_ms=0&to_ms=1000", http.StatusBadRequest},
		{"negative from", "from_ms=-1&to_ms=1000", http.StatusBadRequest},
		{"from equals to", "from_ms=1000&to_ms=1000", http.StatusBadRequest},
		{"from after to", "from_ms=2000&to_ms=1000", http.StatusBadRequest},
		{"padded from", "from_ms=015&to_ms=2000", http.StatusBadRequest},
		{"plus-signed from", "from_ms=%2B1000&to_ms=2000", http.StatusBadRequest},
		{"exponent from", "from_ms=1e3&to_ms=2000", http.StatusBadRequest},
		{"empty from", "from_ms=&to_ms=2000", http.StatusBadRequest},
		{"non-integer to", "from_ms=1000&to_ms=abc", http.StatusBadRequest},
		{"window with range", "window=60&from_ms=1000&to_ms=2000", http.StatusBadRequest},
		{"window all with range", "window=all&from_ms=1000&to_ms=2000", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := chartCode(t, h, "/metrics/agg/chart?"+tc.query)
			if status != tc.status {
				t.Fatalf("%s: status = %d, want %d", tc.query, status, tc.status)
			}
		})
	}

	// The clamp resolves to the server's own clock, never the client's: a
	// clock ahead of the server still receives the truncated range.
	before := time.Now().UnixMilli()
	status, p := chartCode(t, h, "/metrics/agg/chart?from_ms="+pastFrom+"&to_ms="+futureTo)
	after := time.Now().UnixMilli()
	if status != http.StatusOK {
		t.Fatalf("future to_ms status = %d, want 200", status)
	}
	if to := int64(p["to_ms"].(float64)); to < before || to > after {
		t.Fatalf("clamped to_ms = %d, want within the request clock [%d, %d]", to, before, after)
	}

	// Repeated consumed keys are ambiguous, exactly like window and s.
	for _, tc := range []struct{ query, want string }{
		{"from_ms=1&from_ms=2&to_ms=3", "duplicate from_ms"},
		{"from_ms=1&to_ms=2&to_ms=3", "duplicate to_ms"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/agg/chart?"+tc.query, nil))
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%q", tc.query, rec.Code, rec.Body.String())
		}
		if body.Error != tc.want {
			t.Fatalf("%s: error = %q, want %q", tc.query, body.Error, tc.want)
		}
	}
}

// TestChartRangeGeometry pins the explicit-range fold: the requested start
// floors onto the ladder step, the exclusive end excludes a request exactly
// at to_ms while an end-1ms request stays in the last (possibly partial)
// bucket, contributions before the aligned start never count, and the
// minimum-one and bucket-cap behaviours survive on both ingest paths.
func TestChartRangeGeometry(t *testing.T) {
	s := testStore(t)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	alignedFrom := base.Add(30 * time.Minute)
	requestedFrom := base.Add(30*time.Minute + 10*time.Second)
	to := base.Add(50 * time.Minute)
	for _, spec := range []struct {
		id string
		at time.Time
	}{
		{"before-from", alignedFrom.Add(-time.Millisecond)},
		{"aligned", alignedFrom},
		{"requested", requestedFrom},
		{"to-minus-1", to.Add(-time.Millisecond)},
		{"at-to", to},
		{"after-to", to.Add(time.Millisecond)},
	} {
		s.Record(mkRec(spec.id, spec.at, 200, "", nil, 10, 100, 1, 1, 0, 0))
	}
	waitTotals(t, s, 6)
	api := NewAggAPI(metrics.NewBuffer(4), s, time.Second)
	target := fmt.Sprintf("/metrics/agg/chart?from_ms=%d&to_ms=%d", requestedFrom.UnixMilli(), to.UnixMilli())

	// Span 19m50s → the one-minute ladder step; the requested start floors to
	// the :30 mark and 20 one-minute buckets cover [from, to).
	status, p := chartCode(t, http.HandlerFunc(api.HandleAggChart), target)
	if status != http.StatusOK {
		t.Fatalf("range status = %d", status)
	}
	if got := int64(p["from_ms"].(float64)); got != alignedFrom.UnixMilli() {
		t.Fatalf("from_ms = %d, want the floored %d", got, alignedFrom.UnixMilli())
	}
	if got := int64(p["to_ms"].(float64)); got != to.UnixMilli() {
		t.Fatalf("to_ms = %d, want the requested past end %d", got, to.UnixMilli())
	}
	if got := int64(p["bucket_ms"].(float64)); got != time.Minute.Milliseconds() {
		t.Fatalf("bucket_ms = %d, want the 1m ladder step", got)
	}
	buckets := p["buckets"].([]any)
	if len(buckets) != 20 {
		t.Fatalf("buckets = %d, want 20 covering [from, to)", len(buckets))
	}
	first := buckets[0].(map[string]any)
	if int64(first["t"].(float64)) != alignedFrom.UnixMilli() || first["req"].(float64) != 2 {
		t.Fatalf("first bucket = %v, want t=%d req=2 (aligned + requested)", first, alignedFrom.UnixMilli())
	}
	last := buckets[19].(map[string]any)
	if int64(last["t"].(float64)) != to.Add(-time.Minute).UnixMilli() || last["req"].(float64) != 1 {
		t.Fatalf("last bucket = %v, want t=%d req=1 (to-1ms only)", last, to.Add(-time.Minute).UnixMilli())
	}
	var sum float64
	for _, b := range buckets {
		sum += b.(map[string]any)["req"].(float64)
	}
	if sum != 3 {
		t.Fatalf("range req sum = %v, want 3 (before-from, at-to and after-to excluded)", sum)
	}

	// Ring parity: the live tail passes the same to-clip.
	buf := metrics.NewBuffer(8)
	for _, spec := range []struct {
		id string
		at time.Time
	}{
		{"ring-aligned", alignedFrom},
		{"ring-to-minus-1", to.Add(-time.Millisecond)},
		{"ring-at-to", to},
	} {
		buf.Record(mkRec(spec.id, spec.at, 200, "", nil, 10, 100, 1, 1, 0, 0))
	}
	ringAPI := NewAggAPI(buf, nil, time.Second)
	status, p = chartCode(t, http.HandlerFunc(ringAPI.HandleAggChart), target)
	if status != http.StatusOK {
		t.Fatalf("ring range status = %d", status)
	}
	var ringSum float64
	for _, b := range p["buckets"].([]any) {
		ringSum += b.(map[string]any)["req"].(float64)
	}
	if ringSum != 2 {
		t.Fatalf("ring range req sum = %v, want 2 (at-to excluded on the live path too)", ringSum)
	}

	// Minimum one bucket: a 1ms span still renders one aligned bucket.
	status, p = chartCode(t, http.HandlerFunc(api.HandleAggChart), fmt.Sprintf("/metrics/agg/chart?from_ms=%d&to_ms=%d", requestedFrom.UnixMilli(), requestedFrom.UnixMilli()+1))
	if status != http.StatusOK {
		t.Fatalf("1ms range status = %d", status)
	}
	if n := len(p["buckets"].([]any)); n != 1 {
		t.Fatalf("1ms range buckets = %d, want the one-bucket minimum", n)
	}

	// The ladder caps a decade-long range at chartMaxBuckets (+ one alignment
	// partial), never an unbounded allocation.
	status, p = chartCode(t, http.HandlerFunc(api.HandleAggChart), fmt.Sprintf("/metrics/agg/chart?from_ms=%d&to_ms=%d",
		time.Now().AddDate(-10, 0, 0).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli()))
	if status != http.StatusOK {
		t.Fatalf("decade range status = %d", status)
	}
	if n := len(p["buckets"].([]any)); n < 1 || n > chartMaxBuckets+1 {
		t.Fatalf("decade range buckets = %d, want 1..%d", n, chartMaxBuckets+1)
	}
}

// TestChartWindowToMsIsTheServeClock pins the window path's new right edge:
// to_ms is the exact serve clock the fold answered for, within a request
// clock bracket, while from_ms keeps its aligned-start meaning.
func TestChartWindowToMsIsTheServeClock(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	h := http.HandlerFunc(api.HandleAggChart)
	for _, target := range []string{"/metrics/agg/chart?window=15", "/metrics/agg/chart?window=all"} {
		before := time.Now().UnixMilli()
		status, p := chartCode(t, h, target)
		after := time.Now().UnixMilli()
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d", target, status)
		}
		if to := int64(p["to_ms"].(float64)); to < before || to > after {
			t.Fatalf("%s: to_ms = %d, want the serve clock in [%d, %d]", target, to, before, after)
		}
	}
}

// TestChartRangeMemoSeparateAndReusable pins the range memo identity: a range
// of the same span as a window never shares its entry, an identical range is
// served from its own, and the resolved end of a clock-clamped range advances
// with the clock instead of replaying the earlier resolution.
func TestChartRangeMemoSeparateAndReusable(t *testing.T) {
	buf := metrics.NewBuffer(8)
	now := time.Now()
	buf.Record(cacheRecord("a", now.Add(-2*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	h := http.HandlerFunc(api.HandleAggChart)

	// A 30-minute window first, publishing the window-keyed memo entry.
	if status, _ := chartCode(t, h, "/metrics/agg/chart?window=30"); status != http.StatusOK {
		t.Fatalf("window status = %d", status)
	}
	windowEntry := api.chartMemo.entry
	if windowEntry == nil {
		t.Fatal("window chart was not memoized")
	}

	// The same span as an explicit range is a different identity and answers
	// with its own pinned edges.
	fromMs := now.Add(-30 * time.Minute).UnixMilli()
	toMs := now.Add(-time.Second).UnixMilli()
	target := fmt.Sprintf("/metrics/agg/chart?from_ms=%d&to_ms=%d", fromMs, toMs)
	status, p := chartCode(t, h, target)
	if status != http.StatusOK {
		t.Fatalf("range status = %d", status)
	}
	rangeEntry := api.chartMemo.entry
	if rangeEntry == nil || rangeEntry == windowEntry || rangeEntry.key == windowEntry.key {
		t.Fatalf("range request collided with the window memo entry: %v", rangeEntry)
	}
	bucketMs := int64(p["bucket_ms"].(float64))
	if got := int64(p["from_ms"].(float64)); got != fromMs-fromMs%bucketMs {
		t.Fatalf("range from_ms = %d, want the floored %d", got, fromMs-fromMs%bucketMs)
	}
	if got := int64(p["to_ms"].(float64)); got != toMs {
		t.Fatalf("range to_ms = %d, want the requested past end %d", got, toMs)
	}

	// The identical range is served from its memo; only the clock metadata
	// changes.
	status, p2 := chartCode(t, h, target)
	if status != http.StatusOK || api.chartMemo.entry != rangeEntry {
		t.Fatal("identical range refolded instead of hitting its memo")
	}
	if p2["to_ms"] != p["to_ms"] {
		t.Fatalf("range memo hit moved to_ms: %v then %v", p["to_ms"], p2["to_ms"])
	}

	// A clamped end resolves against the clock at every request: waiting past
	// the first response's edge must produce a later edge, not a stale hit.
	future := now.Add(time.Hour).UnixMilli()
	clamped := fmt.Sprintf("/metrics/agg/chart?from_ms=%d&to_ms=%d", fromMs, future)
	_, p3 := chartCode(t, h, clamped)
	t1 := int64(p3["to_ms"].(float64))
	for time.Now().UnixMilli() <= t1 {
		time.Sleep(time.Millisecond)
	}
	_, p4 := chartCode(t, h, clamped)
	if t2 := int64(p4["to_ms"].(float64)); t2 <= t1 {
		t.Fatalf("clamped range replayed a stale to_ms: %d then %d", t1, t2)
	}
}
