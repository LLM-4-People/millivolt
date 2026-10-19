package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
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

// TestLogPageIncludesEmptyRecordID is the HTTP regression for a stored empty
// id, which the requests schema accepts. The candidate scan returns it as an
// exact key; materialization used to reject that key, so any database holding
// one accepted empty-id row answered /metrics/agg/log with 500 forever.
func TestLogPageIncludesEmptyRecordID(t *testing.T) {
	s := testStoreWithQueryTimeout(t, time.Hour)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range 12 {
		id := fmt.Sprintf("row-%02d", i)
		if i == 2 {
			id = ""
		}
		s.Record(&metrics.Record{ID: id, Provider: "p", Start: start.Add(time.Duration(i) * time.Millisecond), StatusCode: 200})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(1), s, time.Hour).HandleLogPage)

	first := get(t, h, "/metrics/agg/log?limit=10")
	rows := first["records"].([]any)
	if len(rows) != 10 || rows[0].(map[string]any)["id"] != "row-11" || rows[9].(map[string]any)["id"] != "" {
		t.Fatalf("first page = %v, want the empty id at the page boundary", first)
	}
	if first["cursor_ms"] != float64(start.Add(2*time.Millisecond).UnixMilli()) || first["cursor_id"] != "" || first["more"] != true {
		t.Fatalf("first page cursor = %v, want the empty-id boundary and more true", first)
	}

	second := get(t, h, fmt.Sprintf("/metrics/agg/log?limit=10&before_ms=%d&before_id=", start.Add(2*time.Millisecond).UnixMilli()))
	rows = second["records"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["id"] != "row-01" || rows[1].(map[string]any)["id"] != "row-00" || second["more"] != false {
		t.Fatalf("second page = %v, want the rows older than the empty-id cursor", second)
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

// ---------- live-log narrow-scan regression ----------

const (
	logPageRegressionRows        = 200_000
	logPageRegressionStatusEvery = 500
)

// logPageRegressionStart is local noon so the time-bucket case is stable in
// every test timezone: the fixture spans just over three minutes inside the
// "work" bucket.
func logPageRegressionStart() time.Time {
	return time.Date(2024, 1, 1, 12, 0, 0, 0, time.Local)
}

// logPageRegressionRecord is the single owner of the 200k-row fixture shape:
// start times ascend one millisecond apart, every 500th row is a 5xx with an
// error signature, and every third row carries a tool name. No row carries the
// "missing" tool the miss benchmark probes.
func logPageRegressionRecord(i int, base time.Time) *metrics.Record {
	status := 200
	var errType, errCode, errMsg string
	if i%logPageRegressionStatusEvery == 0 {
		status, errType, errCode, errMsg = 500, "upstream", "500", "upstream failed"
	}
	var tools []string
	if i%3 == 0 {
		tools = []string{"search"}
	}
	return &metrics.Record{
		ID: fmt.Sprintf("row-%06d", i), Provider: fmt.Sprintf("provider-%02d", i%10),
		Model: fmt.Sprintf("model-%02d", i%10), Client: fmt.Sprintf("client-%03d", i%500),
		KeyHash: fmt.Sprintf("key-%06d", i), ConversationID: fmt.Sprintf("conv-%06d", i),
		StatusCode: status, ErrorType: errType, ErrorCode: errCode, ErrorMsg: errMsg,
		Start: base.Add(time.Duration(i) * time.Millisecond), ToolNames: tools,
	}
}

// logPageRegressionStore seeds the 200k-row fixture into a private scratch
// store. The write cadence intentionally matches the page benchmark so test
// and benchmark exercise the same committed shape.
func logPageRegressionStore(tb testing.TB) *storage.Store {
	tb.Helper()
	d := config.Default()
	s, err := storage.Open(filepath.Join(tb.TempDir(), "log-page.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond,
		QueryTimeout:  time.Minute,
		WriteTrackCap: logPageRegressionRows * 2,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	base := logPageRegressionStart()
	for i := 0; i < logPageRegressionRows; i++ {
		s.Record(logPageRegressionRecord(i, base))
		if i%2048 == 2047 {
			if err := s.Flush(); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		tb.Fatal(err)
	}
	return s
}

// referenceLogPage is the pre-narrow-scan page algorithm kept as a test
// oracle: full-record decode plus the same scopedMatch and scan discipline.
// The narrow handler must return byte-identical records, cursor and "more"
// signal for every filter class.
func referenceLogPage(t *testing.T, s *storage.Store, before *storage.LogCursor, fs []scopeFilter, statusCode string, limit int, mcz *modelCanonizer) ([]metrics.ObservedRecord, bool, int64, string) {
	t.Helper()
	filter, allPushed := logPageQuery(fs, statusCode, mcz)
	out := make([]*metrics.Record, 0, limit)
	cursor := before
	scanned := 0
	hitEnd := s == nil
	batch := min(limit, logScanBatch)
	if s != nil {
		for scanned < logScanMax && len(out) < limit {
			if allPushed {
				batch = limit - len(out)
			}
			if remain := logScanMax - scanned; remain < batch {
				batch = remain
			}
			if batch <= 0 {
				break
			}
			rows, err := s.QueryBefore(t.Context(), cursor, &filter, batch)
			if err != nil {
				t.Fatalf("reference log page: %v", err)
			}
			if len(rows) == 0 {
				hitEnd = true
				break
			}
			scanned += len(rows)
			for _, rec := range rows {
				if rec == nil {
					continue
				}
				cursor = &storage.LogCursor{StartMs: rec.Start.UnixMilli(), ID: rec.ID}
				c := fromRecord(rec, mcz)
				if !scopedMatch(&c, statusCode, fs) {
					continue
				}
				out = append(out, rec)
				if len(out) >= limit {
					break
				}
			}
			if len(out) >= limit {
				break
			}
			if len(rows) < batch {
				hitEnd = true
				break
			}
			if !allPushed {
				batch = min(batch*2, logScanBatch)
			}
		}
	}
	var cursorMs int64
	var cursorID string
	if cursor != nil {
		cursorMs, cursorID = cursor.StartMs, cursor.ID
	}
	return metrics.ObserveRecords(out), !hitEnd && s != nil, cursorMs, cursorID
}

// logPageWire normalizes an observed-record payload through JSON so it can be
// compared byte-for-byte with the handler's decoded response body.
func logPageWire(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLogPageNarrowScanMatchesFullDecode is the 200k-row semantic and
// allocation regression: Go-owned predicates still scan at most logScanMax
// rows in newest-first order, produce exactly the full-decode page, and pay
// only the narrow candidate projection for rejected rows. The pre-fix path
// measured about 55-72ms and 8.1MB per limit-60 miss on this shape; the
// ceilings below sit well under that while leaving headroom for drivers.
func TestLogPageNarrowScanMatchesFullDecode(t *testing.T) {
	s := logPageRegressionStore(t)
	api := NewAggAPI(metrics.NewBuffer(1), s, time.Minute)
	h := http.HandlerFunc(api.HandleLogPage)
	mcz := api.canonizer()
	for _, tc := range []struct {
		name, query  string
		fs           []scopeFilter
		status       string
		wantRows     int
		wantCursorID string
	}{
		{
			name: "tool_missing", query: "f=tool:missing",
			fs: []scopeFilter{{dim: "tool", id: "missing"}}, wantRows: 0, wantCursorID: "row-195904",
		},
		{
			name: "status_class_5xx", query: "f=status:5xx",
			fs: []scopeFilter{{dim: "status", id: "5xx"}}, wantRows: 8, wantCursorID: "row-195904",
		},
		{
			name: "error_predicate", query: "f=error:" + url.QueryEscape("upstream|500|upstream failed"),
			fs: []scopeFilter{{dim: "error", id: "upstream|500|upstream failed"}}, wantRows: 8, wantCursorID: "row-195904",
		},
		{
			name: "time_window", query: "f=time:work",
			fs: []scopeFilter{{dim: "time", id: "work"}}, wantRows: 60, wantCursorID: "row-199940",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, h, "/metrics/agg/log?limit=60&"+tc.query)
			rows, ok := page["records"].([]any)
			if !ok || len(rows) != tc.wantRows {
				t.Fatalf("records = %v, want %d rows", page["records"], tc.wantRows)
			}
			refRecords, refMore, refCursorMs, refCursorID := referenceLogPage(t, s, nil, tc.fs, tc.status, 60, mcz)
			if got := logPageWire(t, refRecords); !reflect.DeepEqual(page["records"], got) {
				t.Fatalf("narrow page differs from full decode:\n got=%v\nwant=%v", page["records"], got)
			}
			if page["more"] != refMore {
				t.Fatalf("more = %v, want %v", page["more"], refMore)
			}
			if got := int64(page["cursor_ms"].(float64)); got != refCursorMs {
				t.Fatalf("cursor_ms = %d, want %d", got, refCursorMs)
			}
			if page["cursor_id"] != refCursorID || page["cursor_id"] != tc.wantCursorID {
				t.Fatalf("cursor_id = %v, want %v (full decode %v)", page["cursor_id"], tc.wantCursorID, refCursorID)
			}
			if page["more"] != true {
				t.Fatalf("more = %v, want true for a scan-limited/truncated page", page["more"])
			}
		})
	}
	// Allocation ceiling for the two scan-limited Go-only misses. Before this
	// change the same limit-60 pages decoded 4,096 full rows (~8MB/190k
	// allocs); the narrow scan must stay far below that while returning the
	// identical empty page and progress cursor.
	for _, tc := range []struct {
		name, query string
		maxBytes    int64
		maxAllocs   int64
	}{
		{"tool_missing", "/metrics/agg/log?limit=60&f=tool:missing", 5 << 20, 120_000},
		{"status_class_5xx", "/metrics/agg/log?limit=60&f=status:5xx", 5 << 20, 120_000},
	} {
		t.Run("alloc/"+tc.name, func(t *testing.T) {
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
				t.Fatalf("%s allocated %d bytes/op, want at most %d", tc.query, got, tc.maxBytes)
			}
			if got := res.AllocsPerOp(); got > tc.maxAllocs {
				t.Fatalf("%s allocated %d times/op, want at most %d", tc.query, got, tc.maxAllocs)
			}
		})
	}
}

// TestLogPageCanonicalModelRulesUseNarrowMatcher pins the residual matcher for
// configured canonicalization: SQL must not push the raw model equality, the
// Go matcher must still apply the rules, and full records must retain their
// stored spelling. The first page scans the full budget without a match; the
// continuation below its cursor returns the older canonical matches exactly.
func TestLogPageCanonicalModelRulesUseNarrowMatcher(t *testing.T) {
	s := testStoreWithQueryTimeout(t, time.Minute)
	base := logPageRegressionStart()
	for i := 0; i < 5_000; i++ {
		model := "Vendor/Other:fp4"
		if i < 5 {
			model = "Vendor/Target:fp4"
		}
		s.Record(&metrics.Record{
			ID: fmt.Sprintf("row-%04d", i), Provider: "p", Model: model,
			Start: base.Add(time.Duration(i) * time.Millisecond), StatusCode: 200,
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(1), s, time.Minute)
	api.ModelCanon = func() config.ModelCanon { return config.ModelCanon{Rules: config.DefaultModelRules()} }
	h := http.HandlerFunc(api.HandleLogPage)
	mcz := api.canonizer()
	fs := []scopeFilter{{dim: "model", id: "target"}}
	plan := planLogPage(fs, "", mcz)
	if plan.allPushed || len(plan.filter.All) != 0 || len(plan.goFilters) != 1 {
		t.Fatalf("model rules plan = %+v, want the Go matcher to own the model predicate", plan)
	}

	first := get(t, h, "/metrics/agg/log?limit=10&f=model:target")
	if rows := first["records"].([]any); len(rows) != 0 || first["more"] != true || first["cursor_id"] != "row-0904" {
		t.Fatalf("first page = %v, want an empty scan-limited page at row-0904", first)
	}
	refRecords, refMore, refCursorMs, refCursorID := referenceLogPage(t, s, nil, fs, "", 10, mcz)
	if got := logPageWire(t, refRecords); !reflect.DeepEqual(first["records"], got) ||
		first["more"] != refMore || int64(first["cursor_ms"].(float64)) != refCursorMs || first["cursor_id"] != refCursorID {
		t.Fatalf("first narrow page differs from full decode: got=%v ref=%v/%v/%d/%s", first, refRecords, refMore, refCursorMs, refCursorID)
	}

	before := &storage.LogCursor{StartMs: int64(first["cursor_ms"].(float64)), ID: first["cursor_id"].(string)}
	second := get(t, h, fmt.Sprintf("/metrics/agg/log?limit=10&before_ms=%d&before_id=%s&f=model:target", before.StartMs, url.QueryEscape(before.ID)))
	rows := second["records"].([]any)
	if len(rows) != 5 || second["more"] != false || second["cursor_id"] != "row-0000" {
		t.Fatalf("continuation = %v, want the five canonical matches ending at row-0000", second)
	}
	for i, row := range rows {
		if got := row.(map[string]any)["model"]; got != "Vendor/Target:fp4" {
			t.Fatalf("row %d model = %v, want the stored raw spelling", i, got)
		}
	}
	refRecords, refMore, refCursorMs, refCursorID = referenceLogPage(t, s, before, fs, "", 10, mcz)
	if got := logPageWire(t, refRecords); !reflect.DeepEqual(second["records"], got) ||
		second["more"] != refMore || int64(second["cursor_ms"].(float64)) != refCursorMs || second["cursor_id"] != refCursorID {
		t.Fatalf("continuation differs from full decode: got=%v ref=%v/%v/%d/%s", second, refRecords, refMore, refCursorMs, refCursorID)
	}
}

// TestLogPageErrorPredicateMatchesAbsorbedAttempt pins the residual error
// matcher's attempt projection: an error that exists only on an absorbed
// retry (the final response was 200) must still match, exactly as the full
// decode did, without materializing every scanned row.
func TestLogPageErrorPredicateMatchesAbsorbedAttempt(t *testing.T) {
	s := testStoreWithQueryTimeout(t, time.Minute)
	base := logPageRegressionStart()
	attempts := []metrics.RetryAttempt{{
		StatusCode: 503, ErrorType: "overloaded", ErrorCode: "server_busy",
		ErrorMsg: "upstream busy", At: base.Add(-time.Second),
	}}
	s.Record(&metrics.Record{
		ID: "absorbed", Provider: "p", Model: "m", Start: base, StatusCode: 200,
		Attempts: attempts,
	})
	s.Record(&metrics.Record{
		ID: "unrelated", Provider: "p", Model: "m", Start: base.Add(time.Millisecond), StatusCode: 200,
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(1), s, time.Minute)
	h := http.HandlerFunc(api.HandleLogPage)
	fs := []scopeFilter{{dim: "error", id: "overloaded|server_busy|upstream busy"}}
	page := get(t, h, "/metrics/agg/log?limit=10&f=error:"+url.QueryEscape(fs[0].id))
	rows := page["records"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "absorbed" || page["more"] != false {
		t.Fatalf("absorbed-attempt page = %v, want exactly [absorbed] and no more", page)
	}
	refRecords, refMore, refCursorMs, refCursorID := referenceLogPage(t, s, nil, fs, "", 10, api.canonizer())
	if got := logPageWire(t, refRecords); !reflect.DeepEqual(page["records"], got) ||
		page["more"] != refMore || int64(page["cursor_ms"].(float64)) != refCursorMs || page["cursor_id"] != refCursorID {
		t.Fatalf("absorbed-attempt narrow page differs from full decode: got=%v ref=%v/%v/%d/%s", page, refRecords, refMore, refCursorMs, refCursorID)
	}
}
