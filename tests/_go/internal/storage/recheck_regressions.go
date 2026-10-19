package storage

import (
	"bytes"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func snapshotWithValue(t *testing.T, column string, value any) []byte {
	t.Helper()
	return legacySnapshot(t, schema, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET "`+column+`" = ? WHERE id = 'legacy-row'`, value)
		return err
	})
}

func snapshotWithStartAndDuration(t *testing.T, start, duration int64) []byte {
	t.Helper()
	return legacySnapshot(t, schema, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET started_at = ?, duration_ms = ? WHERE id = 'legacy-row'`, start, duration)
		return err
	})
}

func replaceSnapshot(t *testing.T, data []byte) (*Store, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dst.db")
	live, err := Open(path, testOpts)
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
	if err := StageSnapshot(path, data); err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	if err := ValidatePendingSnapshot(path); err != nil {
		return nil, err
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open accepted replacement: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened, nil
}

func requireRejected(t *testing.T, data []byte, want string) {
	t.Helper()
	live, err := replaceSnapshot(t, data)
	if err == nil {
		if live != nil {
			live.Close()
		}
		t.Fatalf("replacement admitted; want rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejection = %q, want %q", err, want)
	}
}

func requireMergeRejected(t *testing.T, data []byte, want string) {
	t.Helper()
	live := mergeTestLive(t)
	before, err := live.CountWhere(t.Context(), PurgeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err == nil {
		t.Fatalf("merge admitted (inserted=%d skipped=%d); want rejection mentioning %q", inserted, skipped, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("merge rejection = %q, want %q", err, want)
	}
	after, err := live.CountWhere(t.Context(), PurgeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rejected merge changed live rows %d -> %d", before, after)
	}
}

// TestRestoreRejectsAggregateUnsafeValues pins STO-1: every value outside the
// metrics.SumValues domain is rejected before replace installs or merge commits.
func TestRestoreRejectsAggregateUnsafeValues(t *testing.T) {
	for _, tc := range []struct {
		column string
		value  any
	}{
		{"cost", math.Inf(1)},
		{"cost", math.Inf(-1)},
		{"cost", -1.0},
		{"gen_tps", math.Inf(1)},
		{"overall_tps", -0.5},
	} {
		t.Run(tc.column, func(t *testing.T) {
			data := snapshotWithValue(t, tc.column, tc.value)
			requireRejected(t, data, "invalid "+tc.column)
			requireMergeRejected(t, data, "invalid "+tc.column)
		})
	}
	// SQLite stores a NaN REAL as NULL. The canonical schema forbids it, but
	// a legacy nullable column can carry it; the NULL value stays rejected.
	nullable := strings.Replace(schema, "cost REAL NOT NULL", "cost REAL", 1)
	nan := legacySnapshot(t, nullable, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET cost = NULL WHERE id = 'legacy-row'`)
		return err
	})
	requireRejected(t, nan, "NULL cost")

	// Control: the writer's own finite value and timestamp admit and encode.
	control := snapshotWithValue(t, "cost", 1.5)
	live, err := replaceSnapshot(t, control)
	if err != nil {
		t.Fatalf("valid replace rejected: %v", err)
	}
	if live.Totals().Err() != nil {
		t.Fatalf("valid replace totals error: %v", live.Totals().Err())
	}
}

// TestRestoreRejectsUnrepresentableTimestamps pins STO-2: milliseconds outside
// the time.Time JSON range are rejected on both admission paths.
func TestRestoreRejectsUnrepresentableTimestamps(t *testing.T) {
	for _, column := range recordScanMillisColumns {
		for _, value := range []int64{math.MaxInt64, math.MinInt64} {
			data := snapshotWithValue(t, column, value)
			requireRejected(t, data, "invalid "+column)
			requireMergeRejected(t, data, "invalid "+column)
		}
	}
	for _, value := range []int64{recordScanMillisMin, recordScanMillisMax, time.Now().UnixMilli()} {
		data := snapshotWithValue(t, "started_at", value)
		live, err := replaceSnapshot(t, data)
		if err != nil {
			t.Fatalf("valid started_at %d rejected: %v", value, err)
		}
		if live.Totals().Err() != nil {
			t.Fatalf("valid started_at %d totals error: %v", value, live.Totals().Err())
		}
	}
}

// TestRestoreRejectsUnsafeDurations pins the STO-2 derived-End extension:
// duration_ms must be nonnegative, fit time.Duration nanoseconds, and keep
// started_at+duration_ms inside the RFC 3339 millisecond window.
func TestRestoreRejectsUnsafeDurations(t *testing.T) {
	start := time.Now().UnixMilli()
	for _, tc := range []struct {
		name     string
		start    int64
		duration int64
	}{
		{"max int64", start, math.MaxInt64},
		{"duration multiplication overflow", start, recordScanDurationMaxMs + 1},
		{"negative", start, -1},
		{"end beyond rfc3339", recordScanMillisMax - 500, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := snapshotWithStartAndDuration(t, tc.start, tc.duration)
			requireRejected(t, data, "invalid duration_ms")
			requireMergeRejected(t, data, "invalid duration_ms")
		})
	}

	control := snapshotWithStartAndDuration(t, start-1000, 1500)
	live, err := replaceSnapshot(t, control)
	if err != nil {
		t.Fatalf("normal duration rejected: %v", err)
	}
	rows, err := live.LoadRecent(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("control rows = %d", len(rows))
	}
	if got := rows[0].End.Sub(rows[0].Start); got != 1500*time.Millisecond {
		t.Fatalf("derived End-Start = %v, want 1.5s", got)
	}
}

// TestRestoreAttemptsJSONInvariant pins STO-4: replace shares merge's
// json_valid(attempts) admission check, while empty and null stay accepted and
// empty is canonicalized so HasError/count/export/purge keep working.
func TestRestoreAttemptsJSONInvariant(t *testing.T) {
	requireRejected(t, snapshotWithValue(t, "attempts", "{bad"), "malformed JSON")
	requireMergeRejected(t, snapshotWithValue(t, "attempts", "{bad"), "malformed JSON")

	for _, stored := range []string{"", "null"} {
		live, err := replaceSnapshot(t, snapshotWithValue(t, "attempts", stored))
		if err != nil {
			t.Fatalf("attempts %q rejected: %v", stored, err)
		}
		var got string
		if err := live.rdb.QueryRow(`SELECT attempts FROM requests WHERE id = 'legacy-row'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != "null" {
			t.Fatalf("restored attempts %q canonicalized as %q", stored, got)
		}
		if _, err := live.CountWhere(t.Context(), PurgeFilter{HasError: true}); err != nil {
			t.Fatalf("HasError count after attempts %q: %v", stored, err)
		}
		var buf bytes.Buffer
		if _, err := live.ExportWhere(t.Context(), PurgeFilter{HasError: true}, &buf); err != nil {
			t.Fatalf("HasError export after attempts %q: %v", stored, err)
		}
		if _, err := live.Clear(t.Context(), &PurgeFilter{HasError: true}, metrics.NewBuffer(1)); err != nil {
			t.Fatalf("HasError purge after attempts %q: %v", stored, err)
		}
	}
}

// TestRestoreRejectsNonTextExactKeys pins STO-5: SQLite permits BLOB values in
// TEXT columns, but the log cursor binds ids as TEXT and cannot materialize
// them. Every scan-critical text column requires the TEXT storage class.
func TestRestoreRejectsNonTextExactKeys(t *testing.T) {
	for _, column := range []string{"id", "provider", "model", "key_hash", "conversation_id"} {
		data := snapshotWithValue(t, column, []byte("AB"))
		requireRejected(t, data, "invalid "+column)
		requireMergeRejected(t, data, "invalid "+column)
	}

	data := snapshotWithValue(t, "id", "")
	live, err := replaceSnapshot(t, data)
	if err != nil {
		t.Fatalf("empty TEXT id rejected: %v", err)
	}
	cands, err := live.QueryBeforeCandidates(t.Context(), nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	cursors := make([]LogCursor, len(cands))
	for i := range cands {
		cursors[i] = cands[i].Cursor
	}
	out, err := live.LogPageRecords(t.Context(), cursors)
	if err != nil {
		t.Fatalf("empty TEXT id log page: %v", err)
	}
	if len(out) != len(cursors) {
		t.Fatalf("materialized %d of %d accepted rows", len(out), len(cursors))
	}
}

// TestRestoreRejectsUnownedRequestTriggers pins STO-3: a replacement carrying a
// write-path trigger outside the four owned analytical triggers is rejected
// before install, and a normal snapshot still admits and writes.
func TestRestoreRejectsUnownedRequestTriggers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dst.db")
	live, err := Open(path, testOpts)
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
	if err := os.WriteFile(src, packedSnapshot(t, "backup-row"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER poison AFTER INSERT ON requests
		WHEN NEW.id <> 'millivolt-preflight'
		BEGIN DELETE FROM requests WHERE id = NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshotFile(path, src); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePendingSnapshot(path); err == nil {
		t.Fatal("poison trigger replacement was admitted")
	} else if !strings.Contains(err.Error(), "unowned trigger") {
		t.Fatalf("trigger rejection = %v", err)
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open after rejection: %v", err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "keep-old" {
		t.Fatalf("rejected replacement changed live history: %+v", rows)
	}

	control, err := replaceSnapshot(t, packedSnapshot(t, "backup-row"))
	if err != nil {
		t.Fatalf("normal snapshot rejected: %v", err)
	}
	control.Record(&metrics.Record{ID: "after", Provider: "neutral.example", Model: "n", StatusCode: 200, Start: time.Now()})
	if err := control.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err = control.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || control.Totals().Requests != 2 {
		t.Fatalf("control rows=%d totals=%d, want 2/2", len(rows), control.Totals().Requests)
	}
}

// TestOpenRepairsLegacyEmptyToolNames pins the MCP-5 storage owner: a database
// migrated by the old ADD COLUMN default keeps tool_names=” on old rows, and
// the one-shot repair canonicalizes it to the writer's no-value document so
// every later json_each reads valid JSON.
func TestOpenRepairsLegacyEmptyToolNames(t *testing.T) {
	legacy := strings.Replace(schema, "tool_names TEXT NOT NULL DEFAULT 'null'", "tool_names TEXT NOT NULL DEFAULT ''", 1)
	if legacy == schema {
		t.Fatal("fixture did not find the canonical tool_names default")
	}
	data := legacySnapshot(t, legacy, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE requests SET tool_names = '' WHERE id = 'legacy-row'`)
		return err
	})
	path := filepath.Join(t.TempDir(), "legacy-tool-names.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open legacy tool_names database: %v", err)
	}
	defer live.Close()
	var got string
	if err := live.rdb.QueryRow(`SELECT tool_names FROM requests WHERE id = 'legacy-row'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "null" {
		t.Fatalf("legacy tool_names = %q, want 'null'", got)
	}
	if marker, err := live.LoadMeta(t.Context(), toolNamesRepairMetaKey); err != nil || marker == "" {
		t.Fatalf("tool_names repair marker = %q, %v; want written after the repairing boot", marker, err)
	}
	var names int
	if err := live.rdb.QueryRow(`SELECT COUNT(*) FROM requests, json_each(tool_names) AS j WHERE j.value IS NOT NULL`).Scan(&names); err != nil {
		t.Fatalf("json_each over repaired tool_names: %v", err)
	}
	if names != 0 {
		t.Fatalf("repaired no-value tool_names enumerated %d names", names)
	}
}

// TestWriterProbeReadsItsSentinelBack pins the STO-3 probe half directly: a
// trigger that deletes the sentinel after insert cannot pass admission just
// because SQLite reported one affected row.
func TestWriterProbeReadsItsSentinelBack(t *testing.T) {
	live := openTestStore(t)
	path := live.path
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER probe_eater AFTER INSERT ON requests
		WHEN NEW.id LIKE 'millivolt-preflight%'
		BEGIN DELETE FROM requests WHERE id = NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	if err := probeWriterInsert(db); err == nil {
		t.Fatal("writer probe accepted a row its trigger deleted")
	} else if !strings.Contains(err.Error(), "probe row") {
		t.Fatalf("probe error = %v", err)
	}
}
