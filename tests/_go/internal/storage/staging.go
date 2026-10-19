package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeStagingEntry creates one staging-shaped file or directory and backdates
// it beyond the sweep age. The content is irrelevant; only the name and mtime
// participate in the sweep decision.
func makeStagingEntry(t *testing.T, path string, dir bool) {
	t.Helper()
	if dir {
		if err := os.MkdirAll(filepath.Join(path, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "nested", "snapshot.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * stagingSweepMinAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestOpenReclaimsOldOrphanedStaging pins the startup sweep's closed registry:
// every staging shape a crash can orphan (snapshot/overlap/merge directories,
// archive/upload files, decoded/inspected database directories, preserved
// pending copies and atomic-publish temps) is removed when old, while a recent
// in-progress entry and an unrelated operator file survive. The sweep runs
// during Open, before any handler can create new staging work.
func TestOpenReclaimsOldOrphanedStaging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orphans.db")
	base := filepath.Base(path)
	oldDirs := []string{
		"millivolt-snapshot-old",
		"millivolt-overlap-old",
		"millivolt-merge-old",
		"millivolt-restore-db-old",
		"millivolt-backup-db-old",
	}
	oldFiles := []string{
		"millivolt-backup-old.mvb",
		"millivolt-restore-upload-old.mvb",
		".millivolt-restore-pending-old",
		"." + base + "-incoming-old",
		"." + base + "-state-old",
	}
	for _, name := range oldDirs {
		makeStagingEntry(t, filepath.Join(dir, name), true)
	}
	for _, name := range oldFiles {
		makeStagingEntry(t, filepath.Join(dir, name), false)
	}
	fresh := filepath.Join(dir, "millivolt-snapshot-fresh")
	if err := os.Mkdir(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "operator-notes.txt")
	makeStagingEntry(t, unrelated, false)

	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range append(oldDirs, oldFiles...) {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("old staging entry %s survived startup sweep: %v", name, err)
		}
	}
	for _, path := range []string{fresh, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("sweep removed non-orphan %s: %v", path, err)
		}
	}
}

// TestStagingSweepPreservesLivePendingAndRollback pins the safety fence that
// does not depend on naming: even when the live database, pending admission
// slot, rollback set and state marker are older than the sweep age, they are
// not in the staging registry and survive. A recent in-progress staging entry
// also survives; only the old known orphan is reclaimed.
func TestStagingSweepPreservesLivePendingAndRollback(t *testing.T) {
	s := openTestStore(t)
	dir := s.stageDir()
	path := s.DBPath()
	safe := []string{
		path,
		PendingSnapshotPath(path),
		path + pendingSnapshotPreviousSuffix,
		path + pendingSnapshotPreviousSuffix + "-wal",
		path + pendingSnapshotStateSuffix,
	}
	old := time.Now().Add(-2 * stagingSweepMinAge)
	for _, safePath := range safe[1:] {
		if err := os.WriteFile(safePath, []byte("safe"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(safePath, old, old); err != nil {
			t.Fatal(err)
		}
	}
	orphan := filepath.Join(dir, "millivolt-snapshot-orphan")
	makeStagingEntry(t, orphan, true)
	fresh := filepath.Join(dir, "millivolt-merge-fresh")
	if err := os.Mkdir(fresh, 0o700); err != nil {
		t.Fatal(err)
	}

	if removed := s.sweepOrphanedStaging(time.Now()); removed != 1 {
		t.Fatalf("sweep removed %d entries, want 1", removed)
	}
	for _, safePath := range safe {
		if _, err := os.Stat(safePath); err != nil {
			t.Errorf("sweep removed protected file %s: %v", safePath, err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("sweep removed recent in-progress entry %s: %v", fresh, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("sweep left old orphan %s: %v", orphan, err)
	}
}
