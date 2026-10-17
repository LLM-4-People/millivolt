package storage

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func copyFile(tb testing.TB, src, dst string) {
	tb.Helper()
	in, err := os.Open(src)
	if err != nil {
		tb.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		tb.Fatal(err)
	}
	if err := out.Close(); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkOpenTotals isolates the boot aggregate path: one durable meta row
// versus the pre-cache full request scan on the same 50k-row database. File
// materialization runs outside the timer.
func BenchmarkOpenTotals(b *testing.B) {
	dir := b.TempDir()
	seed := filepath.Join(dir, "seed.db")
	s, err := Open(seed, testOpts)
	if err != nil {
		b.Fatal(err)
	}
	const rows = 50000
	batch := make([]*metrics.Record, 0, 256)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range rows {
		batch = append(batch, &metrics.Record{
			ID: fmt.Sprintf("bench-%06d", i), Provider: "p", Model: "m", StatusCode: 200,
			Start:  start.Add(time.Duration(i) * time.Millisecond),
			Usage:  metrics.Usage{InputTokens: int64(i % 500), OutputTokens: int64(i % 200)},
			TTFTMs: int64(1 + i%300), OverallTPS: float64(1 + i%100),
		})
		if len(batch) == cap(batch) {
			if err := s.insertBatch(batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := s.insertBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	raw, err := sql.Open("sqlite", seed)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		b.Fatal(err)
	}
	raw.Close()

	for _, mode := range []string{"persisted", "rescan"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				path := filepath.Join(dir, fmt.Sprintf("%s-%d.db", mode, i))
				copyFile(b, seed, path)
				if mode == "rescan" {
					raw, err := sql.Open("sqlite", path)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := raw.Exec(`DELETE FROM meta WHERE key = ?`, totalsMetaKey); err != nil {
						b.Fatal(err)
					}
					raw.Close()
				}
				b.StartTimer()
				st, err := Open(path, testOpts)
				if err != nil {
					b.Fatal(err)
				}
				if got := st.Totals().Requests; got != rows {
					b.Fatalf("totals = %d, want %d", got, rows)
				}
				if err := st.Close(); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				os.Remove(path)
			}
		})
	}
}
