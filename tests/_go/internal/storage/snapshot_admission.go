package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// stageDatabaseBytes writes a SQLite file and stages it as the pending
// replacement for dstPath.
func stageDatabaseBytes(t *testing.T, dstPath, srcPath string) {
	t.Helper()
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshot(dstPath, data); err != nil {
		t.Fatal(err)
	}
}

// TestAdmissionRejectsUnwritableWriterSchema pins the insert probe: an extra
// NOT NULL column without a default passes schema/migrate but makes every
// writer INSERT fail. Admission must quarantine it before replacing the live
// database, and live ingestion must stay healthy afterwards.
func TestAdmissionRejectsUnwritableWriterSchema(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
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

	src := filepath.Join(dir, "src.db")
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN audit_extra TEXT NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stageDatabaseBytes(t, dstPath, src)

	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after rejected pending snapshot: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "keep-old" {
		t.Fatalf("live rows after rejection = %#v, want keep-old", rows)
	}
	incoming := PendingSnapshotPath(dstPath)
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Fatal("rejected pending snapshot was not consumed")
	}
	if _, err := os.Stat(incoming + pendingSnapshotInvalidSuffix); err != nil {
		t.Fatalf("rejected pending snapshot was not quarantined: %v", err)
	}

	// The live store still accepts writes on the same database.
	reopened.Record(&metrics.Record{ID: "after-reject", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := reopened.Flush(); err != nil {
		t.Fatalf("live ingestion after rejection: %v", err)
	}
	if n := countRows(t, reopened); n != 2 {
		t.Fatalf("live rows after new write = %d, want 2", n)
	}
}

// TestAdmissionRejectsIDWithoutUniqueKey pins the conflict-target half of the
// probe: id TEXT without a primary key or unique index makes ON CONFLICT(id)
// unrepresentable, which schema/migrate alone never notices.
func TestAdmissionRejectsIDWithoutUniqueKey(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
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

	src := filepath.Join(dir, "src.db")
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	badSchema := strings.Replace(schema, "id TEXT PRIMARY KEY", "id TEXT", 1)
	if _, err := db.Exec(badSchema); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stageDatabaseBytes(t, dstPath, src)

	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after rejected pending snapshot: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "keep-old" {
		t.Fatalf("live rows after rejection = %#v, want keep-old", rows)
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath) + pendingSnapshotInvalidSuffix); err != nil {
		t.Fatalf("rejected pending snapshot was not quarantined: %v", err)
	}
	reopened.Record(&metrics.Record{ID: "after-reject", Provider: "neutral.example", Model: "n", StatusCode: 200, Start: time.Now()})
	if err := reopened.Flush(); err != nil {
		t.Fatalf("live ingestion after rejection: %v", err)
	}
}
