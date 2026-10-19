package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// legacySnapshot copies one canonical snapshot row into a database built from
// a caller-supplied legacy schema, applies mutate, and returns the packed
// bytes. Rebuilding through a copied schema is how an old millivolt database
// looked; the production migration never retrofits constraints on an existing
// table.
func legacySnapshot(t *testing.T, legacySchema string, mutate func(*sql.DB) error) []byte {
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
	if mutate != nil {
		if err := mutate(db); err != nil {
			db.Close()
			t.Fatal(err)
		}
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

// legacyIDSnapshot builds a packed snapshot whose requests table keeps the
// pre-NOT-NULL id definition (SQLite allows NULL in a TEXT PRIMARY KEY) and
// whose only row carries the requested id value.
func legacyIDSnapshot(t *testing.T, id any) []byte {
	t.Helper()
	return legacySnapshot(t, strings.Replace(schema, "id TEXT NOT NULL PRIMARY KEY", "id TEXT PRIMARY KEY", 1), func(db *sql.DB) error {
		if id == nil {
			_, err := db.Exec(`UPDATE requests SET id = NULL WHERE id = 'legacy-row'`)
			return err
		}
		_, err := db.Exec(`UPDATE requests SET id = ? WHERE id = 'legacy-row'`, id)
		return err
	})
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

// nullableColumnSnapshot builds a packed snapshot whose requests table keeps
// the requested column nullable (a pre-NOT-NULL legacy definition) and whose
// only row stores value in that column.
func nullableColumnSnapshot(t *testing.T, notNullDDL, nullableDDL, column string, value any) []byte {
	t.Helper()
	legacySchema := strings.Replace(schema, notNullDDL, nullableDDL, 1)
	return legacySnapshot(t, legacySchema, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET "`+column+`" = ? WHERE id = 'legacy-row'`, value)
		return err
	})
}

func nullableScanColumnCases() []struct {
	name        string
	column      string
	notNullDDL  string
	nullableDDL string
} {
	return []struct {
		name        string
		column      string
		notNullDDL  string
		nullableDDL string
	}{
		{
			name:        "tool_names",
			column:      "tool_names",
			notNullDDL:  "tool_names TEXT NOT NULL DEFAULT ''",
			nullableDDL: "tool_names TEXT",
		},
		{
			name:        "attempts",
			column:      "attempts",
			notNullDDL:  "attempts TEXT NOT NULL DEFAULT 'null'",
			nullableDDL: "attempts TEXT",
		},
	}
}

// TestMergeRejectsNullScanRequiredColumns pins merge fail-closed behavior for
// the scan-critical columns beyond id: a crafted or legacy backup with NULL
// tool_names or attempts passes integrity inspection but must be rejected
// before the merge transaction inserts a row that makes history reads fail.
func TestMergeRejectsNullScanRequiredColumns(t *testing.T) {
	for _, tc := range nullableScanColumnCases() {
		t.Run(tc.name, func(t *testing.T) {
			live := mergeTestLive(t)
			data := nullableColumnSnapshot(t, tc.notNullDDL, tc.nullableDDL, tc.column, nil)
			inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
			if err == nil || !strings.Contains(err.Error(), "NULL "+tc.column) {
				t.Fatalf("merge with NULL %s = %v, want a NULL-column rejection", tc.column, err)
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
		})
	}
}

// TestOpenQuarantinesNullScanRequiredColumnSnapshot pins the replace path for
// the same columns: validation must reject the pending file, and the next boot
// must quarantine it without installing a history-breaking database.
func TestOpenQuarantinesNullScanRequiredColumnSnapshot(t *testing.T) {
	for _, tc := range nullableScanColumnCases() {
		t.Run(tc.name, func(t *testing.T) {
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

			data := nullableColumnSnapshot(t, tc.notNullDDL, tc.nullableDDL, tc.column, nil)
			if err := StageSnapshot(dstPath, data); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePendingSnapshot(dstPath); err == nil || !strings.Contains(err.Error(), "NULL "+tc.column) {
				t.Fatalf("ValidatePendingSnapshot = %v, want a NULL %s rejection", err, tc.column)
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
		})
	}
}

// TestMergeAcceptsEmptyToolNames pins the other side of the boundary: an empty
// string is valid for every text scan column, including tool_names and
// attempts, so it must still merge and remain readable.
func TestMergeAcceptsEmptyToolNames(t *testing.T) {
	live := mergeTestLive(t)
	data := nullableColumnSnapshot(t,
		"tool_names TEXT NOT NULL DEFAULT ''", "tool_names TEXT", "tool_names", "")
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("empty tool_names rejected: %v", err)
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
		if row.ID == "legacy-row" && len(row.ToolNames) == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("merged rows = %#v, want the empty-tool-names row", rows)
	}
}

// snapshotWithoutRequestColumn builds a packed canonical snapshot whose
// requests table omits one column, the shape of a backup written before that
// column existed. VACUUM INTO produces a clean standalone file after the
// ALTER instead of relying on a checkpointed WAL sibling.
func snapshotWithoutRequestColumn(t *testing.T, column string) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "without.db")
	if err := os.WriteFile(path, packedSnapshot(t, "backup-row"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE requests DROP COLUMN "` + column + `"`); err != nil {
		db.Close()
		t.Fatalf("drop %s from backup fixture: %v", column, err)
	}
	packedPath := filepath.Join(dir, "packed.db")
	if _, err := db.Exec(`VACUUM INTO ?`, packedPath); err != nil {
		db.Close()
		t.Fatalf("pack reduced backup fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(packedPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestMergeRejectsOmittedScanColumnOnNullableLive pins the absent-column half
// of the scan preflight. A backup created before tool_names existed omits the
// column; against a legacy live table whose tool_names is nullable, the merge
// INSERT would leave the live default (NULL) and poison every history scan.
// The merge must fail closed before inserting anything.
func TestMergeRejectsOmittedScanColumnOnNullableLive(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "live.db")
	if err := os.WriteFile(livePath, nullableColumnSnapshot(t,
		"tool_names TEXT NOT NULL DEFAULT ''", "tool_names TEXT", "tool_names", ""), 0o600); err != nil {
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
	nullableToolNames := false
	for _, col := range cols {
		if col.name == "tool_names" {
			nullableToolNames = !col.notNull
		}
	}
	if !nullableToolNames {
		t.Fatal("live fixture requests.tool_names was rebuilt as NOT NULL")
	}

	inserted, skipped, err := live.MergeSnapshot(t.Context(), snapshotWithoutRequestColumn(t, "tool_names"))
	if err == nil {
		t.Fatalf("backup omitting tool_names reported success (inserted=%d skipped=%d)", inserted, skipped)
	}
	if !strings.Contains(err.Error(), "tool_names") {
		t.Fatalf("merge error %q does not name the omitted scan column", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("live history unreadable after rejected merge: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "legacy-row" {
		t.Fatalf("live rows after rejected merge = %#v, want legacy-row", rows)
	}
	var nulls int64
	if err := live.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE tool_names IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 0 {
		t.Fatalf("rejected merge still inserted %d rows with NULL tool_names", nulls)
	}
}

// TestMergeDefaultsOmittedScanColumnOnRequiredLive is the admitted half of the
// same shape: a normal live schema declares tool_names NOT NULL with an
// empty-string default, so omitting the column inserts a valid empty value and
// the merged row stays readable. Failing that shape would reject ordinary
// pre-column backups.
func TestMergeDefaultsOmittedScanColumnOnRequiredLive(t *testing.T) {
	live := mergeTestLive(t)
	inserted, skipped, err := live.MergeSnapshot(t.Context(), snapshotWithoutRequestColumn(t, "tool_names"))
	if err != nil {
		t.Fatalf("omitted tool_names rejected on a defaulted live column: %v", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("live history unreadable after defaulted merge: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == "backup-row" && len(row.ToolNames) == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("merged rows = %#v, want the defaulted-tool-names row", rows)
	}
}

// TestPreflightRejectsNullInEveryRecordScanColumn proves the preflight is
// derived from recordColumns rather than a small hand-picked set: every
// column scanRecord scans into a non-null Go value is rejected when NULL.
func TestPreflightRejectsNullInEveryRecordScanColumn(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.db")
	data := legacySnapshot(t, strings.ReplaceAll(schema, " NOT NULL", ""), nil)
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(dir, "canonical.db")
	if err := os.WriteFile(canonical, packedSnapshot(t, "legacy-row"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ATTACH DATABASE ? AS pristine`, canonical); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DETACH DATABASE pristine`)

	// Parse recordColumns independently of the production parser so weakening
	// recordScanColumns cannot make this coverage test pass vacuously.
	columns := recordColumnNames()
	if len(columns) == 0 {
		t.Fatal("recordColumnNames returned no columns")
	}
	if got := recordScanColumns(); len(got) != len(columns) {
		t.Fatalf("recordScanColumns has %d columns, recordColumns has %d", len(got), len(columns))
	}
	for _, column := range columns {
		if _, err := db.Exec(`UPDATE requests SET "` + column + `" = NULL`); err != nil {
			t.Fatalf("set %s NULL: %v", column, err)
		}
		err := preflightRequestScanValues(t.Context(), db, "", "fixture")
		if err == nil || !strings.Contains(err.Error(), "NULL "+column) {
			t.Fatalf("preflight with NULL %s = %v, want a NULL %s rejection", column, err, column)
		}
		if _, err := db.Exec(`DELETE FROM requests`); err != nil {
			t.Fatalf("reset %s row: %v", column, err)
		}
		if _, err := db.Exec(`INSERT INTO requests SELECT * FROM pristine.requests`); err != nil {
			t.Fatalf("restore %s row: %v", column, err)
		}
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

// scanColumnValueSnapshot builds a packed canonical snapshot whose only row
// stores value in column. The canonical schema is kept, so the fixture is the
// shape a crafted backup or a legacy writer could produce: SQLite stores the
// value with its own storage class even when the declared column affinity
// differs.
func scanColumnValueSnapshot(t *testing.T, column string, value any) []byte {
	t.Helper()
	return legacySnapshot(t, schema, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET "`+column+`" = ? WHERE id = 'legacy-row'`, value)
		return err
	})
}

func unscannableScanColumnCases() []struct {
	name   string
	column string
	value  any
} {
	return []struct {
		name   string
		column string
		value  any
	}{
		{"stream_text", "stream", "abc"},
		{"stream_out_of_bool_range", "stream", 2},
		{"stream_blob", "stream", []byte("1")},
		{"status_code_text", "status_code", "abc"},
		{"status_code_real", "status_code", 2.5},
		{"cost_text", "cost", "not-a-number"},
		{"cost_blob", "cost", []byte("1.5")},
	}
}

// TestMergeRejectsUnscannableScanColumnValues pins the value preflight for a
// merge: a row whose storage class or range scanRecord cannot scan must fail
// before the transaction inserts anything. NULL alone was never enough -
// stream='abc' or stream=2 passes integrity, schema and writer probes, then
// makes every history read fail after the poison row is committed.
func TestMergeRejectsUnscannableScanColumnValues(t *testing.T) {
	for _, tc := range unscannableScanColumnCases() {
		t.Run(tc.name, func(t *testing.T) {
			live := mergeTestLive(t)
			inserted, skipped, err := live.MergeSnapshot(t.Context(), scanColumnValueSnapshot(t, tc.column, tc.value))
			if err == nil {
				t.Fatalf("merge with %s=%v reported success (inserted=%d skipped=%d)", tc.column, tc.value, inserted, skipped)
			}
			if !strings.Contains(err.Error(), tc.column) {
				t.Fatalf("merge error %q does not name %s", err, tc.column)
			}
			if inserted != 0 || skipped != 0 {
				t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
			}
			var count int64
			if err := live.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("rejected merge left %d request rows, want only the live row", count)
			}
			rows, err := live.LoadRecent(t.Context(), 10)
			if err != nil {
				t.Fatalf("live history unreadable after rejected merge: %v", err)
			}
			if len(rows) != 1 || rows[0].ID != "live-row" {
				t.Fatalf("live rows after rejected merge = %#v, want live-row", rows)
			}
		})
	}
}

// TestOpenQuarantinesUnscannableScanColumnSnapshot pins the replace path: the
// pending snapshot is rejected without installing it, and the live database
// keeps serving readable history.
func TestOpenQuarantinesUnscannableScanColumnSnapshot(t *testing.T) {
	for _, tc := range unscannableScanColumnCases() {
		t.Run(tc.name, func(t *testing.T) {
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

			if err := StageSnapshot(dstPath, scanColumnValueSnapshot(t, tc.column, tc.value)); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePendingSnapshot(dstPath); err == nil || !strings.Contains(err.Error(), tc.column) {
				t.Fatalf("ValidatePendingSnapshot = %v, want a %s rejection", err, tc.column)
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
		})
	}
}

// TestMergeAcceptsValidScanColumnValues pins the accepted side of the same
// boundary: the writer's own representations - 0/1 booleans, ordinary
// integers and reals, and empty text/JSON columns - must still merge and read
// back.
func TestMergeAcceptsValidScanColumnValues(t *testing.T) {
	live := mergeTestLive(t)
	data := legacySnapshot(t, schema, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET stream = 1, status_code = 201, cost = 1.5,
			tool_names = '', attempts = '', debug = 1 WHERE id = 'legacy-row'`)
		return err
	})
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("valid scan values rejected: %v", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var found *metrics.Record
	for _, row := range rows {
		if row.ID == "legacy-row" {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("merged rows = %#v, want legacy-row", rows)
	}
	if !found.Stream || found.StatusCode != 201 || found.Cost != 1.5 || !found.Debug {
		t.Fatalf("merged row = stream=%v status=%d cost=%v debug=%v, want true/201/1.5/true",
			found.Stream, found.StatusCode, found.Cost, found.Debug)
	}
	if len(found.ToolNames) != 0 || len(found.Attempts) != 0 {
		t.Fatalf("merged empty JSON columns = tool_names=%v attempts=%v, want empty", found.ToolNames, found.Attempts)
	}
}

// TestRecordScanTargetsCoverRecordColumns pins the derivation seam: every
// recordColumns name has exactly one scan destination and every destination
// is a type the preflight knows how to validate. A new column or destination
// type fails here instead of silently escaping the value preflight.
func TestRecordScanTargetsCoverRecordColumns(t *testing.T) {
	var record metrics.Record
	var scratch recordScanScratch
	targets := recordScanTargets(&record, &scratch)
	names := recordScanColumns()
	if len(targets) != len(names) {
		t.Fatalf("recordScanTargets has %d destinations, recordScanColumns has %d", len(targets), len(names))
	}
	for i, target := range targets {
		if _, _, err := recordScanValueKindOf(target); err != nil {
			t.Fatalf("record column %s: %v", names[i], err)
		}
	}
}
