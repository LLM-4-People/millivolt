package web

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

const kpiBenchRecords = 10000

// benchmarkKPIFixture seeds the full default-path shape the bootstrap poll
// folds: a 10k finalized ring whose records are all already durable, so the
// KPI path copies the ring, probes every written id, and adds no ring extras.
func benchmarkKPIFixture(b *testing.B) *AggAPI {
	b.Helper()
	d := config.Default()
	s, err := storage.Open(filepath.Join(b.TempDir(), "kpi-bench.db"), storage.Options{
		WriteChanCap:  2 * kpiBenchRecords,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Hour, // explicit Flush owns durable commit timing
		QueryTimeout:  d.StorageQueryTimeout,
		WriteTrackCap: 2 * kpiBenchRecords,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	records := make([]*metrics.Record, kpiBenchRecords)
	for i := range records {
		records[i] = &metrics.Record{
			ID:         fmt.Sprintf("kpi-%d", i),
			Provider:   "fixture.example",
			Model:      "fixture-model",
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
			StatusCode: 200,
			TTFTMs:     int64(1 + i%500),
			Cost:       0.0001,
			Usage:      metrics.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10},
		}
	}
	for _, r := range records {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}
	if got := s.Totals().Requests; got != kpiBenchRecords {
		b.Fatalf("seed totals = %d, want %d (drops=%d)", got, kpiBenchRecords, s.Dropped())
	}
	buf := metrics.NewBuffer(kpiBenchRecords)
	buf.Backfill(records)
	return NewAggAPI(buf, s, d.StorageQueryTimeout)
}

// BenchmarkKPI measures the bootstrap KPI fold at a 10k-record ring with every
// record durable. "miss" constructs a fresh API per iteration (the pre-memo
// cost), "hit" reuses the exact-revision memo. The differential is the cost
// every 5s-per-tab poll no longer pays.
func BenchmarkKPI(b *testing.B) {
	api := benchmarkKPIFixture(b)
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			fresh := NewAggAPI(api.buf, api.store, api.timeout)
			_ = fresh.kpi()
		}
	})
	b.Run("hit", func(b *testing.B) {
		_ = api.kpi() // publish the exact-revision entry before timing
		b.ReportAllocs()
		for b.Loop() {
			_ = api.kpi()
		}
	})
}
