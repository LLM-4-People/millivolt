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
// linear. The 100k pointer snapshot alone is 800 KB, and the measured cold
// fold is about 825 KB/op, so the 1,000,000 B/op ceiling leaves roughly 21%
// headroom; a per-record temporary allocation (for example fmt.Sprintf added
// to aggregateInto) both adds about 325 KB and multiplies allocations by 100k,
// failing the 200-alloc ceiling.
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
		if got := result.AllocedBytesPerOp(); got > 1_000_000 {
			t.Fatalf("run %d: cold Prometheus fold = %d B/op, want <= 1000000", run, got)
		}
	}
}
