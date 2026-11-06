package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotWithDebugID builds a valid packed snapshot and then inserts one
// request_debug row whose id is the supplied SQL expression. SQLite allows
// NULL in a TEXT PRIMARY KEY and stores any value class in a non-STRICT
// column, so these shapes are reachable through an imported snapshot.
func snapshotWithDebugID(t *testing.T, idExpr string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad-debug.db")
	if err := os.WriteFile(path, packedSnapshot(t, "row"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO request_debug(id, session_id, captured_at, expires_at, payload)
		VALUES (` + idExpr + `, 'sess', 1, 0, '{}')`); err != nil {
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

// TestRestoreRejectsUnfetchableDebugCaptureIDs pins the admission fence for
// sidecar ids LoadDebugCapture cannot address and the expiry sweep cannot
// reach. Such a row would still count against debug_store_max_bytes.
func TestRestoreRejectsUnfetchableDebugCaptureIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
	}{
		{"null", "NULL"},
		{"empty", "''"},
		{"blank", "'  '"},
		{"blob", "X'4142'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dst.db")
			if err := StageSnapshot(path, snapshotWithDebugID(t, tc.expr)); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePendingSnapshot(path); err == nil {
				t.Fatalf("%s capture id was admitted", tc.name)
			} else if !strings.Contains(err.Error(), "unfetchable capture id") {
				t.Fatalf("rejection = %v", err)
			}
		})
	}
}

// TestMergeRejectsUnfetchableDebugCaptureIDs applies the same fence to a
// restore merge, which imports sidecar rows into an already-live database.
func TestMergeRejectsUnfetchableDebugCaptureIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
	}{
		{"null", "NULL"},
		{"empty", "''"},
		{"blob", "X'4142'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := openPurgeTestStore(t, testOpts)
			path := filepath.Join(t.TempDir(), "incoming.db")
			if err := os.WriteFile(path, snapshotWithDebugID(t, tc.expr), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := live.MergeSnapshotFile(t.Context(), path); err == nil {
				t.Fatalf("%s capture id was merged", tc.name)
			} else if !strings.Contains(err.Error(), "unfetchable capture id") {
				t.Fatalf("merge rejection = %v", err)
			}
			if n := countRows(t, live); n != 0 {
				t.Fatalf("failed merge left %d request rows", n)
			}
		})
	}
}
