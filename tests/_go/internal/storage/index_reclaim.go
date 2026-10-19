package storage

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// reclaimSnapshot captures the durable read paths whose results must survive
// the index drop and VACUUM byte-for-byte: filtered candidate/full pages,
// purge-preview counts and filtered export payloads.
type reclaimSnapshot struct {
	candidates []LogCandidate
	records    []*metrics.Record
	counts     map[string]int64
	exports    map[string]string
}

func captureReclaimSnapshot(t *testing.T, s *Store) reclaimSnapshot {
	t.Helper()
	cursor := &LogCursor{StartMs: queryBeforeFixtureStartMs + 10_000, ID: "row-010000"}
	fields := []struct {
		field LogField
		value string
	}{
		{LogProvider, "provider-03"},
		{LogModel, "model-03"},
		{LogClient, "client-003"},
		{LogKeyHash, "key-000005"},
		{LogConversation, "conv-000005"},
		{LogStatus, "500"},
	}
	snapshot := reclaimSnapshot{counts: map[string]int64{}, exports: map[string]string{}}
	for _, tc := range fields {
		for _, before := range []*LogCursor{nil, cursor} {
			filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: tc.value}}}
			candidates, err := s.QueryBeforeCandidates(t.Context(), before, filter, 25)
			if err != nil {
				t.Fatal(err)
			}
			records, err := s.QueryBefore(t.Context(), before, filter, 25)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.candidates = append(snapshot.candidates, candidates...)
			snapshot.records = append(snapshot.records, records...)
		}
	}
	beforeMs := int64(queryBeforeFixtureStartMs + 15_000)
	for name, filter := range map[string]PurgeFilter{
		"provider":            {Provider: "provider-03"},
		"model":               {Model: "model-03"},
		"status":              {StatusCode: 500},
		"before":              {BeforeMs: beforeMs},
		"provider_and_status": {Provider: "provider-03", StatusCode: 500},
		"all":                 {BeforeMs: int64(queryBeforeFixtureStartMs + 1_000_000)},
	} {
		n, err := s.CountWhere(t.Context(), filter)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.counts[name] = n
		var out bytes.Buffer
		if _, err := s.ExportWhere(t.Context(), filter, &out); err != nil {
			t.Fatal(err)
		}
		snapshot.exports[name] = out.String()
	}
	return snapshot
}

func sqlitePragmaInt(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var value int
	if err := db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&value); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}
	return value
}

// TestMigrateReclaimsFreedIndexPagesOnce seeds a legacy-shaped database with
// all six superseded composites and history, reopens it through the production
// migration, and pins the one-time reclamation contract: the indexes are gone,
// every read path returns identical bytes, the freelist is empty and both page
// count and file size shrink. A second open must not vacuum again, which is
// proved by planting a non-empty freelist and observing that it survives.
func TestMigrateReclaimsFreedIndexPagesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reclaim.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	seedQueryBeforeRows(t, s, 20_000, 10, 10)
	want := captureReclaimSnapshot(t, s)
	if want.counts["all"] != 20_000 {
		t.Fatalf("fixture rows = %d, want 20000", want.counts["all"])
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	createLegacyDimensionRequestIndexes(t, raw)
	if _, err := raw.ExecContext(t.Context(), "ANALYZE requests"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	pagesBefore := sqlitePragmaInt(t, raw, "page_count")
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	assertRequestLogIndexOwnership(t, s.rdb)
	if got := captureReclaimSnapshot(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("query, purge-preview or export results changed across reclaim")
	}
	if got := sqlitePragmaInt(t, s.rdb, "freelist_count"); got != 0 {
		t.Fatalf("freelist pages after reclaim = %d, want 0", got)
	}
	pagesAfter := sqlitePragmaInt(t, s.rdb, "page_count")
	if pagesAfter*4 >= pagesBefore*3 {
		t.Fatalf("page count after reclaim = %d, before = %d; want a material shrink", pagesAfter, pagesBefore)
	}
	var journalMode string
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode after reclaim = %q, want wal", journalMode)
	}
	if got := sqlitePragmaInt(t, s.db, "busy_timeout"); got != int(sqliteBusyTimeout.Milliseconds()) {
		t.Fatalf("busy_timeout after reclaim = %d, want %d", got, sqliteBusyTimeout.Milliseconds())
	}
	if got := sqlitePragmaInt(t, s.db, "synchronous"); got != 1 {
		t.Fatalf("synchronous after reclaim = %d, want NORMAL(1)", got)
	}
	if got := sqlitePragmaInt(t, s.db, "journal_size_limit"); got != 67108864 {
		t.Fatalf("journal_size_limit after reclaim = %d, want 67108864", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if afterInfo.Size()*4 >= beforeInfo.Size()*3 {
		t.Fatalf("database size after reclaim = %d, before = %d; want a material shrink", afterInfo.Size(), beforeInfo.Size())
	}

	// Plant free pages without any superseded index. The second open must leave
	// them alone: reclamation is triggered by the index drop, not by a boot.
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(),
		"CREATE TABLE reclaim_pad AS SELECT * FROM requests; DROP TABLE reclaim_pad;"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	planted := sqlitePragmaInt(t, raw, "freelist_count")
	if planted == 0 {
		t.Fatal("fixture did not plant freelist pages")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s.Close()
	assertRequestLogIndexOwnership(t, s.rdb)
	if got := sqlitePragmaInt(t, s.rdb, "freelist_count"); got != planted {
		t.Fatalf("second open changed the freelist: got %d, planted %d (it vacuumed again)", got, planted)
	}
}
