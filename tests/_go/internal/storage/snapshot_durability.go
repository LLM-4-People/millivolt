package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestCommittedAdmissionIgnoresStalePrevious pins the durable commit marker:
// a committed admission keeps its live rows and post-commit writes even when a
// stale rollback sibling reappears, as a lost unlink would leave it.
func TestCommittedAdmissionIgnoresStalePrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	old, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	old.Record(&metrics.Record{ID: "old-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := old.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	previousHook := snapshotDirSyncHook
	syncs := 0
	snapshotDirSyncHook = func(string) error {
		syncs++
		return nil
	}
	t.Cleanup(func() { snapshotDirSyncHook = previousHook })

	if err := StageSnapshot(path, packedSnapshot(t, "replacement")); err != nil {
		t.Fatal(err)
	}
	live, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	live.Record(&metrics.Record{ID: "after-commit", Provider: "neutral.example", Model: "n", StatusCode: 200, Start: time.Now()})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if syncs == 0 {
		t.Fatal("snapshot publish/install/commit never synced the database directory")
	}
	if state, err := readSnapshotState(path); err != nil || state != snapshotStateCommitted {
		t.Fatalf("state after commit = %q, %v; want committed", state, err)
	}

	// Model a power loss that did not persist the rollback unlink.
	if err := os.WriteFile(path+".previous", oldBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]bool, len(rows))
	for _, row := range rows {
		byID[row.ID] = true
	}
	if !byID["replacement"] || !byID["after-commit"] {
		t.Fatalf("committed rows after stale .previous = %#v, want replacement and after-commit", byID)
	}
	if byID["old-row"] {
		t.Fatal("recovery reverted a committed admission to the stale previous database")
	}
	if _, err := os.Stat(path + ".previous"); !os.IsNotExist(err) {
		t.Fatalf("stale rollback sibling survived recovery: %v", err)
	}
	if state, err := readSnapshotState(path); err != nil || state != snapshotStateCommitted {
		t.Fatalf("state after stale-previous recovery = %q, %v; want committed", state, err)
	}
}

// TestPendingAdmissionStateRollsBack pins the other side of the marker: an
// admission that installed but never committed leaves the pending marker, so
// the next boot restores the old database and discards the uncommitted file.
func TestPendingAdmissionStateRollsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	old, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	old.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := old.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshot(path, packedSnapshot(t, "uncommitted")); err != nil {
		t.Fatal(err)
	}
	admission, err := admitPendingSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if admission == nil {
		t.Fatal("compatible pending snapshot was not admitted")
	}
	if state, err := readSnapshotState(path); err != nil || state != snapshotStatePending {
		t.Fatalf("state after install = %q, %v; want pending", state, err)
	}
	// Simulate a crash: no commit and no rollback.

	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "keep-old" {
		t.Fatalf("rows after pending-state recovery = %#v, want keep-old", rows)
	}
	if _, err := os.Stat(PendingSnapshotPath(path)); !os.IsNotExist(err) {
		t.Fatal("recovery re-admitted the interrupted incoming file")
	}
	if state, err := readSnapshotState(path); err != nil || state != "" {
		t.Fatalf("state after pending recovery = %q, %v; want removed", state, err)
	}
}
