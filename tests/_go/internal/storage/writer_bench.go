package storage

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// BenchmarkStorageWriter measures newly inserted finalized records, including
// their real indexes/triggers, JSON fields, durable totals, and WAL commits.
// Every iteration uses new IDs: REPLACE of an existing small working set would
// measure a different workload. Queued publishes at most one queue capacity
// before Flush, so a faster producer cannot turn apparent throughput into drops.
func BenchmarkStorageWriter(b *testing.B) {
	for _, queued := range []bool{false, true} {
		name := "batch"
		if queued {
			name = "queued"
		}
		b.Run(name, func(b *testing.B) {
			opts := testOpts
			opts.WriteTrackCap = 2 * opts.WriteChanCap
			s, err := Open(filepath.Join(b.TempDir(), "writer.db"), opts)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := s.Close(); err != nil {
					b.Error(err)
				}
			})
			count := opts.BatchCap
			if queued {
				count = opts.WriteChanCap
			}
			records := make([]*metrics.Record, count)
			cost := 0.001
			for i := range records {
				records[i] = &metrics.Record{
					Provider: "fixture.example", Model: "fixture-model", Client: "writer-fixture",
					Start: time.Unix(1700000000, 0), Stream: true, StatusCode: 200,
					DurationMs: 10, TTFTMs: 1, FinishReason: "stop", Cost: cost,
					Usage:  metrics.Usage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
					Method: "POST", Path: "/v1/chat/completions", TurnsUser: 1, CharsUser: 32,
					ResponseHeaders: map[string][]string{"Content-Type": {"text/event-stream"}},
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := range b.N {
				for i, record := range records {
					record.ID = strconv.Itoa(iteration*count + i)
				}
				if queued {
					for _, record := range records {
						s.Record(record)
					}
					err = s.Flush()
				} else {
					err = s.insertBatch(records)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			var got int64
			if err := s.rdb.QueryRow("SELECT count(*) FROM requests").Scan(&got); err != nil {
				b.Fatal(err)
			}
			if want := int64(b.N) * int64(count); got != want || s.Dropped() != 0 {
				b.Fatalf("durable=%d want=%d dropped=%d", got, want, s.Dropped())
			}
			b.ReportMetric(float64(got)/b.Elapsed().Seconds(), "records/s")
		})
	}
}
