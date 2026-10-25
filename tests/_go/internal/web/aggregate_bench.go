package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// The optional fixture must be a private scratch database, never a live
// instance's database: Open may migrate it. Without it the benchmark uses
// deterministic, provider-neutral contributions and no disk access.
func benchmarkContributions(b *testing.B) []contrib {
	b.Helper()
	if path := os.Getenv("MILLIVOLT_BENCH_DB"); path != "" {
		d := config.Default()
		s, err := storage.Open(path, storage.Options{WriteChanCap: d.StorageWriteChanCap,
			BatchCap: d.StorageBatchCap, FlushInterval: d.StorageFlushInterval, QueryTimeout: d.StorageQueryTimeout})
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		api := NewAggAPI(metrics.NewBuffer(1), s, d.StorageQueryTimeout)
		api.ModelCanon = d.ModelCanon
		var rows []contrib
		var projection *projectionData
		prepare := func(p *projectionData) { projection = p }
		if err := api.streamWindow(context.Background(), 0, api.canonizer(), prepare, func(c *contrib) error { rows = append(rows, *c); return nil }, nil); err != nil {
			b.Fatal(err)
		}
		// The benchmark re-interns these rows into a fresh dictionary, so
		// projected fixtures need their labels restored from the source
		// projection's dictionaries.
		if projection != nil {
			for i := range rows {
				if rows[i].labels == nil {
					rows[i].labels = projectedLabels(projection, &rows[i])
				}
			}
		}
		return rows
	}
	now := time.Now().UnixMilli()
	rows := make([]contrib, 100000)
	for i := range rows {
		// Oldest first: row indexes and starts advance together, so a recent
		// window has high row indexes and can use the retained metric tail.
		start := now - int64(len(rows)-1-i)*1000
		rows[i] = contrib{start: start, status: 200, in: 100, out: 50, ttft: int64(1 + i%3000), tps: float64(1 + i%500),
			labels: &contribLabels{prov: "fixture.example", model: "model", client: "client"}}
	}
	return rows
}

func BenchmarkAggregateFold(b *testing.B) {
	rows := benchmarkContributions(b)
	p := &projectionData{rows: rows}
	for i := range rows {
		rows[i].rowIndex = uint32(i + 1)
		p.dimensions.intern(&rows[i])
		p.noteChunkStart(rows[i].rowIndex, rows[i].start)
	}
	p.metrics.append(rows, 0)
	now := time.Now().UnixMilli()
	min := now
	for _, c := range rows {
		if c.start < min {
			min = c.start
		}
	}
	from, step, count := chartWindowEdges(now, min)
	b.Run("chart", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			f := newChartFold(from, step, count, "", nil)
			f.prepare(p, nil)
			for i := range rows {
				_ = f.fold(&rows[i])
			}
			f.freezeOrder()
			_ = f.payload(now)
		}
	})
	b.Run("explorer", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			f := newExplorerFold("provider", "", nil)
			f.prepare(p, nil)
			for i := range rows {
				_ = f.fold(&rows[i])
			}
			_ = f.payload()
		}
	})
	// A bounded chart window must touch only the blocks whose newest row is
	// inside the window; the visitor owns that skip.
	b.Run("chart_window", func(b *testing.B) {
		since := now - 15*60*1000
		fFrom, fStep, fCount := chartWindowEdges(now, since)
		b.ReportAllocs()
		for b.Loop() {
			f := newChartFold(fFrom, fStep, fCount, "", nil)
			f.prepare(p, nil)
			if err := p.visitSince(b.Context(), since, func(c *contrib) error { return f.fold(c) }); err != nil {
				b.Fatal(err)
			}
			f.freezeOrder()
			_ = f.payload(now)
		}
	})
}

// benchmarkLogStore seeds a private scratch store for live-log page
// benchmarks. The fixture is deterministic, provider-neutral and never a live
// deployment database.
func benchmarkLogStore(b *testing.B, rows int) *storage.Store {
	b.Helper()
	d := config.Default()
	s, err := storage.Open(filepath.Join(b.TempDir(), "log.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond,
		QueryTimeout:  30 * time.Second,
		WriteTrackCap: rows * 2,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	start := time.UnixMilli(1_700_000_000_000)
	for i := range rows {
		s.Record(&metrics.Record{
			ID: fmt.Sprintf("row-%07d", i), Provider: fmt.Sprintf("provider-%02d", i%10),
			Model: fmt.Sprintf("model-%02d", i%10), Client: fmt.Sprintf("client-%06d", i),
			KeyHash: fmt.Sprintf("key-%06d", i), ConversationID: fmt.Sprintf("conv-%06d", i),
			StatusCode: 200, Start: start.Add(time.Duration(i) * time.Millisecond),
			DurationMs: 10, TTFTMs: 1, FinishReason: "stop",
			Usage:  metrics.Usage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
			Method: "POST", Path: "/v1/chat/completions",
		})
		if i%2048 == 2047 {
			if err := s.Flush(); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkHandleLogPage measures the two pushdown classes at the page limits
// the dashboard uses. SQL-pushed scopes request only the remaining rows; Go-only
// predicates grow geometrically from that page size to the scan cap.
func BenchmarkHandleLogPage(b *testing.B) {
	s := benchmarkLogStore(b, 200_000)
	api := NewAggAPI(metrics.NewBuffer(1), s, 30*time.Second)
	h := http.HandlerFunc(api.HandleLogPage)
	cases := []struct {
		name, query string
	}{
		{"empty", "/metrics/agg/log?limit=%d"},
		{"sql_provider", "/metrics/agg/log?limit=%d&f=provider:provider-03"},
		{"go_status_class", "/metrics/agg/log?limit=%d&f=status:2xx"},
		{"go_status_class_5xx", "/metrics/agg/log?limit=%d&f=status:5xx"},
		{"go_tool_missing", "/metrics/agg/log?limit=%d&f=tool:missing"},
	}
	for _, limit := range []int{60, 200} {
		for _, tc := range cases {
			b.Run(fmt.Sprintf("%s/limit_%d", tc.name, limit), func(b *testing.B) {
				url := fmt.Sprintf(tc.query, limit)
				b.ReportAllocs()
				for b.Loop() {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
					if w.Code != http.StatusOK {
						b.Fatalf("%s status=%d body=%s", url, w.Code, w.Body.String())
					}
				}
			})
		}
	}
}

// benchmarkMetricRows builds oldest-first projection rows with valid metrics
// so a delta append exercises every retained order.
func benchmarkMetricRows(n int) []contrib {
	rows := make([]contrib, n)
	for i := range rows {
		v := int64((i*7919)%n + 1)
		rows[i] = contrib{ttft: v, tps: float64(v), dec: v}
	}
	return rows
}

var benchmarkProjectionMetrics projectionMetrics

// BenchmarkProjectionMetricAppend measures one small catch-up append against a
// retained history. The base is built before the timer; each iteration starts
// from that same base so repeated deltas never cross a compaction boundary.
func BenchmarkProjectionMetricAppend(b *testing.B) {
	for _, history := range []int{100_000, 1_000_000} {
		b.Run(fmt.Sprintf("history_%d", history), func(b *testing.B) {
			rows := benchmarkMetricRows(history + 16)
			var base projectionMetrics
			base.append(rows[:history], 0)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				m := base
				m.append(rows, history)
				benchmarkProjectionMetrics = m
			}
		})
	}
	for _, history := range []int{100_000, 1_000_000} {
		b.Run(fmt.Sprintf("compact_%d", history), func(b *testing.B) {
			benchmarkProjectionCompaction(b, history)
		})
	}
}

// benchmarkProjectionCompaction times one threshold-triggered base+pending
// fold. Setup leaves pending at threshold-1 samples and the measured append
// adds the sample that crosses the bound.
func benchmarkProjectionCompaction(b *testing.B, history int) {
	// 5461 fully populated rows hold 16383 pending samples; the measured row
	// adds three more and folds all three base orders at once.
	pendingRows := (metricOrderPendingSamples - 1) / 3
	rows := benchmarkMetricRows(history + pendingRows + 1)
	var base projectionMetrics
	base.append(rows[:history], 0)
	base.append(rows[:len(rows)-1], history)
	if got := base.pending.sampleCount(); got != 3*pendingRows {
		b.Fatalf("pending samples = %d, want %d", got, 3*pendingRows)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m := base
		m.append(rows, len(rows)-1)
		benchmarkProjectionMetrics = m
	}
}

// BenchmarkAggregateFlightCoalesce measures N concurrent callers of one cold
// chart key. The hooks hold the leader until every waiter has joined, so each
// iteration measures the coalesced path: one fold, N-1 waits, then the shared
// publication. Only the memo slots are reset, so no preload or fixture is
// involved. Allocations are per coalesced batch of N requests.
// BenchmarkProjectionVisitSinceCancellation measures how quickly an abandoned
// fold observes cancellation relative to walking the same history. The canceled
// sub-benchmark stops at the next ctxCheckRows block boundary instead of
// completing the 200k-row visit.
func BenchmarkProjectionVisitSinceCancellation(b *testing.B) {
	rows := benchmarkMetricRows(200_000)
	p := &projectionData{rows: rows}
	b.Run("full_history", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := p.visitSince(context.Background(), 0, func(*contrib) error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("canceled_after_1024", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx, cancel := context.WithCancel(context.Background())
			visited := 0
			err := p.visitSince(ctx, 0, func(*contrib) error {
				visited++
				if visited == 1_024 {
					cancel()
				}
				return nil
			})
			cancel()
			if !errors.Is(err, context.Canceled) {
				b.Fatalf("canceled fold returned %v", err)
			}
			if visited > 1_024+ctxCheckRows {
				b.Fatalf("canceled fold visited %d rows, want at most %d", visited, 1_024+ctxCheckRows)
			}
		}
	})
}

// BenchmarkMetricOrderViewRead measures the chart/explorer rank reader over an
// unchanged published view. It walks base+pending as one exact order; the
// remaining B/op is the bounded rank state, never a history-sized merge.
func BenchmarkMetricOrderViewRead(b *testing.B) {
	for _, history := range []int{50_000, 200_000} {
		b.Run(fmt.Sprintf("history_%d", history), func(b *testing.B) {
			rows := benchmarkMetricRows(history + 16)
			var m projectionMetrics
			m.append(rows[:history], 0)
			m.append(rows, history)
			source := m.allOrderSource()
			mask := make([]uint8, len(rows))
			counts := make([]int, 8)
			for i := range mask {
				if !validMetricSample(rows[i].ttft) {
					continue
				}
				bucket := uint8(i%len(counts) + 1)
				mask[i] = bucket
				counts[bucket-1]++
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, _, _ = rankPercentilesStatsMerge(source.base.ttft, source.pending.ttft, mask, counts, nil, nil)
				_, _, _ = rankPercentilesStatsMerge(source.base.tps, source.pending.tps, mask, counts, nil, nil)
				_, _ = rankPercentilesMerge(source.base.dec, source.pending.dec, mask, counts, nil, nil)
			}
		})
	}
}

func BenchmarkAggregateFlightCoalesce(b *testing.B) {
	const callers = 8
	now := time.Now()
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("flight", now.Add(-time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	b.ReportAllocs()
	for b.Loop() {
		api.chartMemo.mu.Lock()
		api.chartMemo.entry = nil
		api.chartMemo.flight = nil
		api.chartMemo.mu.Unlock()

		var folds, waits atomic.Int64
		allJoined := make(chan struct{})
		api.testHooks = &aggregateTestHooks{
			cold: func() {
				if folds.Add(1) == 1 {
					select {
					case <-allJoined:
					case <-time.After(5 * time.Second):
					}
				}
			},
			wait: func() {
				if waits.Add(1) == callers-1 {
					close(allJoined)
				}
			},
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(callers)
		for range callers {
			go func() {
				defer wg.Done()
				<-start
				if _, err := api.chart(b.Context(), 60, nil, "", now.UnixMilli()); err != nil {
					b.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if folds.Load() != 1 || waits.Load() != callers-1 {
			b.Fatalf("folds = %d, waits = %d; want 1, %d", folds.Load(), waits.Load(), callers-1)
		}
	}
	b.ReportMetric(1, "folds/op")
	b.ReportMetric(callers-1, "waits/op")
}

func benchmarkExplorerProjection(rows []contrib) *projectionData {
	p, err := buildProjection(rows, labelTuples(rows), storage.ProjectionCursor{}, 1)
	if err != nil {
		panic(err)
	}
	return &p
}

// BenchmarkExplorerLineageTopology measures a warm explorer request over a
// projection that already owns the resolved conversation graph. The per-request
// work is scoped matching, rail/gallery flags and the bounded payload, not a
// rebuild of every conversation node.
func BenchmarkExplorerLineageTopology(b *testing.B) {
	for _, conversations := range []int{4_000, 100_000, 200_000} {
		b.Run(fmt.Sprintf("conversations_%d", conversations), func(b *testing.B) {
			rows := make([]contrib, conversations)
			for i := range rows {
				parent := ""
				if i%7 == 0 && i > 0 {
					parent = fmt.Sprintf("s:%d", i-1)
				}
				rows[i] = contrib{
					start: int64(i), status: 200,
					labels: &contribLabels{
						client: "fixture-client", key: "fixture-key",
						prov: "fixture.example", model: "fixture-model",
						conv: fmt.Sprintf("s:%d", i), parentConv: parent,
					},
				}
			}
			p := benchmarkExplorerProjection(rows)
			mcz := &modelCanonizer{memo: map[string]string{}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				fold := newExplorerFold("provider", "", nil)
				fold.prepare(p, mcz)
				for i := range p.rows {
					if err := fold.fold(&p.rows[i]); err != nil {
						b.Fatal(err)
					}
				}
				_ = fold.payload()
			}
		})
	}
}

// BenchmarkExplorerCanonicalModels measures the steady-state query dictionary
// preparation. The cold first request after a projection/rules change pays the
// canonicalization once; every subsequent revision-compatible request reuses
// the projection-owned canonical IDs and dictionary.
func BenchmarkExplorerCanonicalModels(b *testing.B) {
	rules := config.Default().ModelCanon()
	exec := config.CompileModelRules(rules.Rules)
	revision := modelCanonIdentity(rules)
	for _, models := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("models_%d", models), func(b *testing.B) {
			rows := make([]contrib, models)
			for i := range rows {
				rows[i] = contrib{labels: &contribLabels{model: fmt.Sprintf("vendor.example/Model-%d.5:tag-fp4", i)}, status: 200}
			}
			p := benchmarkExplorerProjection(rows)
			mcz := &modelCanonizer{exec: exec, identity: revision, memo: map[string]string{}}
			var warm queryDimensions
			warm.prepare(p, mcz)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var q queryDimensions
				q.prepare(p, mcz)
			}
		})
	}
}

// Unlike the fold kernel, this includes revision checks, query preparation,
// ring/pending merge and payload construction. Clear only the result memo on
// each iteration: it measures a previously unseen view, not a warm cache hit.
// Preload is startup work and explicitly outside the query measurement.
func BenchmarkAggregateUncached(b *testing.B) {
	path := os.Getenv("MILLIVOLT_BENCH_DB")
	if path == "" {
		b.Skip("set MILLIVOLT_BENCH_DB to a private scratch fixture")
	}
	d := config.Default()
	s, err := storage.Open(path, storage.Options{WriteChanCap: d.StorageWriteChanCap, BatchCap: d.StorageBatchCap, FlushInterval: d.StorageFlushInterval, QueryTimeout: d.StorageQueryTimeout})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	api := NewAggAPI(metrics.NewBuffer(d.HistorySize), s, d.StorageQueryTimeout)
	api.ModelCanon = d.ModelCanon
	if err := api.PreloadHistory(b.Context()); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixMilli()
	b.Run("chart", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			api.chartMemo.entry = nil
			if _, err := api.chart(b.Context(), 0, nil, "", now); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("explorer", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			api.explorerMemo.entry = nil
			if _, err := api.explorer(b.Context(), "provider", nil, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
