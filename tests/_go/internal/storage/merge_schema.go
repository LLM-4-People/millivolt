package storage

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func mergeTestLive(t *testing.T) *Store {
	t.Helper()
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	return live
}

func writeBackupDatabase(t *testing.T, name, ddl string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMergeRejectsMissingRequiredColumns pins the preflight: a backup whose
// requests table has no user_agent column must fail before touching rows,
// instead of reporting its row as a duplicate skip.
func TestMergeRejectsMissingRequiredColumns(t *testing.T) {
	live := mergeTestLive(t)
	path := writeBackupDatabase(t, "partial.db", `CREATE TABLE requests (
		id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL,
		key_hash TEXT NOT NULL, stream INTEGER NOT NULL, status_code INTEGER NOT NULL,
		started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL, ttft_ms INTEGER NOT NULL,
		first_token_at INTEGER NOT NULL, last_token_at INTEGER NOT NULL,
		finish_reason TEXT NOT NULL, error_type TEXT NOT NULL, error_msg TEXT NOT NULL,
		error_code TEXT NOT NULL, tool_calls INTEGER NOT NULL,
		input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, total_tokens INTEGER NOT NULL,
		cache_read_tokens INTEGER NOT NULL, cache_write_tokens INTEGER NOT NULL,
		reasoning_tokens INTEGER NOT NULL, client_disconnected INTEGER NOT NULL,
		rate_limit_remaining INTEGER NOT NULL, rate_limit_limit INTEGER NOT NULL, cost REAL NOT NULL,
		turns_user INTEGER NOT NULL, turns_assistant INTEGER NOT NULL, turns_tool INTEGER NOT NULL,
		response_headers TEXT NOT NULL);
		INSERT INTO requests VALUES
		('old-row','p','m','k',0,200,1,0,0,0,0,'','','','',0,0,0,0,0,0,0,0,0,0,0,0,0,0,'');`)
	inserted, skipped, err := live.MergeSnapshotFile(t.Context(), path)
	if err == nil {
		t.Fatalf("incompatible backup reported success (inserted=%d skipped=%d)", inserted, skipped)
	}
	if !strings.Contains(err.Error(), "missing required column") || !strings.Contains(err.Error(), "user_agent") {
		t.Fatalf("merge error %q does not name the missing required column", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	if n := countRows(t, live); n != 1 || live.Totals().Requests != 1 {
		t.Fatalf("failed merge changed live rows=%d totals=%+v", n, live.Totals())
	}
}

// TestMergeRejectsIDOnlySchema covers the most destructive shape: a backup
// with only an id column used to "merge" zero rows and report the row as a
// duplicate skip with no error.
func TestMergeRejectsIDOnlySchema(t *testing.T) {
	live := mergeTestLive(t)
	path := writeBackupDatabase(t, "id-only.db", `CREATE TABLE requests (id TEXT PRIMARY KEY); INSERT INTO requests(id) VALUES ('only-id');`)
	inserted, skipped, err := live.MergeSnapshotFile(t.Context(), path)
	if err == nil {
		t.Fatalf("id-only backup reported success (inserted=%d skipped=%d)", inserted, skipped)
	}
	if !strings.Contains(err.Error(), "missing required column") {
		t.Fatalf("merge error %q does not name the incompatible schema", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "live-row" {
		t.Fatalf("live rows after rejected merge = %#v", rows)
	}
}

// TestMergeRejectsExtraRequiredColumn pins the other half of the preflight:
// an incoming NOT NULL column with no live home would be silently dropped, so
// the backup is rejected before any row is touched.
func TestMergeRejectsExtraRequiredColumn(t *testing.T) {
	live := mergeTestLive(t)
	path := writeBackupDatabase(t, "extra.db", `CREATE TABLE requests (
		id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL,
		key_hash TEXT NOT NULL, user_agent TEXT NOT NULL, stream INTEGER NOT NULL,
		status_code INTEGER NOT NULL, started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL,
		ttft_ms INTEGER NOT NULL, first_token_at INTEGER NOT NULL, last_token_at INTEGER NOT NULL,
		finish_reason TEXT NOT NULL, error_type TEXT NOT NULL, error_msg TEXT NOT NULL,
		error_code TEXT NOT NULL, tool_calls INTEGER NOT NULL,
		input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, total_tokens INTEGER NOT NULL,
		cache_read_tokens INTEGER NOT NULL, cache_write_tokens INTEGER NOT NULL,
		reasoning_tokens INTEGER NOT NULL, client_disconnected INTEGER NOT NULL,
		rate_limit_remaining INTEGER NOT NULL, rate_limit_limit INTEGER NOT NULL, cost REAL NOT NULL,
		turns_user INTEGER NOT NULL, turns_assistant INTEGER NOT NULL, turns_tool INTEGER NOT NULL,
		response_headers TEXT NOT NULL, audit_extra TEXT NOT NULL);
		INSERT INTO requests VALUES
		('extra-row','p','m','k','ua',0,200,1,0,0,0,0,'','','','',0,0,0,0,0,0,0,0,0,0,0,0,0,0,'','must-not-be-dropped');`)
	inserted, skipped, err := live.MergeSnapshotFile(t.Context(), path)
	if err == nil {
		t.Fatalf("backup with an extra required column reported success (inserted=%d skipped=%d)", inserted, skipped)
	}
	if !strings.Contains(err.Error(), "unsupported required column") || !strings.Contains(err.Error(), "audit_extra") {
		t.Fatalf("merge error %q does not name the unsupported column", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("failed merge changed live rows = %d, want 1", n)
	}
}

// TestMergeCountsDuplicateIDsExplicitly pins the post-fix contract: duplicate
// ids are counted by the explicit live-id probe, while a valid new row merges
// exactly and the live row keeps precedence.
func TestMergeCountsDuplicateIDsExplicitly(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared-a", Provider: "live.example", Model: "n", StatusCode: 200})
	live.Record(&metrics.Record{ID: "shared-b", Provider: "live.example", Model: "n", StatusCode: 200})
	live.Record(&metrics.Record{ID: "live-only", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	now := time.Now()
	src.Record(&metrics.Record{ID: "shared-a", Provider: "backup.example", Model: "n", StatusCode: 200, Start: now})
	src.Record(&metrics.Record{ID: "shared-b", Provider: "backup.example", Model: "n", StatusCode: 200, Start: now.Add(time.Millisecond)})
	src.Record(&metrics.Record{ID: "backup-only", Provider: "backup.example", Model: "n", StatusCode: 200, Start: now.Add(2 * time.Millisecond)})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 || skipped != 2 {
		t.Fatalf("inserted=%d skipped=%d, want 1/2", inserted, skipped)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]*metrics.Record, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	if byID["shared-a"] == nil || byID["shared-a"].Provider != "live.example" ||
		byID["shared-b"] == nil || byID["shared-b"].Provider != "live.example" {
		t.Fatalf("live rows lost precedence: %#v", byID)
	}
	if byID["backup-only"] == nil || byID["live-only"] == nil {
		t.Fatalf("merge rows = %#v", byID)
	}
	if live.Totals().Requests != 4 {
		t.Fatalf("totals after merge = %d, want 4", live.Totals().Requests)
	}
}
