package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestLogPageQueryClassification pins the one pushdown owner: exact stored
// dimensions are SQL predicates, while derived predicates must keep the
// growing Go scan even when some of their rows also narrow SQL.
func TestLogPageQueryClassification(t *testing.T) {
	plain := &modelCanonizer{memo: map[string]string{}}
	withRules := &modelCanonizer{
		exec: config.CompileModelRules([]config.ModelRule{{Mode: "lower"}}),
		memo: map[string]string{},
	}
	for _, tc := range []struct {
		name       string
		fs         []scopeFilter
		status     string
		mcz        *modelCanonizer
		wantPushed bool
		wantAll    int
	}{
		{"empty", nil, "", plain, true, 0},
		{"client", []scopeFilter{{dim: "client", id: "c"}}, "", plain, true, 1},
		{"provider", []scopeFilter{{dim: "provider", id: "p"}}, "", plain, true, 1},
		{"key", []scopeFilter{{dim: "key", id: "k"}}, "", plain, true, 1},
		{"conversation", []scopeFilter{{dim: "conversation", id: "s:1"}}, "", plain, true, 1},
		{"model raw", []scopeFilter{{dim: "model", id: "m"}}, "", plain, true, 1},
		{"model padded", []scopeFilter{{dim: "model", id: " m "}}, "", plain, false, 1},
		{"model rules", []scopeFilter{{dim: "model", id: "m"}}, "", withRules, false, 0},
		{"status class", []scopeFilter{{dim: "status", id: "5xx"}}, "", plain, false, 0},
		{"time", []scopeFilter{{dim: "time", id: "work"}}, "", plain, false, 0},
		{"tool", []scopeFilter{{dim: "tool", id: "search"}}, "", plain, false, 0},
		{"error", []scopeFilter{{dim: "error", id: "x|y|z"}}, "", plain, false, 0},
		{"numeric status", nil, "429", plain, true, 1},
		{"noncanonical status", nil, "0429", plain, false, 1},
		{"sql and go", []scopeFilter{{dim: "provider", id: "p"}, {dim: "tool", id: "search"}}, "", plain, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter, pushed := logPageQuery(tc.fs, tc.status, tc.mcz)
			if pushed != tc.wantPushed || len(filter.All) != tc.wantAll {
				t.Fatalf("pushed=%v predicates=%d, want pushed=%v predicates=%d", pushed, len(filter.All), tc.wantPushed, tc.wantAll)
			}
		})
	}
}

func TestLogPageRetainsTimestampTies(t *testing.T) {
	s := testStore(t)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range 31 {
		s.Record(&metrics.Record{ID: fmt.Sprintf("tie-%02d", i), Provider: "p", Start: start, StatusCode: 200})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(2), s, time.Second).HandleLogPage)
	first := get(t, h, fmt.Sprintf("/metrics/agg/log?before_ms=%d&before_id=&limit=10", start.UnixMilli()+1))
	if len(first["records"].([]any)) != 10 {
		t.Fatalf("first page: %v", first)
	}
	second := get(t, h, fmt.Sprintf("/metrics/agg/log?before_ms=%d&before_id=tie-21&limit=10", start.UnixMilli()))
	rows := second["records"].([]any)
	if len(rows) != 10 || rows[0].(map[string]any)["id"] != "tie-20" || rows[9].(map[string]any)["id"] != "tie-11" {
		t.Fatalf("timestamp tie lost at page boundary: %v", second)
	}
}

func TestLogPagePushdownReturnsScopedMatchesDirectly(t *testing.T) {
	// Exact provider equality is pushed into SQL, so a page returns the
	// matching rows without decoding the newer unrelated history first.
	s := testStoreWithQueryTimeout(t, time.Hour)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range logScanMax + 7 {
		provider := "other"
		if i < 7 {
			provider = "target"
		}
		s.Record(&metrics.Record{ID: fmt.Sprintf("row-%04d", i), Provider: provider, Start: start, StatusCode: 200})
		if i%logScanBatch == 0 {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(1), s, time.Hour).HandleLogPage)
	first := get(t, h, "/metrics/agg/log?limit=10&f=provider:target")
	records, _ := first["records"].([]any)
	if len(records) != 7 || first["more"] != false {
		t.Fatalf("pushdown page = %d records, more=%v; want 7, false", len(records), first["more"])
	}
	for _, target := range []string{
		"?limit=10&before_ms=1", "?limit=10&before_id=x", "?limit=10&before_ms=nope&before_id=x",
		"?limit=10&before_ms=1&before_ms=2&before_id=x", "?limit=10&before_ms=1&before_id=x&before_id=y",
		"?limit=10&limit=20",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/agg/log"+target, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("malformed cursor accepted: %s status=%d", target, w.Code)
		}
	}
}

func TestLogPageUnpushableScopeBudgetAdvancesAcrossTimestampTies(t *testing.T) {
	// This tests a row-work budget, not a wall-clock performance requirement.
	// Tool membership cannot be pushed into SQL, so a page that scans its
	// full budget without a match must still return a progress cursor instead
	// of rescanning the same newest rows forever.
	// The query deadline must stay a generous real one: a synthetic deadline
	// inside a synctest bubble sprints past while the single query goroutine
	// blocks on real SQLite file I/O on a loaded CI runner, fail-closing the
	// page with 500. The hour must ride BOTH the aggregate context below and
	// the store's own QueryTimeout: storage.withQueryTimeout wraps the handler
	// context unconditionally, so the shared fixture's one-second cap silently
	// overrode this one-hour budget and a loaded runner failed it as
	// "agg log page: context deadline exceeded".
	s := testStoreWithQueryTimeout(t, time.Hour)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range logScanMax + 7 {
		s.Record(&metrics.Record{ID: fmt.Sprintf("row-%04d", i), Provider: "target", Start: start, StatusCode: 200})
		if i%logScanBatch == 0 {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(1), s, time.Hour).HandleLogPage)
	first := get(t, h, "/metrics/agg/log?limit=10&f=tool:missing")
	if len(first["records"].([]any)) != 0 || first["more"] != true || first["cursor_id"] != "row-0007" {
		t.Fatalf("scope-budget cursor=%v", first)
	}
	second := get(t, h, fmt.Sprintf("/metrics/agg/log?limit=10&f=tool:missing&before_ms=%.0f&before_id=%s", first["cursor_ms"], url.QueryEscape(first["cursor_id"].(string))))
	if len(second["records"].([]any)) != 0 || second["more"] != false {
		t.Fatalf("scope-budget continuation=%v", second)
	}
}

func TestLogPageFailsClosedOnCancellation(t *testing.T) {
	s := testStore(t)
	s.Record(&metrics.Record{ID: "must-not-leak", Start: time.Unix(1, 0), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, canceledRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled-request=%t", canceledRequest), func(t *testing.T) {
			// An already-expired aggregate deadline and an already-canceled
			// caller are deterministic; neither relies on a fast/slow SQL query.
			timeout := time.Duration(0)
			r := httptest.NewRequest(http.MethodGet, "/metrics/agg/log?limit=10", nil)
			if canceledRequest {
				timeout = time.Second
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			NewAggAPI(metrics.NewBuffer(1), s, timeout).HandleLogPage(w, r)
			if w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != `{"error":"log page failed"}` {
				t.Fatalf("canceled page must not publish partial rows: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAggregateQueryContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = time.Second
		a := NewAggAPI(nil, nil, timeout)
		ctx, cancel := a.queryCtx(httptest.NewRequest(http.MethodGet, "/metrics/agg/log?limit=10", nil))
		defer cancel()
		time.Sleep(timeout - time.Nanosecond)
		synctest.Wait()
		if err := ctx.Err(); err != nil {
			t.Fatalf("aggregate deadline fired early: %v", err)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if err := ctx.Err(); err != context.DeadlineExceeded {
			t.Fatalf("aggregate deadline not enforced: %v", err)
		}
	})
}

// TestLogPageDecodeBudget pins the page-size request discipline. A SQL-pushed
// or fully matching Go-only scope must decode only the rows the requested page
// can use; the old fixed 512-row batch failed this on a 60-row page.
func TestLogPageDecodeBudget(t *testing.T) {
	s := testStoreWithQueryTimeout(t, time.Hour)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range 5_000 {
		s.Record(&metrics.Record{ID: fmt.Sprintf("row-%05d", i), Provider: fmt.Sprintf("provider-%02d", i%10), Start: start.Add(time.Duration(i) * time.Millisecond), StatusCode: 200})
		if i%512 == 511 {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(1), s, time.Hour).HandleLogPage)
	for _, tc := range []struct {
		name, query string
		maxBytes    int64
		maxAllocs   int64
	}{
		{"empty_limit_60", "/metrics/agg/log?limit=60", 600 << 10, 12_000},
		{"empty_limit_200", "/metrics/agg/log?limit=200", 2 << 20, 15_000},
		{"sql_provider_limit_60", "/metrics/agg/log?limit=60&f=provider:provider-03", 600 << 10, 12_000},
		{"go_status_class_limit_60", "/metrics/agg/log?limit=60&f=status:2xx", 600 << 10, 12_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.query, nil))
					if w.Code != http.StatusOK {
						b.Fatalf("%s status=%d body=%s", tc.query, w.Code, w.Body.String())
					}
				}
			})
			if got := res.AllocedBytesPerOp(); got > tc.maxBytes {
				t.Fatalf("%s allocated %d bytes/op, want at most %d (decoded rows must follow the page)", tc.query, got, tc.maxBytes)
			}
			if got := res.AllocsPerOp(); got > tc.maxAllocs {
				t.Fatalf("%s allocated %d times/op, want at most %d", tc.query, got, tc.maxAllocs)
			}
		})
	}
}

// TestStreamWindowRingCancellation bounds the ring-only visitor: an abandoned
// fold must observe cancellation at the next block boundary instead of
// completing every ring row before the final check.
func TestStreamWindowRingCancellation(t *testing.T) {
	const rows = 4 * ctxCheckRows
	buf := metrics.NewBuffer(rows)
	for i := range rows {
		buf.Record(&metrics.Record{ID: fmt.Sprintf("ring-%06d", i), Start: time.UnixMilli(int64(1_800_000_000_000 + i)), StatusCode: 200})
	}
	api := NewAggAPI(buf, nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	err := api.streamWindow(ctx, 0, nil, nil, func(*contrib) error {
		visited++
		if visited == 100 {
			cancel()
		}
		return nil
	}, nil)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ring fold returned %v", err)
	}
	if visited > 100+ctxCheckRows {
		t.Fatalf("canceled ring fold visited %d rows, want at most %d", visited, 100+ctxCheckRows)
	}
	if visited == rows {
		t.Fatal("canceled ring fold walked the whole ring")
	}
}

// cancelAfterErrCalls cancels its wrapped context on the Nth Err observation,
// making mid-fold cancellation deterministic without timing races.
type cancelAfterErrCalls struct {
	context.Context
	cancel context.CancelFunc
	limit  int
	calls  int
}

func (c *cancelAfterErrCalls) Err() error {
	c.calls++
	if c.calls >= c.limit {
		c.cancel()
	}
	return c.Context.Err()
}

// TestStreamAllChartCancellationDoesNotPublish pins the all-window boundary:
// cancellation observed before the fold completes must skip the ready
// publication instead of emitting a partial chart.
func TestStreamAllChartCancellationDoesNotPublish(t *testing.T) {
	buf := metrics.NewBuffer(8)
	for i := range 3 {
		buf.Record(&metrics.Record{ID: fmt.Sprintf("ring-%d", i), Start: time.UnixMilli(int64(1_800_000_000_000 + i)), StatusCode: 200})
	}
	api := NewAggAPI(buf, nil, time.Second)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterErrCalls{Context: base, cancel: cancel, limit: 3}
	published := 0
	err := api.streamAllChart(ctx, nil, "", nil, time.Now().UnixMilli(), func(int64, int64, int64, int, *chartFold) {
		published++
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled all-window fold returned %v", err)
	}
	if published != 0 {
		t.Fatalf("canceled all-window fold published %d partial charts", published)
	}
}

func TestLogPageStartsAtDurableNewestWithoutRing(t *testing.T) {
	s := testStore(t)
	s.Record(&metrics.Record{ID: "durable", Provider: "p", Start: time.UnixMilli(1_800_000_000_000), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(2), s, time.Second)
	w := httptest.NewRecorder()
	api.HandleLogPage(w, httptest.NewRequest(http.MethodGet, "/metrics/agg/log?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("cursorless first archive page: status=%d body=%s", w.Code, w.Body.String())
	}
	page := get(t, http.HandlerFunc(api.HandleLogPage), "/metrics/agg/log?limit=10")
	if rows := page["records"].([]any); len(rows) != 1 || rows[0].(map[string]any)["id"] != "durable" {
		t.Fatalf("empty/late-start ring must not hide durable history: %v", page)
	}
}
