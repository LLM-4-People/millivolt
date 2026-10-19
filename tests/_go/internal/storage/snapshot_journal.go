package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// hotJournalFromSnapshot builds a real SQLite hot rollback journal for the
// packed snapshot data. It opens a private copy in rollback-journal mode,
// starts a transaction that modifies a request row, and copies the journal
// while the transaction is still uncommitted. The copied journal therefore
// contains the original request page and is valid for rollback on a later
// open.
func hotJournalFromSnapshot(t *testing.T, data []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "journal-source.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)&_pragma=cache_size(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE requests SET provider = 'hot-journal-poison' WHERE id = 'stale-row'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Dirty enough pages to make SQLite spill the dirty request page into the
	// journal and write a synced journal header before the transaction ends.
	if _, err := db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('hot-journal-filler', ?)`, strings.Repeat("y", 65536)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	journal, err := os.ReadFile(path + "-journal")
	if err != nil {
		db.Close()
		t.Fatalf("read hot journal: %v", err)
	}
	if len(journal) == 0 || journal[0] == 0 {
		db.Close()
		t.Fatal("hot journal fixture is not a hot journal")
	}
	if _, err := db.Exec(`ROLLBACK`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return journal
}

// TestPendingSnapshotRejectsStaleHotJournal reproduces an interrupted
// preflight: a hot -journal from an older snapshot is left beside the pending
// slot, then a different snapshot is published there. Publication and
// preflight must discard the journal instead of replaying its pages into the
// new snapshot. Before the fix, the read-write preflight applied the stale
// journal and silently replaced the later snapshot's request row.
func TestPendingSnapshotRejectsStaleHotJournal(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "live.db")
	live, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	live.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	if err := StageSnapshot(dstPath, packedSnapshot(t, "later-row")); err != nil {
		t.Fatal(err)
	}
	staleJournal := hotJournalFromSnapshot(t, packedSnapshot(t, "stale-row"))
	if err := os.WriteFile(PendingSnapshotPath(dstPath)+"-journal", staleJournal, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ValidatePendingSnapshot(dstPath); err != nil {
		t.Fatalf("later snapshot rejected because of stale hot journal: %v", err)
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath) + "-journal"); !os.IsNotExist(err) {
		t.Fatalf("preflight left the stale hot journal beside the pending snapshot: %v", err)
	}

	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after stripping the stale hot journal: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "later-row" {
		t.Fatalf("live rows after hot-journal recovery = %#v, want only later-row", rows)
	}
}
