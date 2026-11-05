package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ttftAnchorSnapshot packs a source database holding both TTFT shapes a merge
// can receive: "legacy-anchor" carries the pre-0.3.34 start-anchored signature
// (ttft_ms > 0 with final_attempt_at = 0), while "stamped-anchor" keeps its
// adopted attempt and per-attempt value.
func ttftAnchorSnapshot(t *testing.T) []byte {
	t.Helper()
	src := openTestStore(t)
	start := time.UnixMilli(1_700_000_300_000)
	rows := []*metrics.Record{
		{ID: "legacy-anchor", Provider: "backup.example", Model: "m", StatusCode: 200, Stream: true,
			Start: start, End: start.Add(time.Second), FinalAttemptAt: start,
			FirstTokenAt: start.Add(100 * time.Millisecond), LastTokenAt: start.Add(time.Second)},
		{ID: "stamped-anchor", Provider: "backup.example", Model: "m", StatusCode: 200, Stream: true,
			Start: start, End: start.Add(time.Second), FinalAttemptAt: start,
			FirstTokenAt: start.Add(200 * time.Millisecond), LastTokenAt: start.Add(time.Second)},
	}
	for _, r := range rows {
		metrics.FinalizeRecord(r)
		src.Record(r)
	}
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	// The source database predates the repair: reintroduce the signature a
	// live store must never accept again after its own boot repair.
	if _, err := src.db.Exec(`UPDATE requests SET ttft_ms = 900, final_attempt_at = 0 WHERE id = 'legacy-anchor'`); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestMergeZeroesLegacyTTFTAnchors pins the merge-side closure of the boot
// repair's one-shot marker: meta is never merged, so a backup whose rows were
// written before 0.3.34 (or that predates the marker) would otherwise
// reintroduce the unrecoverable start-anchored ttft_ms into a live database
// that already finished its repair. The merge zeroes exactly that signature
// and leaves a stamped adopted attempt byte-for-byte.
func TestMergeZeroesLegacyTTFTAnchors(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "m", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err := live.MergeSnapshot(t.Context(), ttftAnchorSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 2 || skipped != 0 {
		t.Fatalf("inserted=%d skipped=%d, want 2/0", inserted, skipped)
	}
	var legacyTTFT, legacyFinal, stampedTTFT, stampedFinal int64
	if err := live.rdb.QueryRow(`SELECT ttft_ms, final_attempt_at FROM requests WHERE id = 'legacy-anchor'`).Scan(&legacyTTFT, &legacyFinal); err != nil {
		t.Fatal(err)
	}
	if legacyTTFT != 0 || legacyFinal != 0 {
		t.Fatalf("merged legacy anchor = ttft_ms %d, final_attempt_at %d; want 0/0", legacyTTFT, legacyFinal)
	}
	if err := live.rdb.QueryRow(`SELECT ttft_ms, final_attempt_at FROM requests WHERE id = 'stamped-anchor'`).Scan(&stampedTTFT, &stampedFinal); err != nil {
		t.Fatal(err)
	}
	if stampedTTFT != 200 || stampedFinal == 0 {
		t.Fatalf("merged stamped anchor = ttft_ms %d, final_attempt_at %d; want the 200ms value and the stamped attempt", stampedTTFT, stampedFinal)
	}
}

// TestMergeZeroesTTFTWhenAttemptColumnAbsent pins the other half of the merge
// predicate: a backup written before final_attempt_at existed has no column to
// read, so every positive ttft_ms is the legacy shape once the live default 0
// applies. The mapping must treat the missing column as 0 instead of
// referencing it and failing the whole merge.
func TestMergeZeroesTTFTWhenAttemptColumnAbsent(t *testing.T) {
	src := openTestStore(t)
	start := time.UnixMilli(1_700_000_400_000)
	r := &metrics.Record{ID: "columnless-anchor", Provider: "backup.example", Model: "m", StatusCode: 200, Stream: true,
		Start: start, End: start.Add(time.Second), FinalAttemptAt: start,
		FirstTokenAt: start.Add(150 * time.Millisecond), LastTokenAt: start.Add(time.Second)}
	metrics.FinalizeRecord(r)
	src.Record(r)
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	packed := filepath.Join(dir, "packed.db")
	if err := os.WriteFile(packed, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", packed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE requests DROP COLUMN final_attempt_at`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	columnless := filepath.Join(dir, "columnless.db")
	if _, err := db.Exec("VACUUM INTO ?", columnless); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	live := openTestStore(t)
	inserted, skipped, err := live.MergeSnapshotFile(t.Context(), columnless)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	var ttft, final int64
	if err := live.rdb.QueryRow(`SELECT ttft_ms, final_attempt_at FROM requests WHERE id = 'columnless-anchor'`).Scan(&ttft, &final); err != nil {
		t.Fatal(err)
	}
	if ttft != 0 || final != 0 {
		t.Fatalf("merged columnless anchor = ttft_ms %d, final_attempt_at %d; want 0/0", ttft, final)
	}
}
