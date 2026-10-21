package storage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
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

func snapshotFromRecords(t *testing.T, records []*metrics.Record) []byte {
	t.Helper()
	live := openTestStore(t)
	for _, record := range records {
		if record.Start.IsZero() {
			record.Start = time.UnixMilli(1_800_200_000_000)
		}
		live.Record(record)
	}
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := live.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("snapshot records: %v", err)
	}
	return data
}

func aggregateRecords(t *testing.T, count int, apply func(*metrics.Record)) []*metrics.Record {
	t.Helper()
	records := make([]*metrics.Record, count)
	start := time.UnixMilli(1_800_200_000_000)
	for i := range records {
		records[i] = &metrics.Record{
			ID: fmt.Sprintf("aggregate-%02d", i), Provider: "neutral.example",
			Model: "n", StatusCode: 200, Start: start.Add(time.Duration(i) * time.Millisecond),
		}
		if apply != nil {
			apply(records[i])
		}
	}
	return records
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
		{"gen_tps", -0.5},
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

// TestRestoreRejectsAggregateTotalOverflow pins the STO-1 residual: every row
// can be representable while the full column total is not. Admission must
// reject the aggregate before replace installs or merge commits.
func TestRestoreRejectsAggregateTotalOverflow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*metrics.Record)
	}{
		{"cost", func(r *metrics.Record) { r.Cost = 1e307 }},
		{"gen_tps", func(r *metrics.Record) { r.DecodeTPS = 1e307 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := snapshotFromRecords(t, aggregateRecords(t, 20, tc.apply))
			requireRejected(t, data, "pending snapshot aggregate totals")
			requireMergeRejected(t, data, "totals after backup merge")
		})
	}
}

// TestRestoreRejectsIntegerTotalOverflowAndNegative pins both integer
// admission layers: a negative per-row count is rejected by the scan check, and
// an aggregate above int64 is rejected by the staged/merged Totals.Add scan.
func TestRestoreRejectsIntegerTotalOverflowAndNegative(t *testing.T) {
	for _, column := range aggregateCountColumns {
		t.Run(column, func(t *testing.T) {
			row := 0
			overflow := snapshotFromRecords(t, aggregateRecords(t, 2, func(r *metrics.Record) {
				value := int64(1)
				if row == 0 {
					value = math.MaxInt64
				}
				row++
				switch column {
				case "input_tokens":
					r.Usage.InputTokens = value
				case "output_tokens":
					r.Usage.OutputTokens = value
				case "cache_read_tokens":
					r.Usage.CacheReadTokens = value
				case "reasoning_tokens":
					r.Usage.ReasoningTokens = value
				case "ttft_ms":
					r.TTFTMs = value
				default:
					t.Fatalf("no fixture for %s", column)
				}
			}))
			// MaxInt64 plus one overflows the checked integer total.
			requireRejected(t, overflow, "pending snapshot aggregate totals")
			requireMergeRejected(t, overflow, "totals after backup merge")

			negative := snapshotFromRecords(t, aggregateRecords(t, 1, func(r *metrics.Record) {
				switch column {
				case "input_tokens":
					r.Usage.InputTokens = -1
				case "output_tokens":
					r.Usage.OutputTokens = -1
				case "cache_read_tokens":
					r.Usage.CacheReadTokens = -1
				case "reasoning_tokens":
					r.Usage.ReasoningTokens = -1
				case "ttft_ms":
					r.TTFTMs = -1
				}
			}))
			requireRejected(t, negative, "invalid "+column)
			requireMergeRejected(t, negative, "invalid "+column)
		})
	}
}

// TestRestoreAcceptsFiniteAggregateTotals is the control: 20 ordinary rows
// install, their totals stay representable, and the same set merges cleanly.
func TestRestoreAcceptsFiniteAggregateTotals(t *testing.T) {
	data := snapshotFromRecords(t, aggregateRecords(t, 20, func(r *metrics.Record) {
		r.Cost = 1.5
		r.DecodeTPS = 20
		r.TTFTMs = 100
		r.Usage.InputTokens = 10
		r.Usage.OutputTokens = 5
		r.Usage.CacheReadTokens = 2
		r.Usage.ReasoningTokens = 1
	}))
	live, err := replaceSnapshot(t, data)
	if err != nil {
		t.Fatalf("finite aggregate control rejected: %v", err)
	}
	if live.Totals().Err() != nil {
		t.Fatalf("finite aggregate control totals error: %v", live.Totals().Err())
	}
	if live.Totals().Requests != 20 {
		t.Fatalf("finite aggregate control requests = %d", live.Totals().Requests)
	}

	merge := mergeTestLive(t)
	inserted, skipped, err := merge.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("finite aggregate merge rejected: %v", err)
	}
	if inserted != 20 || skipped != 0 {
		t.Fatalf("finite aggregate merge inserted=%d skipped=%d, want 20/0", inserted, skipped)
	}
	if merge.Totals().Err() != nil {
		t.Fatalf("finite aggregate merge totals error: %v", merge.Totals().Err())
	}
}

// TestRestoreAcceptsEmptyAggregateTotals is the empty control: no rows means
// no aggregate work, and both admission paths stay successful.
func TestRestoreAcceptsEmptyAggregateTotals(t *testing.T) {
	data := snapshotFromRecords(t, nil)
	live, err := replaceSnapshot(t, data)
	if err != nil {
		t.Fatalf("empty replace rejected: %v", err)
	}
	if live.Totals().Err() != nil || live.Totals().Requests != 0 {
		t.Fatalf("empty replace totals = %+v err=%v", live.Totals(), live.Totals().Err())
	}

	merge := mergeTestLive(t)
	before := merge.Totals()
	inserted, skipped, err := merge.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("empty merge rejected: %v", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("empty merge inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	after := merge.Totals()
	if after.Err() != nil || after.Requests != before.Requests || after.Cost != before.Cost || after.InputTok != before.InputTok {
		t.Fatalf("empty merge changed totals: before=%+v after=%+v", before, after)
	}
}

// TestMergeDuplicateOnlyOverflowIsNoOp pins D2: duplicate source rows are not
// inserted, so an overflowing source-only aggregate must not reject a merge
// that leaves live history and totals unchanged.
func TestMergeDuplicateOnlyOverflowIsNoOp(t *testing.T) {
	source := snapshotFromRecords(t, aggregateRecords(t, 20, func(r *metrics.Record) {
		r.Cost = 1e307
	}))
	live := openTestStore(t)
	for i := 0; i < 20; i++ {
		live.Record(&metrics.Record{
			ID: fmt.Sprintf("aggregate-%02d", i), Provider: "neutral.example",
			Model: "n", StatusCode: 200, Start: time.UnixMilli(1_800_300_000_000 + int64(i)),
			Cost: 1.5,
		})
	}
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	before := live.Totals()
	inserted, skipped, err := live.MergeSnapshot(t.Context(), source)
	if err != nil {
		t.Fatalf("duplicate-only merge rejected: %v", err)
	}
	if inserted != 0 || skipped != 20 {
		t.Fatalf("duplicate-only merge inserted=%d skipped=%d, want 0/20", inserted, skipped)
	}
	after := live.Totals()
	if after.Err() != nil || after.Requests != before.Requests || after.Cost != before.Cost || after.InputTok != before.InputTok {
		t.Fatalf("duplicate-only merge changed totals: before=%+v after=%+v", before, after)
	}
}

// TestMergeRejectsCombinedAggregateOverflow pins D3: each side can have a
// representable total while the live-plus-incoming total is not. The merge must
// roll back and leave the live rows and totals untouched.
func TestMergeRejectsCombinedAggregateOverflow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*metrics.Record)
	}{
		{"cost", func(r *metrics.Record) { r.Cost = 1e306 }},
		{"input_tokens", func(r *metrics.Record) { r.Usage.InputTokens = 50_000_000_000_000_000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := openTestStore(t)
			for i := 0; i < 100; i++ {
				r := &metrics.Record{
					ID: fmt.Sprintf("live-%03d", i), Provider: "neutral.example",
					Model: "n", StatusCode: 200, Start: time.UnixMilli(1_800_400_000_000 + int64(i)),
				}
				tc.apply(r)
				live.Record(r)
			}
			if err := live.Flush(); err != nil {
				t.Fatal(err)
			}
			before := live.Totals()
			if before.Err() != nil {
				t.Fatalf("live control totals error: %v", before.Err())
			}
			sourceRecords := make([]*metrics.Record, 100)
			for i := range sourceRecords {
				r := &metrics.Record{
					ID: fmt.Sprintf("incoming-%03d", i), Provider: "neutral.example",
					Model: "n", StatusCode: 200, Start: time.UnixMilli(1_800_500_000_000 + int64(i)),
				}
				tc.apply(r)
				sourceRecords[i] = r
			}
			source := snapshotFromRecords(t, sourceRecords)
			inserted, skipped, err := live.MergeSnapshot(t.Context(), source)
			if err == nil || !strings.Contains(err.Error(), "totals after backup merge") {
				t.Fatalf("combined overflow merge = inserted=%d skipped=%d err=%v, want totals rejection", inserted, skipped, err)
			}
			rows, err := live.LoadRecent(t.Context(), 1000)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 100 {
				t.Fatalf("rejected merge changed live rows: %d", len(rows))
			}
			after := live.Totals()
			if after.Err() != nil || after.Requests != before.Requests || after.Cost != before.Cost || after.InputTok != before.InputTok {
				t.Fatalf("rejected merge changed live totals: before=%+v after=%+v", before, after)
			}
		})
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

// TestRestoreTimestampBoundariesAreUTC pins STO-2: every scanned time and the
// derived End stay in UTC, so marshalling at the representable boundaries does
// not depend on the process-local zone. It is deliberately race-safe: it does
// not mutate the global time.Local used by concurrent store writers.
func TestRestoreTimestampBoundariesAreUTC(t *testing.T) {
	fields := map[string]func(*metrics.Record) time.Time{
		"started_at":      func(r *metrics.Record) time.Time { return r.Start },
		"first_token_at":  func(r *metrics.Record) time.Time { return r.FirstTokenAt },
		"last_token_at":   func(r *metrics.Record) time.Time { return r.LastTokenAt },
		"first_answer_at": func(r *metrics.Record) time.Time { return r.FirstAnswerAt },
		"final_attempt_at": func(r *metrics.Record) time.Time {
			return r.FinalAttemptAt
		},
	}
	current := time.Now().UnixMilli()
	for _, column := range recordScanMillisColumns {
		field := fields[column]
		if field == nil {
			t.Fatalf("no field mapping for scanned time %s", column)
		}
		// Non-start timestamps treat zero and negative values as absent, so
		// exercise their live path with a current positive instant.
		data := snapshotWithValue(t, column, current)
		live, err := replaceSnapshot(t, data)
		if err != nil {
			t.Fatalf("%s rejected: %v", column, err)
		}
		rows, err := live.LoadRecent(t.Context(), 1)
		if err != nil {
			t.Fatalf("%s load: %v", column, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s rows = %d", column, len(rows))
		}
		if got := field(rows[0]); got.Location() != time.UTC || got.UnixMilli() != current {
			t.Fatalf("%s = %v (%v), want UTC at %d", column, got, got.Location(), current)
		}
		if column == "started_at" && (rows[0].End.Location() != time.UTC || rows[0].End.UnixMilli() != current) {
			t.Fatalf("derived End = %v (%v), want UTC at %d", rows[0].End, rows[0].End.Location(), current)
		}
		if _, err := json.Marshal(rows[0]); err != nil {
			t.Fatalf("%s does not marshal: %v", column, err)
		}
	}
	for _, value := range []int64{recordScanMillisMin, recordScanMillisMax} {
		data := snapshotWithValue(t, "started_at", value)
		live, err := replaceSnapshot(t, data)
		if err != nil {
			t.Fatalf("started_at %d rejected: %v", value, err)
		}
		rows, err := live.LoadRecent(t.Context(), 1)
		if err != nil {
			t.Fatalf("started_at %d load: %v", value, err)
		}
		if len(rows) != 1 {
			t.Fatalf("started_at %d rows = %d", value, len(rows))
		}
		if got := rows[0].Start; got.Location() != time.UTC || got.UnixMilli() != value {
			t.Fatalf("started_at = %v (%v), want UTC at %d", got, got.Location(), value)
		}
		// The fixture has duration_ms=0, so End is the same instant.
		if got := rows[0].End; got.Location() != time.UTC || got.UnixMilli() != value {
			t.Fatalf("derived End = %v (%v), want UTC at %d", got, got.Location(), value)
		}
		if _, err := json.Marshal(rows[0]); err != nil {
			t.Fatalf("started_at %d does not marshal: %v", value, err)
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
