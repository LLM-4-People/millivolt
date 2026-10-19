package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// legacyIDSnapshot builds a packed snapshot whose requests table keeps the
// pre-NOT-NULL id definition (SQLite allows NULL in a TEXT PRIMARY KEY) and
// whose only row carries the requested id value. Rebuilding via a copied
// schema is how an old millivolt database looked; the production migration
// never retrofits constraints on an existing table.
func legacyIDSnapshot(t *testing.T, id any) []byte {
	t.Helper()
	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "legacy-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	valid, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(t.TempDir(), "valid.db")
	if err := os.WriteFile(validPath, valid, 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := strings.Replace(schema, "id TEXT NOT NULL PRIMARY KEY", "id TEXT PRIMARY KEY", 1)
	if _, err := db.Exec(legacySchema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`ATTACH DATABASE ? AS legacy_source`, validPath); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO requests SELECT * FROM legacy_source.requests`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`DETACH DATABASE legacy_source`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if id == nil {
		_, err = db.Exec(`UPDATE requests SET id = NULL WHERE id = 'legacy-row'`)
	} else {
		_, err = db.Exec(`UPDATE requests SET id = ? WHERE id = 'legacy-row'`, id)
	}
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestMergeRejectsNullRequestID pins the merge preflight: SQLite permits NULL
// in a legacy TEXT PRIMARY KEY, but scanRecord requires a string id. A merge
// must fail closed before any row is inserted, leaving the live history
// readable instead of admitting a poison row.
func TestMergeRejectsNullRequestID(t *testing.T) {
	// A database from before the NOT NULL DDL keeps its nullable id: migrate
	// adds columns but never rebuilds the table. That is the shape where a
	// NULL-id merge row used to be admitted and poison every history scan.
	livePath := filepath.Join(t.TempDir(), "live.db")
	if err := os.WriteFile(livePath, legacyIDSnapshot(t, "live-row"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(livePath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	cols, err := tableColumnDetails(t.Context(), live.db, "", "requests")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range cols {
		if col.name == "id" && col.notNull {
			t.Fatal("legacy live requests.id was rebuilt as NOT NULL, want the migrated table kept")
		}
	}
	data := legacyIDSnapshot(t, nil)
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err == nil {
		t.Fatalf("backup with a NULL request id reported success (inserted=%d skipped=%d)", inserted, skipped)
	}
	if !strings.Contains(err.Error(), "NULL id") {
		t.Fatalf("merge error %q does not name the NULL id row", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("live history unreadable after rejected merge: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "live-row" {
		t.Fatalf("live rows after rejected merge = %#v, want live-row", rows)
	}
}

// TestMergeAcceptsEmptyRequestID pins the other side of the boundary: an
// empty string is a valid non-NULL id, so it still merges and scans.
func TestMergeAcceptsEmptyRequestID(t *testing.T) {
	live := mergeTestLive(t)
	data := legacyIDSnapshot(t, "")
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("empty request id rejected: %v", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("merged rows = %#v, want the empty-id row", rows)
	}
}

// TestOpenQuarantinesNullRequestIDSnapshot pins the replace preflight: an
// archive that passes integrity and the writer INSERT probe but carries a
// NULL request id must be rejected before installation, preserving the live
// database and its readable history.
func TestOpenQuarantinesNullRequestIDSnapshot(t *testing.T) {
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

	data := legacyIDSnapshot(t, nil)
	if err := StageSnapshot(dstPath, data); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePendingSnapshot(dstPath); err == nil || !strings.Contains(err.Error(), "NULL id") {
		t.Fatalf("ValidatePendingSnapshot = %v, want a NULL id rejection", err)
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath)); err != nil {
		t.Fatalf("validation consumed the rejected pending snapshot: %v", err)
	}

	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after rejected pending snapshot: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("live history unreadable after rejected replacement: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "keep-old" {
		t.Fatalf("live rows after rejection = %#v, want keep-old", rows)
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath) + pendingSnapshotInvalidSuffix); err != nil {
		t.Fatalf("rejected pending snapshot was not quarantined: %v", err)
	}
}

// TestFreshRequestSchemaRequiresID pins the fresh DDL. Existing tables keep
// their old definition (migrate never rebuilds them); only new databases get
// the NOT NULL id, and the row preflight above covers the legacy shape.
func TestFreshRequestSchemaRequiresID(t *testing.T) {
	live := mergeTestLive(t)
	cols, err := tableColumnDetails(t.Context(), live.db, "", "requests")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range cols {
		if col.name == "id" {
			if !col.notNull {
				t.Fatal("fresh requests.id is nullable, want NOT NULL PRIMARY KEY")
			}
			return
		}
	}
	t.Fatal("fresh requests table has no id column")
}
