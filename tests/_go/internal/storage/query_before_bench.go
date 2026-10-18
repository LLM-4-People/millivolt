package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const (
	queryBeforeFixtureStartMs = 1_700_000_000_000
	queryBeforeSeedChunk      = 1_000
	benchQueryBeforeRows      = 100_000
	benchQueryBeforePage      = 200
	benchQueryBeforeProviders = 10
	benchQueryBeforeModels    = 10
)

// seedQueryBeforeRows fills a store with n rows whose start times ascend one
// millisecond apart and whose provider/model values cycle independently, so a
// provider or model equality matches n/providers or n/models rows.
func seedQueryBeforeRows(t testing.TB, s *Store, n, providers, models int) {
	t.Helper()
	batch := make([]*metrics.Record, 0, queryBeforeSeedChunk)
	start := time.UnixMilli(queryBeforeFixtureStartMs)
	for i := 0; i < n; i++ {
		batch = append(batch, &metrics.Record{
			ID:         fmt.Sprintf("row-%06d", i),
			Provider:   fmt.Sprintf("provider-%02d", i%providers),
			Model:      fmt.Sprintf("model-%02d", (i/providers)%models),
			KeyHash:    "k",
			StatusCode: 200,
			Start:      start.Add(time.Duration(i) * time.Millisecond),
		})
		if len(batch) == cap(batch) {
			if err := s.insertBatch(batch); err != nil {
				t.Fatalf("seed batch at %d: %v", i, err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := s.insertBatch(batch); err != nil {
			t.Fatalf("seed final batch: %v", err)
		}
	}
}

// benchmarkQueryBeforeStore builds the 100k-row fixture, closes it once and
// reopens it so planner statistics reflect the real row distribution and the
// measured query starts from a production-shaped boot.
func benchmarkQueryBeforeStore(b *testing.B) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "query-before-bench.db")
	s, err := Open(path, testOpts)
	if err != nil {
		b.Fatal(err)
	}
	seedQueryBeforeRows(b, s, benchQueryBeforeRows, benchQueryBeforeProviders, benchQueryBeforeModels)
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	s, err = Open(path, testOpts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// BenchmarkQueryBeforeEquality is an end-to-end page benchmark: it measures
// the exact 200-row page the dashboard serves, including scanRecord decode, for
// an exact provider and raw-model equality, with and without the exclusive
// cursor. It is not an index-only path and it is not an A/B against the old
// single-column index. The fixture has 10 providers and 10 models, so each
// equality matches 10,000 rows; BenchmarkQueryBeforeCount isolates the same
// predicates without row decode.
func BenchmarkQueryBeforeEquality(b *testing.B) {
	s := benchmarkQueryBeforeStore(b)
	provider := &LogFilter{All: []LogPredicate{{Field: LogProvider, Value: "provider-03"}}}
	model := &LogFilter{All: []LogPredicate{{Field: LogModel, Value: "model-03"}}}
	cursor := &LogCursor{StartMs: queryBeforeFixtureStartMs + 50_000, ID: "row-050000"}
	for _, tc := range []struct {
		name   string
		before *LogCursor
		filter *LogFilter
	}{
		{"provider", nil, provider},
		{"provider_cursor", cursor, provider},
		{"model", nil, model},
		{"model_cursor", cursor, model},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				records, err := s.QueryBefore(b.Context(), tc.before, tc.filter, benchQueryBeforePage)
				if err != nil {
					b.Fatal(err)
				}
				if len(records) != benchQueryBeforePage {
					b.Fatalf("rows = %d, want %d", len(records), benchQueryBeforePage)
				}
			}
		})
	}
}

// BenchmarkQueryBeforeCount measures the same provider/model equality
// predicates as a COUNT(*), so the scan/index path is visible without the
// 200-row decode and heap allocation QueryBefore performs. The predicate
// clause comes from LogFilter.where, the same owner QueryBefore uses.
func BenchmarkQueryBeforeCount(b *testing.B) {
	s := benchmarkQueryBeforeStore(b)
	for _, tc := range []struct {
		name   string
		filter *LogFilter
		want   int
	}{
		{"provider", &LogFilter{All: []LogPredicate{{Field: LogProvider, Value: "provider-03"}}}, benchQueryBeforeRows / benchQueryBeforeProviders},
		{"model", &LogFilter{All: []LogPredicate{{Field: LogModel, Value: "model-03"}}}, benchQueryBeforeRows / benchQueryBeforeModels},
	} {
		where, args := tc.filter.where()
		query := `SELECT COUNT(*) FROM requests WHERE 1=1` + where
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var got int
				if err := s.rdb.QueryRowContext(b.Context(), query, args...).Scan(&got); err != nil {
					b.Fatal(err)
				}
				if got != tc.want {
					b.Fatalf("count = %d, want %d", got, tc.want)
				}
			}
		})
	}
}
