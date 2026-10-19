package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
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
		".millivolt-backup-old.mvb",
		"millivolt-restore-upload-old.mvb",
		".millivolt-restore-pending-old",
		"." + base + "-incoming-old",
		"." + base + "-state-old",
		"millivolt-snapshot-old-journal",
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
	// The public download artifact uses millivolt-backup-<stamp>.mvb. It is a
	// retained operator file, not a private staging name, so the sweep must
	// never reclaim it even when it is older than the age bound.
	retainedArtifact := filepath.Join(dir, "millivolt-backup-20260101-000000.mvb")
	makeStagingEntry(t, retainedArtifact, false)

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
	for _, path := range []string{fresh, unrelated, retainedArtifact} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("sweep removed non-orphan %s: %v", path, err)
		}
	}
}

// TestStagingSweepPreservesLivePendingAndRollback pins the path-identity
// safety fence: even when the live database basename matches the snapshot
// staging pattern and the live set, pending admission slot, rollback set,
// state marker and quarantine names are older than the sweep age, they all
// survive. A recent in-progress staging entry also survives; only the old
// known orphan is reclaimed.
func TestStagingSweepPreservesLivePendingAndRollback(t *testing.T) {
	// The live basename itself matches snapshotStagingPattern. Path identity,
	// not the name shape, must keep the live set and every admission-derived
	// path safe.
	dir := t.TempDir()
	path := filepath.Join(dir, "millivolt-snapshot-live.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("fixture live WAL missing: %v", err)
	}
	if _, err := os.Stat(path + "-shm"); err != nil {
		t.Fatalf("fixture live SHM missing: %v", err)
	}
	safe := []string{
		path,
		path + "-wal",
		path + "-shm",
		path + "-journal",
		PendingSnapshotPath(path),
		PendingSnapshotPath(path) + "-wal",
		PendingSnapshotPath(path) + "-shm",
		PendingSnapshotPath(path) + "-journal",
		path + pendingSnapshotPreviousSuffix,
		path + pendingSnapshotPreviousSuffix + "-wal",
		path + pendingSnapshotPreviousSuffix + "-shm",
		path + pendingSnapshotPreviousSuffix + "-journal",
		path + pendingSnapshotStateSuffix,
		PendingSnapshotPath(path) + pendingSnapshotInvalidSuffix,
		PendingSnapshotPath(path) + pendingSnapshotInterruptedSuffix,
	}
	old := time.Now().Add(-2 * stagingSweepMinAge)
	for _, safePath := range safe {
		if _, err := os.Lstat(safePath); os.IsNotExist(err) {
			if err := os.WriteFile(safePath, []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
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

// TestOpenSweepPreservesLiveDatabaseNamedLikeStaging is the restart
// regression: a live db_path whose basename matches the snapshot staging
// pattern, plus a retained public backup artifact, both older than the sweep
// age, must survive a real Close/Open cycle. Only a true private staging
// orphan may be reclaimed.
func TestOpenSweepPreservesLiveDatabaseNamedLikeStaging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "millivolt-snapshot-live.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(&metrics.Record{ID: "survivor", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * stagingSweepMinAge)
	aged := []string{path}
	for _, suffix := range snapshotSidecarSuffixes {
		if _, err := os.Stat(path + suffix); err == nil {
			aged = append(aged, path+suffix)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	retainedArtifact := filepath.Join(dir, "millivolt-backup-20260101-000000.mvb")
	makeStagingEntry(t, retainedArtifact, false)
	orphan := filepath.Join(dir, ".millivolt-backup-orphan.mvb")
	makeStagingEntry(t, orphan, false)
	for _, agedPath := range aged {
		if err := os.Chtimes(agedPath, old, old); err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("restart with a staging-shaped live basename: %v", err)
	}
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("live history after restart: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "survivor" {
		t.Fatalf("live rows after restart = %#v, want survivor", rows)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("startup sweep deleted the live database: %v", err)
	}
	if _, err := os.Stat(retainedArtifact); err != nil {
		t.Fatalf("startup sweep deleted the retained public backup artifact: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("startup sweep left the true staging orphan %s: %v", orphan, err)
	}

	again, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("second restart after sweep: %v", err)
	}
	defer again.Close()
	rows, err = again.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("history after second restart: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "survivor" {
		t.Fatalf("live rows after second restart = %#v, want survivor", rows)
	}
}
