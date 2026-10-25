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
// linear. The byte figure is race-instrumented: -race shadow memory roughly
// doubles it, so it is logged for diagnosis, not asserted. The allocation
// ceiling is the invariant that catches per-record temporaries: a
// fmt.Sprintf added to aggregateInto allocates once per record, multiplying
// allocations by 100k and failing the 200-alloc ceiling in every build.
func TestPrometheusColdFoldAllocationCeiling(t *testing.T) {
	const records = 100_000
	for run := 0; run < 5; run++ {
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
		t.Logf("run %d: %d B/op %d allocs/op", run, result.AllocedBytesPerOp(), result.AllocsPerOp())
		if got := result.AllocsPerOp(); got > 200 {
			t.Fatalf("run %d: cold Prometheus fold = %d allocs/op, want <= 200", run, got)
		}
	}
}
