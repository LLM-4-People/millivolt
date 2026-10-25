package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPrometheusColdFoldAllocationCeiling bounds the cold scrape's per-record
// work. Every iteration advances the finalized revision with one append (which
// evicts once the ring is full) so the cache cannot serve a warmed body: the
// snapshot, fold and sort run each time. The record count is capped below the
// benchmark's million-record probe so the gate stays affordable; the fold is
// linear. The allocation ceiling catches per-record temporaries: a
// fmt.Sprintf added to aggregateInto allocates once per record, multiplying
// allocations by 100k and failing the 200-alloc ceiling in every build.
//
// A fixed non-per-record byte ceiling is deliberately not asserted. Under
// -race, sync.Pool drops one in four Puts (src/sync/pool.go), so the pooled
// 800 KB sample slice is reallocated in a random subset of scrapes; with only
// 9-16 iterations per run, each miss adds about 80 KB to B/op. Race runs
// measured 892,008..1,146,556 B/op over the first 20 samples, 1,213,506 by 30
// and 1,294,087 by 70 (p95 1,271,705), so the tail grows with the sample rather
// than settling. A 1 MiB per-scrape sink measures 2,061,285..2,204,327 B/op,
// only about 0.77 MB above the worst observed baseline. The lowest ceiling with
// 20% headroom over that baseline is about 1.55 MB, under 0.51 MB below the
// sink and still inside the pool-drop tail, so it would flake before it could
// reliably reject the sink. Residual gap: one fixed allocation per scrape with
// fewer than about 140 allocations passes here; per-record and per-read
// regressions still fail the allocation ceiling.
func TestPrometheusColdFoldAllocationCeiling(t *testing.T) {
	const records = 100_000
	result := testing.Benchmark(func(b *testing.B) {
		buf := benchmarkPrometheusBuffer(b, records)
		req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
		w := httptest.NewRecorder()
		cold := &Record{Provider: "alpha", StatusCode: 200, TTFTMs: 1}
		b.ReportAllocs()
		for b.Loop() {
			buf.Record(cold)
			w.Body.Reset()
			HandlePrometheus(w, req, buf)
			if w.Code != http.StatusOK {
				b.Fatalf("GET /metrics/prometheus = %d, want 200", w.Code)
			}
			benchmarkPrometheusBody = w.Body.String()
		}
	})
	t.Logf("cold Prometheus fold = %d B/op %d allocs/op (N=%d)",
		result.AllocedBytesPerOp(), result.AllocsPerOp(), result.N)
	if got := result.AllocsPerOp(); got > 200 {
		t.Fatalf("cold Prometheus fold = %d allocs/op, want <= 200", got)
	}
}
