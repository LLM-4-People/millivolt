package storage

import (
	"fmt"
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

// makeOldLivePath creates a missing protected fixture file and backdates it
// beyond the sweep age, so only the path-identity fence can preserve it.
func makeOldLivePath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte("safe"), 0o600); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
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

// TestOpenSweepPreservesSymlinkedLiveTarget is the restart regression for a
// symlinked db_path: the configured name is proxy.db, while the resolved live
// target is millivolt-snapshot-live.db and therefore matches the snapshot
// staging pattern. Path protection must cover the resolved target (and the
// sidecars/admission files derived from it), not only the symlink spelling.
func TestOpenSweepPreservesSymlinkedLiveTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "millivolt-snapshot-live.db")
	s, err := Open(target, testOpts)
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

	link := filepath.Join(dir, "proxy.db")
	if err := os.Symlink(filepath.Base(target), link); err != nil {
		t.Fatal(err)
	}
	protected := []string{
		target,
		target + "-wal",
		target + "-shm",
		PendingSnapshotPath(target),
		PendingSnapshotPath(target) + "-wal",
		PendingSnapshotPath(target) + "-shm",
		PendingSnapshotPath(target) + "-journal",
		target + pendingSnapshotPreviousSuffix,
		target + pendingSnapshotStateSuffix,
		PendingSnapshotPath(target) + pendingSnapshotInvalidSuffix,
		PendingSnapshotPath(target) + pendingSnapshotInterruptedSuffix,
	}
	for _, protectedPath := range protected {
		makeOldLivePath(t, protectedPath)
	}
	orphan := filepath.Join(dir, "millivolt-snapshot-orphan")
	makeStagingEntry(t, orphan, true)

	reopened, err := Open(link, testOpts)
	if err != nil {
		t.Fatalf("restart through symlinked db_path: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("startup sweep deleted the resolved live target: %v", err)
	}
	for _, protectedPath := range protected {
		if _, err := os.Stat(protectedPath); err != nil {
			t.Errorf("sweep removed resolved live path %s: %v", protectedPath, err)
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("sweep left the true staging orphan %s: %v", orphan, err)
	}
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("live history after symlinked restart: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "survivor" {
		t.Fatalf("live rows after symlinked restart = %#v, want survivor", rows)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(link, testOpts)
	if err != nil {
		t.Fatalf("second restart through symlinked db_path: %v", err)
	}
	defer again.Close()
	rows, err = again.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("history after second symlinked restart: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "survivor" {
		t.Fatalf("live rows after second symlinked restart = %#v, want survivor", rows)
	}
}

// TestStagingSweepProtectsResolvedSymlinkAdmissionPaths pins every path shape
// derived from the resolved target, including the SQLite sidecars, pending
// slot, rollback set and durable state marker. The sweep reads names from the
// configured directory, so only resolving the symlink (here to an absolute
// target) can recognize them.
func TestStagingSweepProtectsResolvedSymlinkAdmissionPaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "millivolt-snapshot-live.db")
	if err := os.WriteFile(target, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "proxy.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	protected := []string{
		target,
		target + "-wal",
		target + "-shm",
		target + "-journal",
		PendingSnapshotPath(target),
		PendingSnapshotPath(target) + "-wal",
		PendingSnapshotPath(target) + "-shm",
		PendingSnapshotPath(target) + "-journal",
		target + pendingSnapshotPreviousSuffix,
		target + pendingSnapshotPreviousSuffix + "-wal",
		target + pendingSnapshotPreviousSuffix + "-shm",
		target + pendingSnapshotPreviousSuffix + "-journal",
		target + pendingSnapshotStateSuffix,
		PendingSnapshotPath(target) + pendingSnapshotInvalidSuffix,
		PendingSnapshotPath(target) + pendingSnapshotInterruptedSuffix,
	}
	for _, protectedPath := range protected {
		makeOldLivePath(t, protectedPath)
	}
	orphan := filepath.Join(dir, "millivolt-snapshot-orphan")
	makeStagingEntry(t, orphan, true)

	s := &Store{path: link}
	if removed := s.sweepOrphanedStaging(time.Now()); removed != 1 {
		t.Fatalf("sweep removed %d entries, want 1", removed)
	}
	for _, protectedPath := range protected {
		if _, err := os.Stat(protectedPath); err != nil {
			t.Errorf("sweep removed resolved live path %s: %v", protectedPath, err)
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("sweep left the true staging orphan %s: %v", orphan, err)
	}
}

// TestStagingSweepReclaimsOrphansPastFirstBatch pins the batched directory
// walk. With more old orphans than one read batch, a single ReadDir batch
// leaves the tail unexamined on every boot; the sweep must keep reading in
// bounded batches until the directory is exhausted (or the per-boot budget is
// reached), while the protected staging-shaped live path still survives.
func TestStagingSweepReclaimsOrphansPastFirstBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "millivolt-snapshot-live.db")
	makeOldLivePath(t, path)
	// The filesystem returns directory order, not lexical order, so the
	// regression needs more removable orphans than one batch rather than a
	// single entry that could land in the first batch by chance.
	const orphans = 300
	names := make([]string, orphans)
	for i := range names {
		names[i] = filepath.Join(dir, fmt.Sprintf("millivolt-snapshot-orphan-%03d", i))
		makeStagingEntry(t, names[i], true)
	}

	s := &Store{path: path}
	if removed := s.sweepOrphanedStaging(time.Now()); removed != orphans {
		t.Fatalf("sweep removed %d entries, want all %d orphans", removed, orphans)
	}
	for _, name := range names {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("sweep left an orphan past the first read batch: %s: %v", name, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sweep removed the protected live path: %v", err)
	}
}

// TestStagingSweepPreservesQuarantinedPendingSidecars pins the quarantine
// naming and the sweep fence together. A rejected or interrupted pending
// snapshot moves its sidecars beside the quarantined main, and those files
// must use the protected sidecar spelling rather than a suffix order the
// sweep's path fence does not know. The live basename is staging-shaped so a
// name-only sweep would reclaim the old sidecars if the fence missed them.
func TestStagingSweepPreservesQuarantinedPendingSidecars(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix string
	}{
		{"rejected", pendingSnapshotInvalidSuffix},
		{"interrupted", pendingSnapshotInterruptedSuffix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "millivolt-snapshot-live.db")
			s := &Store{path: path}
			incoming := PendingSnapshotPath(path)
			makeStagingEntry(t, incoming, false)
			for _, sidecar := range snapshotSidecarSuffixes {
				makeStagingEntry(t, incoming+sidecar, false)
			}
			if err := quarantinePendingSnapshot(incoming, tc.suffix); err != nil {
				t.Fatal(err)
			}

			quarantined := incoming + tc.suffix
			actual := make(map[string]string, len(snapshotSidecarSuffixes))
			for _, sidecar := range snapshotSidecarSuffixes {
				normalized := quarantined + sidecar
				legacy := incoming + sidecar + tc.suffix
				actualPath := normalized
				if _, err := os.Stat(normalized); err != nil {
					if _, legacyErr := os.Stat(legacy); legacyErr != nil {
						t.Fatalf("quarantined sidecar %s missing in both spellings", sidecar)
					}
					actualPath = legacy
				}
				actual[sidecar] = actualPath
			}

			if removed := s.sweepOrphanedStaging(time.Now()); removed != 0 {
				t.Fatalf("sweep removed %d quarantined entries, want 0", removed)
			}
			if _, err := os.Stat(quarantined); err != nil {
				t.Fatalf("sweep removed the quarantined main file: %v", err)
			}
			for _, sidecar := range snapshotSidecarSuffixes {
				want := quarantined + sidecar
				if _, err := os.Stat(want); err != nil {
					t.Fatalf("quarantined sidecar %s was not kept under the protected spelling: %v", sidecar, err)
				}
				if actual[sidecar] != want {
					t.Fatalf("quarantine wrote sidecar %s at %s, want the protected spelling %s", sidecar, actual[sidecar], want)
				}
			}
		})
	}
}
