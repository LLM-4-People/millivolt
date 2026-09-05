package web

import (
	"context"
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
		rows[i] = contrib{start: now - int64(i)*1000, status: 200, in: 100, out: 50, ttft: int64(1 + i%3000), tps: float64(1 + i%500), prov: "fixture.example", model: "model", client: "client"}
	}
	return rows
}

func BenchmarkAggregateFold(b *testing.B) {
	rows := benchmarkContributions(b)
	p := &projectionData{rows: rows}
	for i := range rows {
		rows[i].rowIndex = uint32(i + 1)
		p.dimensions.intern(&rows[i])
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
