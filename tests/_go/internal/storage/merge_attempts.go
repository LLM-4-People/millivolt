package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func attemptsBackup(t *testing.T, id, attempts string) []byte {
	t.Helper()
	src := openTestStore(t)
	src.Record(&metrics.Record{ID: id, Provider: "backup.example", Model: "m", StatusCode: 200, Start: time.Now()})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.Exec(`UPDATE requests SET attempts = ? WHERE id = ?`, attempts, id); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}

// TestMergeCanonicalizesEmptyAttempts pins the merge-side JSON invariant: the
// legacy empty spelling becomes the canonical 'null', so HasError and every
// other json_each query keep working after the merge and after a reopen. The
// live database's one-shot migrate marker must not be the only protection.
func TestMergeCanonicalizesEmptyAttempts(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "m", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err := live.MergeSnapshot(t.Context(), attemptsBackup(t, "empty-attempts", ""))
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	var stored string
	if err := live.rdb.QueryRow(`SELECT attempts FROM requests WHERE id = 'empty-attempts'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "null" {
		t.Fatalf("merged attempts = %q, want canonical null", stored)
	}
	if n, err := live.CountWhere(t.Context(), PurgeFilter{HasError: true}); err != nil || n != 0 {
		t.Fatalf("HasError after canonicalizing merge = %d, %v", n, err)
	}

	path := live.path
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if n, err := reopened.CountWhere(t.Context(), PurgeFilter{HasError: true}); err != nil || n != 0 {
		t.Fatalf("HasError after reopen = %d, %v", n, err)
	}
	if err := reopened.rdb.QueryRow(`SELECT attempts FROM requests WHERE id = 'empty-attempts'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "null" {
		t.Fatalf("reopened attempts = %q, want canonical null", stored)
	}
}

// TestMergePreservesCanonicalAttemptsBytes pins byte identity: a valid
// non-empty JSON document is copied exactly, not re-encoded through a Go type.
func TestMergePreservesCanonicalAttemptsBytes(t *testing.T) {
	const canonical = `[ {"status_code": 503, "error_type": "upstream"} ]`
	live := openTestStore(t)
	if _, _, err := live.MergeSnapshot(t.Context(), attemptsBackup(t, "valid-attempts", canonical)); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := live.rdb.QueryRow(`SELECT attempts FROM requests WHERE id = 'valid-attempts'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != canonical {
		t.Fatalf("merged attempts = %q, want the exact source bytes %q", stored, canonical)
	}
	if n, err := live.CountWhere(t.Context(), PurgeFilter{HasError: true}); err != nil || n != 1 {
		t.Fatalf("HasError after valid merge = %d, %v; want 1", n, err)
	}
}

// TestMergeRejectsMalformedAttempts pins fail-closed validation: a non-empty
// invalid JSON value aborts the whole merge before a row can poison error
// queries, and the live database stays usable.
func TestMergeRejectsMalformedAttempts(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "m", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	beforeTotals := live.Totals()
	inserted, skipped, err := live.MergeSnapshot(t.Context(), attemptsBackup(t, "malformed-attempts", "{"))
	if err == nil {
		t.Fatalf("malformed attempts merged successfully (inserted=%d skipped=%d)", inserted, skipped)
	}
	if !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("merge error %q does not name the malformed JSON value", err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	if n, err := live.CountWhere(t.Context(), PurgeFilter{}); err != nil || n != 1 {
		t.Fatalf("rows after rejected merge = %d, %v; want only the live row", n, err)
	}
	if got := live.Totals(); got != beforeTotals {
		t.Fatalf("totals after rejected merge = %+v, want %+v", got, beforeTotals)
	}
	if _, err := live.CountWhere(t.Context(), PurgeFilter{HasError: true}); err != nil {
		t.Fatalf("HasError after rejected merge: %v", err)
	}
}
