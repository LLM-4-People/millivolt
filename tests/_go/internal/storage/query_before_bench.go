package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const (
	queryBeforeFixtureStartMs  = 1_700_000_000_000
	queryBeforeSeedChunk       = 1_000
	benchQueryBeforeRows       = 100_000
	benchQueryBeforePage       = 200
	benchQueryBeforeProviders  = 10
	benchQueryBeforeModels     = 10
	benchQueryBeforeClients    = 500
	queryBeforeRareStatus      = 500
	queryBeforeCommonStatus    = 200
	queryBeforeRareStatusEvery = 1_000
)

// queryBeforeFixtureRow is the single owner of the seeded fixture shape: start
// times ascend one millisecond apart, provider and model cycle independently,
// client values repeat every benchQueryBeforeClients rows, key and conversation
// ids are unique, and every queryBeforeRareStatusEvery-th row carries the rare
// status. Plan and page tests derive their expected matches from this function,
// so a fixture change cannot silently invalidate them.
func queryBeforeFixtureRow(i, providers, models int) *metrics.Record {
	status := queryBeforeCommonStatus
	if i%queryBeforeRareStatusEvery == 0 {
		status = queryBeforeRareStatus
	}
	return &metrics.Record{
		ID:             fmt.Sprintf("row-%06d", i),
		Provider:       fmt.Sprintf("provider-%02d", i%providers),
		Model:          fmt.Sprintf("model-%02d", (i/providers)%models),
		Client:         fmt.Sprintf("client-%03d", i%benchQueryBeforeClients),
		KeyHash:        fmt.Sprintf("key-%06d", i),
		ConversationID: fmt.Sprintf("conv-%06d", i),
		StatusCode:     status,
		Start:          time.UnixMilli(queryBeforeFixtureStartMs).Add(time.Duration(i) * time.Millisecond),
	}
}

// seedQueryBeforeRows fills a store with n rows built by
// queryBeforeFixtureRow, so a provider or model equality matches n/providers or
// n/models rows while the remaining equality columns keep the distribution the
// plan and page benchmarks need.
func seedQueryBeforeRows(t testing.TB, s *Store, n, providers, models int) {
	t.Helper()
	batch := make([]*metrics.Record, 0, queryBeforeSeedChunk)
	for i := 0; i < n; i++ {
		batch = append(batch, queryBeforeFixtureRow(i, providers, models))
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
// the exact 200-row (or fewer, for a rare equality) page the dashboard serves,
// including scanRecord decode, for every exact equality field LogFilter pushes
// down, with and without the exclusive cursor. It is not an index-only path and
// it is not an A/B against the old single-column index. The fixture has 10
// providers and 10 models (10,000 matches each), 500 clients (200 matches),
// unique key hashes and conversation ids, and a rare status every 1,000 rows;
// BenchmarkQueryBeforeCount isolates the same predicates without row decode.
func BenchmarkQueryBeforeEquality(b *testing.B) {
	s := benchmarkQueryBeforeStore(b)
	cursor := &LogCursor{StartMs: queryBeforeFixtureStartMs + 50_000, ID: "row-050000"}
	for _, tc := range []struct {
		name   string
		before *LogCursor
		field  LogField
		value  string
		want   int
	}{
		{"provider", nil, LogProvider, "provider-03", benchQueryBeforePage},
		{"provider_cursor", cursor, LogProvider, "provider-03", benchQueryBeforePage},
		{"model", nil, LogModel, "model-03", benchQueryBeforePage},
		{"model_cursor", cursor, LogModel, "model-03", benchQueryBeforePage},
		{"client", nil, LogClient, "client-003", benchQueryBeforeRows / benchQueryBeforeClients},
		{"client_cursor", cursor, LogClient, "client-003", benchQueryBeforeRows / benchQueryBeforeClients / 2},
		{"key_hash", nil, LogKeyHash, "key-000005", 1},
		{"key_hash_cursor", cursor, LogKeyHash, "key-000005", 1},
		{"conversation", nil, LogConversation, "conv-000005", 1},
		{"conversation_cursor", cursor, LogConversation, "conv-000005", 1},
		{"status", nil, LogStatus, "500", benchQueryBeforeRows / queryBeforeRareStatusEvery},
		{"status_cursor", cursor, LogStatus, "500", benchQueryBeforeRows / queryBeforeRareStatusEvery / 2},
	} {
		b.Run(tc.name, func(b *testing.B) {
			filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: tc.value}}}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				records, err := s.QueryBefore(b.Context(), tc.before, filter, benchQueryBeforePage)
				if err != nil {
					b.Fatal(err)
				}
				if len(records) != tc.want {
					b.Fatalf("rows = %d, want %d", len(records), tc.want)
				}
			}
		})
	}
}

// BenchmarkQueryBeforeCount measures every pushed-down equality predicate as a
// COUNT(*), so the scan/index path is visible without the 200-row decode and
// heap allocation QueryBefore performs. The predicate clause comes from
// LogFilter.where, the same owner QueryBefore uses.
func BenchmarkQueryBeforeCount(b *testing.B) {
	s := benchmarkQueryBeforeStore(b)
	for _, tc := range []struct {
		name  string
		field LogField
		value string
		want  int
	}{
		{"provider", LogProvider, "provider-03", benchQueryBeforeRows / benchQueryBeforeProviders},
		{"model", LogModel, "model-03", benchQueryBeforeRows / benchQueryBeforeModels},
		{"client", LogClient, "client-003", benchQueryBeforeRows / benchQueryBeforeClients},
		{"key_hash", LogKeyHash, "key-000005", 1},
		{"conversation", LogConversation, "conv-000005", 1},
		{"status", LogStatus, "500", benchQueryBeforeRows / queryBeforeRareStatusEvery},
	} {
		filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: tc.value}}}
		where, args, err := filter.where()
		if err != nil {
			b.Fatal(err)
		}
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
