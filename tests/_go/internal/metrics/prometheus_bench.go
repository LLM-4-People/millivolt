package metrics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

var (
	benchmarkPrometheusBody       string
	benchmarkPrometheusExposition string
)

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
// operator ring and the million-record ceiling. cold advances the finalized
// revision with one append per iteration (which evicts once the ring is full),
// so every scrape snapshots, aggregates, and sorts. cached repeats an
// unchanged scrape, which must write the retained body without that work.
func BenchmarkHandlePrometheus(b *testing.B) {
	for _, records := range []int{1_000, 1_000_000} {
		scrapeBenchmark(b, strconv.Itoa(records)+"/cold", records, true)
		scrapeBenchmark(b, strconv.Itoa(records)+"/cached", records, false)
	}
}

// BenchmarkPrometheusExpositionCached isolates the cache hit at the million
// record ceiling: it must return the retained body without touching the ring
// records or invoking render.
func BenchmarkPrometheusExpositionCached(b *testing.B) {
	buf := benchmarkPrometheusBuffer(b, 1_000_000)
	render := func(records []*Record) (string, error) {
		return renderPrometheusExposition(records)
	}
	if _, err := buf.PrometheusExposition(render); err != nil {
		b.Fatalf("warm build: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		body, err := buf.PrometheusExposition(render)
		if err != nil {
			b.Fatalf("cached scrape: %v", err)
		}
		benchmarkPrometheusExposition = body
	}
}

func scrapeBenchmark(b *testing.B, name string, records int, cold bool) {
	b.Run(name, func(b *testing.B) {
		buf := benchmarkPrometheusBuffer(b, records)
		req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
		w := httptest.NewRecorder()
		// A timed cold iteration must invalidate the cache first; one append
		// advances the finalized revision and (once full) evicts the oldest.
		coldRecord := &Record{Provider: "alpha", StatusCode: 200, TTFTMs: 1}
		if !cold {
			// Warm one build outside the timed region; every iteration below
			// then has an unchanged revision.
			HandlePrometheus(w, req, buf)
			if w.Code != http.StatusOK {
				b.Fatalf("GET /metrics/prometheus = %d, want 200", w.Code)
			}
			w.Body.Reset()
		}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if cold {
				buf.Record(coldRecord)
			}
			w.Body.Reset()
			HandlePrometheus(w, req, buf)
			if w.Code != http.StatusOK {
				b.Fatalf("GET /metrics/prometheus = %d, want 200", w.Code)
			}
			benchmarkPrometheusBody = w.Body.String()
		}
	})
}
