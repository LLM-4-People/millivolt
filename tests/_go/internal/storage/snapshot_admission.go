package storage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
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

// TestOpenRejectsUnwritableLiveWriterSchema pins the boot-time counterpart of
// admission's writer insert probe: an existing live database with an extra
// NOT NULL column passes schema/migrate but would make every committed batch
// fail. Open must fail closed at boot instead of starting a store that
// silently drops all durable writes.
func TestOpenRejectsUnwritableLiveWriterSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	live, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN audit_extra TEXT NOT NULL`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, testOpts)
	if err == nil {
		reopened.Close()
		t.Fatal("Open accepted a live schema whose writer INSERT cannot execute")
	}
	if !strings.Contains(err.Error(), "writer insert probe") {
		t.Fatalf("Open error %q does not name the writer insert probe", err)
	}
}

// TestValidatePendingSnapshotRejectsMissingPendingFile pins fail-closed
// validation: an absent pending file is not an empty snapshot to initialize.
// Before this guard, preflight's sql.Open created a fresh empty database at the
// pending path, ValidatePendingSnapshot returned nil, and the next boot would
// have admitted that empty file over live history.
func TestValidatePendingSnapshotRejectsMissingPendingFile(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	err := ValidatePendingSnapshot(dstPath)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ValidatePendingSnapshot missing pending = %v, want a not-exist rejection", err)
	}
	if _, statErr := os.Stat(PendingSnapshotPath(dstPath)); !os.IsNotExist(statErr) {
		t.Fatalf("validation created the missing pending file: %v", statErr)
	}
}

// TestValidatePendingSnapshotRunsAdmissionPreflight pins the restore-time
// validation owner: it validates the staged pending file with the same
// schema/migrate and writer-insert preflight boot runs, rejects a file that
// passes integrity but cannot accept the writer INSERT, and never installs or
// removes the pending file itself.
func TestValidatePendingSnapshotRunsAdmissionPreflight(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	if err := StageSnapshot(dstPath, packedSnapshot(t, "valid-row")); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePendingSnapshot(dstPath); err != nil {
		t.Fatalf("valid pending snapshot rejected: %v", err)
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath)); err != nil {
		t.Fatalf("validation consumed the pending snapshot: %v", err)
	}
	if _, err := os.Stat(dstPath); !os.IsNotExist(err) {
		t.Fatalf("validation installed the pending snapshot: %v", err)
	}

	// Build the same extra-required-column shape admission rejects: integrity
	// passes, but the writer's exact INSERT omits the new NOT NULL column.
	mutable := filepath.Join(dir, "mutable.db")
	data, err := os.ReadFile(PendingSnapshotPath(dstPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mutable, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", mutable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM requests`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN audit_extra TEXT NOT NULL`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backup.CheckDatabaseFile(mutable); err != nil {
		t.Fatalf("invalid fixture must still pass the integrity check: %v", err)
	}
	bad, err := os.ReadFile(mutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshot(dstPath, bad); err != nil {
		t.Fatal(err)
	}
	err = ValidatePendingSnapshot(dstPath)
	if err == nil || !strings.Contains(err.Error(), "writer insert probe") {
		t.Fatalf("ValidatePendingSnapshot = %v, want a writer insert probe rejection", err)
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath)); err != nil {
		t.Fatalf("validation removed the rejected pending snapshot: %v", err)
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
	badSchema := strings.Replace(schema, "id TEXT NOT NULL PRIMARY KEY", "id TEXT", 1)
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
