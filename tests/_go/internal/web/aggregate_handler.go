package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestChartAllMinStoredAndUnflushed pins the All window's minimum across both
// ingest paths through the production handler: stored rows first, then
// unflushed ring rows older than every stored row. The window must start at
// the oldest scoped contribution, count each request once, and a canceled
// request must stay an error rather than a recent-only chart.
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
	for _, age := range []time.Duration{time.Minute, 2 * time.Hour} {
		r := mkRec(age.String(), now.Add(-age), 200, "", nil, 10, 100, 5, 5, 0, 0.01)
		r.Provider = "history.example"
		buf.Record(r)
	}
	api := NewAggAPI(buf, s, time.Second)
	p := get(t, http.HandlerFunc(api.HandleAggChart), "/metrics/agg/chart?window=all&f=provider:history.example")
	oldest := now.Add(-2 * time.Hour)
	from := int64(p["from_ms"].(float64))
	step := int64(p["bucket_ms"].(float64))
	if step == 0 || from%step != 0 {
		t.Fatalf("from_ms=%d is not aligned to bucket_ms=%d", from, step)
	}
	if from > oldest.UnixMilli() || oldest.UnixMilli()-from >= step {
		t.Fatalf("from_ms=%d does not start with the oldest scoped unflushed start %d", from, oldest.UnixMilli())
	}
	var requests float64
	for _, bucket := range p["buckets"].([]any) {
		requests += bucket.(map[string]any)["req"].(float64)
	}
	if requests != 4 {
		t.Fatalf("chart requests = %v, want 4 (2 stored + 2 unflushed, deduped)", requests)
	}
	// A failed min lookup must never become a successful recent-only chart.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
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
	if _, err := s.Clear(context.Background(), &storage.PurgeFilter{Provider: "p"}, nil); err != nil {
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
	if postRec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST Allow = %q, want %q", postRec.Header().Get("Allow"), "GET")
	}
	kpiPost := httptest.NewRecorder()
	http.HandlerFunc(api.HandleBootstrap).ServeHTTP(kpiPost, httptest.NewRequest(http.MethodPost, "/metrics/bootstrap", nil))
	if kpiPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics/bootstrap → %d, want 405", kpiPost.Code)
	}
	if kpiPost.Header().Get("Allow") != "GET" {
		t.Fatalf("POST /metrics/bootstrap Allow = %q, want %q", kpiPost.Header().Get("Allow"), "GET")
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
		buf.Record(rec)
		s.Record(rec)
	}
	waitTotals(t, s, 3)
	agg := NewAggAPI(buf, s, time.Second)
	// Cap the full snapshot (main.go derives web.BootRingCapMul * dash_log_rows; this pins the
	// cap mechanics to a tiny ring).
	buf.SetSnapshotLimit(2)

	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	index := httptest.NewRecorder()
	Handler(agg).ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if p["dashboard_version"] != dashboardContentVersion() || !strings.Contains(index.Body.String(), `<meta name="dashboard-version" content="`+dashboardContentVersion()+`">`) {
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
	for _, key := range []string{"dash", "pause", "throttle", "debug", "storm"} {
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
	agg.Storm = func() any {
		return map[string]any{"enabled": true, "storms": []any{map[string]any{"provider": "neutral.example", "error_percent": 75}}}
	}
	p2 := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	for _, key := range []string{"dash", "pause", "throttle", "debug", "storm"} {
		if _, ok := p2[key]; !ok {
			t.Errorf("%s section missing with a provider wired", key)
		}
	}
	storm := p2["storm"].(map[string]any)["storms"].([]any)[0].(map[string]any)
	if storm["provider"] != "neutral.example" || storm["error_percent"] != float64(75) {
		t.Fatalf("bootstrap changed authoritative storm state: %v", storm)
	}
	agg.Dash, agg.Pause, agg.Throttle, agg.Debug, agg.Storm = nil, nil, nil, nil, nil

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
		buf.Record(rec)
		s.Record(rec)
	}
	waitTotals(t, s, 2)
	agg := NewAggAPI(buf, s, time.Second)

	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	if len(p["records"].([]any)) != 2 {
		t.Fatalf("records = %d, want 2", len(p["records"].([]any)))
	}
}
