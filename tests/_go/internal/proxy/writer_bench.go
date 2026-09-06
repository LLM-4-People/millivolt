package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// BenchmarkProxyDurableContention measures full metadata, lifecycle, stream
// analysis, ring publication and the durable queue. Transport is in-process
// and performs no socket or provider access. Unlike BenchmarkStorageWriter,
// producers deliberately overload the bounded queue; durable rows plus the
// reported drops must exactly equal successful requests. Compare both rates,
// never infer durable throughput from successful response throughput alone.
func BenchmarkProxyDurableContention(b *testing.B) {
	for _, workers := range []int{32, 128} {
		b.Run(fmt.Sprintf("workers%d", workers), func(b *testing.B) {
			cfg := config.Default()
			store, err := storage.Open(filepath.Join(b.TempDir(), "writer.db"), storage.Options{
				WriteChanCap: cfg.StorageWriteChanCap, BatchCap: cfg.StorageBatchCap,
				FlushInterval: cfg.StorageFlushInterval, QueryTimeout: cfg.StorageQueryTimeout,
				WriteTrackCap: 2 * cfg.HistorySize,
			})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { store.Close() })
			p := New(cfg, store.Recorder(metrics.NewBuffer(cfg.HistorySize)))
			payload := benchmarkRequestBody(29)
			response := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"hello world \"}}]}\n\n", 10) +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110,\"cost\":0.001}}\n\ndata: [DONE]\n\n"
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Date": {"Sat, 05 Sep 2026 22:00:00 GMT"}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
			})})
			var failures atomic.Int64
			var wg sync.WaitGroup
			b.ReportAllocs()
			b.ResetTimer()
			begin := time.Now()
			for worker := range workers {
				wg.Go(func() {
					for i := worker; i < b.N; i += workers {
						r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
						r.Header.Set("Content-Type", "application/json")
						r.Header.Set("Authorization", "Bearer fixture-secret")
						r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
						r.Header.Set("X-Proxy-Client", "synthetic-benchmark-128")
						w := &benchmarkResponseSink{header: make(http.Header)}
						p.ServeHTTP(w, r)
						if w.status != 200 || w.bytes != len(response) {
							failures.Add(1)
						}
					}
				})
			}
			wg.Wait()
			wireElapsed := time.Since(begin)
			if err := store.Flush(); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			n, err := store.CountWhere(b.Context(), storage.PurgeFilter{})
			if err != nil {
				b.Fatal(err)
			}
			if failures.Load() != 0 || n+int64(store.Dropped()) != int64(b.N) {
				b.Fatalf("failures=%d rows=%d drops=%d total=%d", failures.Load(), n, store.Dropped(), b.N)
			}
			b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "durable/s")
			b.ReportMetric(float64(store.Dropped()), "dropped")
			b.ReportMetric(float64(b.N)/wireElapsed.Seconds(), "wire/s")
		})
	}
}
