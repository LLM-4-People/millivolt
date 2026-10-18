package storage

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

const benchLegacyIndexRows = 10_000

// schemaRequestIndexNames lists every named index the startup schema sequence
// created on requests. The legacy benchmark strips exactly this set instead of
// keeping a second copy of the DDL, so a renamed or added index cannot quietly
// stop being rebuilt by the measured Open.
func schemaRequestIndexNames(tb testing.TB, db *sql.DB) []string {
	tb.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'requests' AND sql IS NOT NULL
		ORDER BY name`)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			tb.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return names
}

// benchLegacyIndexSeed seeds a current-schema database, checkpoints it, then
// removes every named requests index and the planner statistics table on a raw
// connection. Each measured Open therefore runs the schema's index creation,
// migrate and ensureRequestStatistics work a legacy database needs.
func benchLegacyIndexSeed(b *testing.B) (string, []string) {
	b.Helper()
	seed := filepath.Join(b.TempDir(), "legacy-index-seed.db")
	s, err := Open(seed, testOpts)
	if err != nil {
		b.Fatal(err)
	}
	seedQueryBeforeRows(b, s, benchLegacyIndexRows, benchQueryBeforeProviders, benchQueryBeforeModels)
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}

	raw, err := sql.Open("sqlite", seed)
	if err != nil {
		b.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		b.Fatal(err)
	}
	names := schemaRequestIndexNames(b, raw)
	if len(names) == 0 {
		b.Fatal("legacy seed has no named requests indexes to strip; the schema contract is no longer exercised")
	}
	for _, name := range names {
		if _, err := raw.Exec(`DROP INDEX IF EXISTS "` + strings.ReplaceAll(name, `"`, `""`) + `"`); err != nil {
			b.Fatalf("drop index %s: %v", name, err)
		}
	}
	if _, err := raw.Exec("DROP TABLE IF EXISTS sqlite_stat1"); err != nil {
		b.Fatalf("drop planner statistics: %v", err)
	}
	var remaining int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'requests' AND sql IS NOT NULL`).Scan(&remaining); err != nil {
		b.Fatal(err)
	}
	if remaining != 0 {
		b.Fatalf("legacy seed still carries %d named requests indexes", remaining)
	}
	if _, err := raw.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		b.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		b.Fatal(err)
	}
	return seed, names
}

// BenchmarkOpenLegacyIndexBuild measures Open on a 10k-row database whose named
// requests indexes and planner statistics were stripped: migrate's index
// creation, the rest of migrate and ensureRequestStatistics (ANALYZE) run
// inside the timer. Seed construction and per-iteration file copies stay
// outside it.
func BenchmarkOpenLegacyIndexBuild(b *testing.B) {
	b.StopTimer()
	seed, wantIndexes := benchLegacyIndexSeed(b)
	dir := b.TempDir()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, fmt.Sprintf("legacy-index-open-%d.db", i))
		b.StopTimer()
		copyFile(b, seed, path)
		b.StartTimer()
		s, err := Open(path, testOpts)
		if err != nil {
			b.Fatal(err)
		}
		if got := s.Totals().Requests; got != benchLegacyIndexRows {
			b.Fatalf("totals = %d, want %d", got, benchLegacyIndexRows)
		}
		gotIndexes := schemaRequestIndexNames(b, s.rdb)
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		// The measured Open must rebuild exactly the indexes the seed stripped,
		// or the benchmark silently stops exercising the schema's index work.
		if !slices.Equal(gotIndexes, wantIndexes) {
			b.Fatalf("rebuilt indexes = %v, want %v", gotIndexes, wantIndexes)
		}
		if err := os.Remove(path); err != nil {
			b.Fatal(err)
		}
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
