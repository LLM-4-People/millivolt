package web

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// BenchmarkAggregateColdRows measures the cold durable-row materialization
// path: scanContrib decodes every stored row, fromRecord-shaped contributions
// are interned into the projection dictionaries, and the metric orders are
// appended. BenchmarkAggregateFold feeds pre-projected rows with mcz == nil, so
// it never executes any of it; BenchmarkAggregateUncached skips unless
// MILLIVOLT_BENCH_DB is set. This benchmark seeds its own private scratch
// database through the shared footprint fixture and builds a fresh AggAPI per
// iteration, so a run needs no external fixture and every iteration is cold.
func BenchmarkAggregateColdRows(b *testing.B) {
	for _, n := range [...]int{1_000, 10_000} {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			store, _, _ := footprintStore(b, n)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				agg := NewAggAPI(metrics.NewBuffer(4), store, time.Minute)
				var rows int
				if err := agg.withProjectionBudget(ctx, projectionMaintenance, func(p *projectionData) error {
					if p != nil {
						rows = len(p.rows)
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if rows != n {
					b.Fatalf("cold projection materialized %d rows, want %d", rows, n)
				}
			}
		})
	}
}

// BenchmarkFromRecord measures one ring contribution's materialization (the
// same shape the cold durable path pays, without the column decodes). Ring rows
// keep their live label sidecar by contract, so this is the irreducible
// per-row cost for unflushed records.
func BenchmarkFromRecord(b *testing.B) {
	const n = 1_000
	recs := make([]*metrics.Record, n)
	for i := range recs {
		recs[i] = footprintRecord(i)
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, r := range recs {
			c := fromRecord(r, nil)
			if c.id == "" {
				b.Fatal("fromRecord lost the request id")
			}
		}
	}
}
