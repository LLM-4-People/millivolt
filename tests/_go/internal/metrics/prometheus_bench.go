package metrics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

var benchmarkPrometheusBody string

// benchmarkPrometheusBuffer seeds a wrapped-free ring with the mixed shape the
// exposition folds: four providers, sparse TTFTs, occasional 500s and
// zero-token records. Setup is excluded from the timed region.
func benchmarkPrometheusBuffer(b *testing.B, records int) *Buffer {
	b.Helper()
	buf := NewBuffer(records)
	providers := [...]string{"alpha", "beta", "gamma", "delta"}
	for i := range records {
		r := &Record{
			Provider:   providers[i%len(providers)],
			Model:      "fixture-model",
			StatusCode: 200,
			TTFTMs:     int64(1 + i%500),
			Usage: Usage{
				InputTokens:     100,
				OutputTokens:    50,
				CacheReadTokens: 10,
			},
		}
		if i%97 == 0 {
			r.StatusCode = 500
			r.ErrorType = "provider_error"
		}
		if i%53 == 0 {
			r.TTFTMs = 0
		}
		buf.Record(r)
	}
	return buf
}

// BenchmarkHandlePrometheus reports the full exposition cost at the small
// operator ring and the million-record ceiling. The aggregate owns both the
// per-provider counts and the flat TTFT samples, so the 1M case allocates the
// snapshot, one growing flat sample slice, and the response body - never
// another ring walk, per-provider sample slices, or a second flattened copy.
func BenchmarkHandlePrometheus(b *testing.B) {
	for _, records := range []int{1_000, 1_000_000} {
		b.Run(strconv.Itoa(records), func(b *testing.B) {
			buf := benchmarkPrometheusBuffer(b, records)
			req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
			w := httptest.NewRecorder()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				w.Body.Reset()
				HandlePrometheus(w, req, buf)
				if w.Code != http.StatusOK {
					b.Fatalf("GET /metrics/prometheus = %d, want 200", w.Code)
				}
				benchmarkPrometheusBody = w.Body.String()
			}
		})
	}
}
