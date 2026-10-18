package web

import (
	"context"
	"fmt"
	"os"
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
		if err := api.streamWindow(context.Background(), 0, api.canonizer(), nil, func(c *contrib) error { rows = append(rows, *c); return nil }, nil); err != nil {
			b.Fatal(err)
		}
		return rows
	}
	now := time.Now().UnixMilli()
	rows := make([]contrib, 100000)
	for i := range rows {
		// Oldest first: row indexes and starts advance together, so a recent
		// window has high row indexes and can use the retained metric tail.
		start := now - int64(len(rows)-1-i)*1000
		rows[i] = contrib{start: start, status: 200, in: 100, out: 50, ttft: int64(1 + i%3000), tps: float64(1 + i%500), prov: "fixture.example", model: "model", client: "client"}
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
			if err := p.visitSince(since, func(c *contrib) error { return f.fold(c) }); err != nil {
				b.Fatal(err)
			}
			f.freezeOrder()
			_ = f.payload(now)
		}
	})
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
