package storage

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestSnapshotIncludesFlushedRows(t *testing.T) {
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: "snap-1", Provider: "neutral.example", Model: "neutral", Start: time.Now(), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.CheckDatabase(s.stageDir(), data); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Encode(s.stageDir(), backup.Archive{Database: data}); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotFileContentsAndCleanup pins the file-backed snapshot owner: the
// returned packed file is a valid database in its own private directory,
// cleanup removes that directory, and a failed snapshot returns no path and
// leaves no directory behind.
func TestSnapshotFileContentsAndCleanup(t *testing.T) {
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: "file-snap", Provider: "neutral.example", Model: "n", StatusCode: 200})
	path, cleanup, err := s.SnapshotFile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if path == "" || cleanup == nil {
		t.Fatalf("SnapshotFile returned path=%q cleanup=%v", path, cleanup != nil)
	}
	dir := filepath.Dir(path)
	if err := backup.CheckDatabaseFile(path); err != nil {
		t.Fatalf("SnapshotFile path is not an admitted packed database: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.CheckDatabase(s.stageDir(), data); err != nil {
		t.Fatalf("SnapshotFile content is not an admitted packed database: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the snapshot directory %s: %v", dir, err)
	}
	assertNoSnapshotStaging(t, s.stageDir())

	// A canceled context fails VACUUM after the staging directory exists; the
	// owner must remove it and return no path or cleanup.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path, cleanup, err = s.SnapshotFile(ctx)
	if err == nil {
		t.Fatal("SnapshotFile succeeded with a canceled context")
	}
	if path != "" || cleanup != nil {
		t.Fatalf("failed SnapshotFile returned path=%q cleanup=%v", path, cleanup != nil)
	}
	assertNoSnapshotStaging(t, s.stageDir())
}

func assertNoSnapshotStaging(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		for _, prefix := range []string{"millivolt-snapshot-", "millivolt-merge-", "millivolt-overlap-"} {
			if strings.HasPrefix(e.Name(), prefix) {
				t.Fatalf("snapshot staging directory left behind: %s", e.Name())
			}
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir()+"/snap.db", testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func packedSnapshot(t *testing.T, id string) []byte {
	t.Helper()
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: id, Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenQuarantinesInvalidPendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	dst, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	dst.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := dst.Flush(); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	incoming := PendingSnapshotPath(dstPath)
	if err := os.WriteFile(incoming, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Fatal("invalid pending was not consumed")
	}
	if _, err := os.Stat(incoming + ".invalid"); err != nil {
		t.Fatalf("invalid pending was not quarantined: %v", err)
	}
	got, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep-old" {
		t.Fatalf("live rows after quarantine = %#v", got)
	}
}

// TestOpenQuarantinesIncompatiblePendingSnapshot is the regression for the
// admission ordering bug: a structurally valid SQLite file whose requests
// table cannot be migrated must be rejected during preflight, before the live
// database is moved or removed. The original row must survive and the live
// file must keep its identity.
func TestOpenQuarantinesIncompatiblePendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	dst, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	dst.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := dst.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}

	incoming := PendingSnapshotPath(dstPath)
	bad, err := sql.Open("sqlite", incoming)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Exec(`CREATE TABLE requests (id TEXT PRIMARY KEY); INSERT INTO requests(id) VALUES ('incompatible')`); err != nil {
		t.Fatal(err)
	}
	if err := bad.Close(); err != nil {
		t.Fatal(err)
	}
	// The integrity/header admission passes: only the schema initializer can
	// reject this file, which is exactly the preflight the bug skipped.
	if err := backup.CheckDatabaseFile(incoming); err != nil {
		t.Fatalf("fixture should pass the integrity check: %v", err)
	}

	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after incompatible pending snapshot: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep-old" {
		t.Fatalf("live rows after incompatible pending = %#v, want keep-old", got)
	}
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Fatal("incompatible pending was not consumed")
	}
	if _, err := os.Stat(incoming + ".invalid"); err != nil {
		t.Fatalf("incompatible pending was not quarantined: %v", err)
	}
	if _, err := os.Stat(dstPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("rejected pending left a rollback sibling")
	}
	after, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("live database file was replaced by the rejected pending snapshot")
	}
}

// TestPendingSnapshotRollbackRestoresLiveDatabase pins the admission object's
// rollback contract: after a compatible incoming database is installed, a
// rollback removes it and restores the original database and sidecars.
func TestPendingSnapshotRollbackRestoresLiveDatabase(t *testing.T) {
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
	before, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshot(dstPath, packedSnapshot(t, "replacement")); err != nil {
		t.Fatal(err)
	}
	admission, err := admitPendingSnapshot(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if admission == nil {
		t.Fatal("compatible pending snapshot was not admitted")
	}
	if ids := readRequestIDs(t, dstPath); len(ids) != 1 || ids[0] != "replacement" {
		t.Fatalf("installed ids = %v, want [replacement]", ids)
	}
	if _, err := os.Stat(dstPath + ".previous"); err != nil {
		t.Fatalf("admission did not keep a rollback sibling: %v", err)
	}
	if err := admission.rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rollback did not restore the original live database bytes")
	}
	if ids := readRequestIDs(t, dstPath); len(ids) != 1 || ids[0] != "keep-old" {
		t.Fatalf("restored ids = %v, want [keep-old]", ids)
	}
	if _, err := os.Stat(dstPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("rollback left its previous sibling behind")
	}
}

// TestOpenRecoversInterruptedPendingSnapshot pins the boot fence: a .previous
// sibling means an admission never committed, so the previous database is
// restored and the staged incoming file is quarantined with its own suffix.
func TestOpenRecoversInterruptedPendingSnapshot(t *testing.T) {
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
	if err := os.Rename(dstPath, dstPath+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshot(dstPath, packedSnapshot(t, "interrupted")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep-old" {
		t.Fatalf("rows after interrupted-admission recovery = %#v, want keep-old", got)
	}
	if _, err := os.Stat(dstPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("boot recovery left its previous sibling behind")
	}
	incoming := PendingSnapshotPath(dstPath)
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Fatal("boot recovery re-admitted the interrupted incoming file")
	}
	if _, err := os.Stat(incoming + ".invalid-interrupted"); err != nil {
		t.Fatalf("boot recovery did not quarantine the interrupted incoming file: %v", err)
	}
}

// TestOpenRecoversInterruptedPendingSnapshotAfterInstall covers the other
// crash window: the incoming file already replaced the live path but commit
// never ran. Boot must discard the uncommitted new database and restore the
// previous one.
func TestOpenRecoversInterruptedPendingSnapshotAfterInstall(t *testing.T) {
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
	if err := StageSnapshot(dstPath, packedSnapshot(t, "uncommitted")); err != nil {
		t.Fatal(err)
	}
	admission, err := admitPendingSnapshot(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if admission == nil {
		t.Fatal("compatible pending snapshot was not admitted")
	}
	// Simulate a crash here: no commit, no rollback, store never opened.
	if ids := readRequestIDs(t, dstPath); len(ids) != 1 || ids[0] != "uncommitted" {
		t.Fatalf("installed ids = %v, want [uncommitted]", ids)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep-old" {
		t.Fatalf("rows after post-install recovery = %#v, want keep-old", got)
	}
	if _, err := os.Stat(dstPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("boot recovery left its previous sibling behind")
	}
}

// TestOpenAdmitsPendingSnapshotOverLiveAndCommits pins the successful replace
// path with a live database present: the incoming rows win, and commit removes
// the rollback sibling.
func TestOpenAdmitsPendingSnapshotOverLiveAndCommits(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	live, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	live.Record(&metrics.Record{ID: "old-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := StageSnapshot(dstPath, packedSnapshot(t, "replacement")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	ids := readRequestIDs(t, dstPath)
	if len(ids) != 1 || ids[0] != "replacement" {
		t.Fatalf("admitted ids = %v, want [replacement]", ids)
	}
	if _, err := os.Stat(dstPath + ".previous"); !os.IsNotExist(err) {
		t.Fatal("commit left its rollback sibling behind")
	}
	if _, err := os.Stat(PendingSnapshotPath(dstPath)); !os.IsNotExist(err) {
		t.Fatal("admission left the incoming file behind")
	}
}

func readRequestIDs(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestOpenLeavesPendingOnUnreadableIncoming(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	if err := StageSnapshot(dstPath, packedSnapshot(t, "staged")); err != nil {
		t.Fatal(err)
	}
	incoming := PendingSnapshotPath(dstPath)
	if err := os.Chmod(incoming, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(incoming, 0o600) })
	if _, err := Open(dstPath, testOpts); err == nil {
		t.Fatal("Open succeeded with an unreadable pending snapshot")
	}
	if _, err := os.Stat(incoming); err != nil {
		t.Fatalf("unreadable pending was discarded: %v", err)
	}
	if _, err := os.Stat(incoming + ".invalid"); !os.IsNotExist(err) {
		t.Fatal("unreadable pending was quarantined as invalid")
	}
}

// TestOpenRestoresLiveWhenSidecarRenameFails pins rollback safety for the
// sidecar moves: the live database is moved aside with its WAL/SHM siblings,
// so a sidecar rename that cannot complete must abort admission and restore
// the original files instead of deleting or replacing the live database.
func TestOpenRestoresLiveWhenSidecarRenameFails(t *testing.T) {
	for _, sidecar := range []string{"-wal", "-shm"} {
		t.Run(sidecar, func(t *testing.T) {
			dir := t.TempDir()
			dstPath := filepath.Join(dir, "dst.db")
			dst, err := Open(dstPath, testOpts)
			if err != nil {
				t.Fatal(err)
			}
			dst.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
			if err := dst.Flush(); err != nil {
				t.Fatal(err)
			}
			dst.Close()
			before, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			const stale = "stale-sidecar"
			if err := os.WriteFile(dstPath+sidecar, []byte(stale), 0o600); err != nil {
				t.Fatal(err)
			}
			// Block the rollback name: renaming the sidecar over a non-empty
			// directory fails after the live main file was already moved.
			blockPath(t, dstPath+".previous"+sidecar)
			if err := StageSnapshot(dstPath, packedSnapshot(t, "incoming-row")); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dstPath, testOpts); err == nil {
				t.Fatal("Open succeeded when a live WAL/SHM sidecar could not be moved aside")
			} else {
				msg := err.Error()
				if !strings.Contains(msg, "pending snapshot") {
					t.Fatalf("error %q should name the pending snapshot", msg)
				}
				// Exactly one level owns the label: the Open wrap. A doubled
				// "pending snapshot: pending snapshot:" means admitPendingSnapshot
				// started wrapping its own cause again.
				if n := strings.Count(msg, "pending snapshot:"); n != 1 {
					t.Fatalf("error %q carries %d \"pending snapshot:\" prefixes, want exactly 1", msg, n)
				}
			}
			if _, err := os.Stat(PendingSnapshotPath(dstPath)); err != nil {
				t.Fatalf("pending snapshot was discarded: %v", err)
			}
			after, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("live database was replaced after a sidecar rename failure")
			}
			if got, err := os.ReadFile(dstPath + sidecar); err != nil || string(got) != stale {
				t.Fatalf("live sidecar after failed admission = %q, %v; want the original bytes", got, err)
			}
		})
	}
}

func blockPath(t *testing.T, path string) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMergeSnapshotKeepsLiveRows(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	live.Record(&metrics.Record{ID: "live-only", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "shared", Provider: "backup.example", Model: "n", StatusCode: 200})
	src.Record(&metrics.Record{ID: "backup-only", Provider: "backup.example", Model: "n", StatusCode: 200})
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
	if inserted != 1 || skipped != 1 {
		t.Fatalf("inserted=%d skipped=%d", inserted, skipped)
	}
	if _, err := live.db.Exec("DETACH DATABASE backup_merge"); err == nil {
		t.Fatal("backup_merge still attached after MergeSnapshot")
	}
	got, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*metrics.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if byID["shared"] == nil || byID["shared"].Provider != "live.example" {
		t.Fatalf("live row lost on conflict: %#v", byID["shared"])
	}
	if byID["live-only"] == nil || byID["backup-only"] == nil {
		t.Fatalf("merged rows = %#v", got)
	}
	if live.Totals().Requests != 3 {
		t.Fatalf("totals after merge = %d, want 3", live.Totals().Requests)
	}
}

// TestMergeSnapshotFileKeepsLiveRows pins the file-backed merge owner: the
// caller's packed file is attached in place, live rows still win, the counts
// and totals match the byte wrapper, and neither the source file nor a merge
// staging copy is left behind.
func TestMergeSnapshotFileKeepsLiveRows(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	live.Record(&metrics.Record{ID: "live-only", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "shared", Provider: "backup.example", Model: "n", StatusCode: 200})
	src.Record(&metrics.Record{ID: "backup-only", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := src.SnapshotFile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	inserted, skipped, err := live.MergeSnapshotFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 || skipped != 1 {
		t.Fatalf("inserted=%d skipped=%d", inserted, skipped)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file-backed merge did not leave the caller's file to its owner: %v", err)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after MergeSnapshotFile")
	}
	got, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*metrics.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if byID["shared"] == nil || byID["shared"].Provider != "live.example" {
		t.Fatalf("live row lost on conflict: %#v", byID["shared"])
	}
	if byID["live-only"] == nil || byID["backup-only"] == nil {
		t.Fatalf("merged rows = %#v", got)
	}
	if live.Totals().Requests != 3 {
		t.Fatalf("totals after merge = %d, want 3", live.Totals().Requests)
	}
	assertNoSnapshotStaging(t, live.stageDir())
}

// TestStageSnapshotFileStagesAndAdmits pins the file-backed replace owner: the
// validated source is streamed into the pending name with 0600 permissions,
// the source stays with its owner, and Open admits the staged snapshot.
func TestStageSnapshotFileStagesAndAdmits(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := Open(srcPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "file-stage", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	source, cleanup, err := src.SnapshotFile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	src.Close()
	want, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(dir, "dst.db")
	if err := StageSnapshotFile(dstPath, source); err != nil {
		t.Fatal(err)
	}
	incoming := PendingSnapshotPath(dstPath)
	got, err := os.ReadFile(incoming)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("file-backed stage changed the snapshot bytes")
	}
	info, err := os.Stat(incoming)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pending snapshot permissions = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("file-backed stage consumed its source: %v", err)
	}
	dst, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Fatal("pending snapshot was not consumed")
	}
	recent, err := dst.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].ID != "file-stage" {
		t.Fatalf("restored rows = %#v", recent)
	}
}

// TestSnapshotOverlapFileInspectsInPlace pins the path-backed inspect owner:
// it counts the id intersection without writing a copy and leaves the caller's
// file untouched.
func TestSnapshotOverlapFileInspectsInPlace(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "packed.db")
	if err := os.WriteFile(path, packedSnapshot(t, "shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := live.SnapshotOverlapFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("overlap = %d, want 1", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("path-backed overlap removed its caller's file: %v", err)
	}
	if n := attachedDatabaseCount(t, live, "backup_inspect"); n != 0 {
		t.Fatal("backup_inspect still attached after SnapshotOverlapFile")
	}
	assertNoSnapshotStaging(t, live.stageDir())
}

// TestAttachedSnapshotIsReadOnly pins the inspect and merge ATTACH contract:
// the attached packed snapshot rejects writes (so a restore path can never
// mutate the caller's file or its own source), the shared SQLiteURI keeps
// metacharacter paths literal, and the live database remains writable for the
// merge. The write is attempted on the same connection shape each production
// site uses: the read pool for inspect, the held writer for merge.
func TestAttachedSnapshotIsReadOnly(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "shared", Provider: "backup.example", Model: "n", StatusCode: 200})
	src.Record(&metrics.Record{ID: "backup-only", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A literal filename with every URI metacharacter the shared helper owns.
	path := filepath.Join(t.TempDir(), "packed ?#% name.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		alias string
		db    *sql.DB
	}{
		{name: "inspect read pool", alias: "backup_inspect", db: live.rdb},
		{name: "merge writer", alias: "backup_merge", db: live.db},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := tc.db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := attachSnapshotDatabase(t.Context(), conn, tc.alias, path); err != nil {
				t.Fatalf("attach %s: %v", tc.alias, err)
			}
			defer conn.ExecContext(context.Background(), "DETACH DATABASE "+tc.alias)
			if _, err := conn.ExecContext(t.Context(),
				"INSERT INTO "+tc.alias+`.request_debug (id, session_id, captured_at, expires_at, payload)
				VALUES ('attached-write', '', 0, 0, '')`); err == nil {
				t.Fatalf("write through attached %s succeeded; attached snapshot is not read-only", tc.alias)
			}
		})
	}
	// Inspect and merge still work through the same helper.
	if n, err := live.SnapshotOverlapFile(t.Context(), path); err != nil || n != 1 {
		t.Fatalf("overlap through readonly attach = %d, %v; want 1, nil", n, err)
	}
	inserted, skipped, err := live.MergeSnapshotFile(t.Context(), path)
	if err != nil {
		t.Fatalf("merge through readonly attach: %v", err)
	}
	if inserted != 1 || skipped != 1 {
		t.Fatalf("merge inserted=%d skipped=%d, want 1/1", inserted, skipped)
	}
	if got, err := live.LoadRecent(t.Context(), 10); err != nil || len(got) != 2 {
		t.Fatalf("live rows after merge = %d, %v; want 2 rows", len(got), err)
	}
}

func TestMergeSnapshotRejectsIncompatibleDebugTable(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	data := packedSnapshot(t, "from-backup")
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE request_debug; CREATE TABLE request_debug (foo TEXT); INSERT INTO request_debug(foo) VALUES ('x')`); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.db")
	if _, err := db.Exec("VACUUM INTO ?", broken); err != nil {
		t.Fatal(err)
	}
	db.Close()
	brokenData, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = live.MergeSnapshot(t.Context(), brokenData); err == nil {
		t.Fatal("merge succeeded with an incompatible request_debug table")
	} else if !strings.Contains(err.Error(), "request_debug") {
		t.Fatalf("merge error %q does not name the incompatible debug table", err)
	}
	if live.Totals().Requests != 1 {
		t.Fatalf("totals after rejected debug merge = %d, want 1", live.Totals().Requests)
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("rows after rejected debug merge = %d, want only the live row", n)
	}
}

// TestMergeSnapshotSkipsDebugOverCap pins the sidecar cap across a restore:
// when the incoming fetchable captures do not fit the remaining budget, the
// whole debug set is skipped while request rows still merge.
func TestMergeSnapshotSkipsDebugOverCap(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 64 << 10
	live, err := Open(filepath.Join(t.TempDir(), "live-debug-cap.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	now := time.Now()
	if err := live.SaveDebugCapture(t.Context(), "live-cap", "sess", now.UnixMilli(), 0, make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	if err := src.SaveDebugCapture(t.Context(), "backup-cap", "sess", now.UnixMilli(), 0, make([]byte, 100<<10)); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.MergeSnapshot(t.Context(), data); err != nil {
		t.Fatal(err)
	}
	got, err := live.CountDebugSession(t.Context(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("debug captures after capped merge = %d, want only the live row", got)
	}
	live.debugMu.Lock()
	bytes := live.debugBytes
	live.debugMu.Unlock()
	if bytes != 32<<10 {
		t.Fatalf("debug byte counter = %d, want %d", bytes, 32<<10)
	}
}

func debugByteCounter(s *Store) int64 {
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	return s.debugBytes
}

func metaValue(t *testing.T, s *Store, key string) string {
	t.Helper()
	var value string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func projectionStateEpoch(t *testing.T, s *Store) int64 {
	t.Helper()
	var epoch int64
	if err := s.db.QueryRow(`SELECT epoch FROM request_projection_state WHERE singleton = 1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func attachedDatabaseCount(t *testing.T, s *Store, name string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_database_list WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMergeSnapshotTotalsFailureRollsBack pins merge atomicity: when the
// totals document cannot be persisted, the request rows and debug sidecars
// merged earlier in the same transaction disappear with it, in-memory totals
// and the sidecar byte counter stay untouched, ATTACH is cleaned up, and a
// healthy retry merges the archive exactly once.
func TestMergeSnapshotTotalsFailureRollsBack(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	live, err := Open(filepath.Join(t.TempDir(), "merge-rollback.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	const livePayload = "live-debug"
	if err := live.SaveDebugCapture(t.Context(), "live-debug", "sess", time.Now().UnixMilli(), 0, []byte(livePayload)); err != nil {
		t.Fatal(err)
	}

	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	const backupPayload = "backup-debug"
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", time.Now().UnixMilli(), 0, []byte(backupPayload)); err != nil {
		t.Fatal(err)
	}
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	beforeTotals := live.Totals()
	beforeDoc := metaValue(t, live, totalsMetaKey)
	beforeEpoch := projectionStateEpoch(t, live)
	beforeBytes := debugByteCounter(live)
	if beforeTotals.Requests != 1 || beforeBytes != int64(len(livePayload)) {
		t.Fatalf("test setup: totals=%+v debug bytes=%d", beforeTotals, beforeBytes)
	}
	if _, err := live.db.Exec(`CREATE TRIGGER test_merge_totals_abort BEFORE INSERT ON meta
		WHEN NEW.key = 'totals' BEGIN SELECT RAISE(ABORT, 'injected totals persist failure'); END`); err != nil {
		t.Fatal(err)
	}

	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err == nil {
		t.Fatal("merge succeeded while the totals document was rejected")
	}
	if !strings.Contains(err.Error(), "totals") {
		t.Fatalf("merge error %q does not name the totals failure", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("request rows after failed merge = %d, want only the live row", n)
	}
	recent, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recent {
		if r.ID == "backup-row" {
			t.Fatal("backup request row survived the rolled-back merge")
		}
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || raw != nil {
		t.Fatalf("backup debug sidecar after failed merge = %q, %v", raw, err)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "live-debug"); err != nil || string(raw) != livePayload {
		t.Fatalf("live debug sidecar after failed merge = %q, %v", raw, err)
	}
	if got := live.Totals(); got != beforeTotals {
		t.Fatalf("in-memory totals after failed merge = %+v, want %+v", got, beforeTotals)
	}
	if got := metaValue(t, live, totalsMetaKey); got != beforeDoc {
		t.Fatalf("persisted totals after failed merge changed: %s", got)
	}
	if got := projectionStateEpoch(t, live); got != beforeEpoch {
		t.Fatalf("projection epoch after failed merge = %d, want %d", got, beforeEpoch)
	}
	if got := debugByteCounter(live); got != beforeBytes {
		t.Fatalf("debug byte counter after failed merge = %d, want %d", got, beforeBytes)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after failed merge")
	}

	if _, err := live.db.Exec(`DROP TRIGGER test_merge_totals_abort`); err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err = live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("healthy retry: %v", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("retry counts inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	if n := countRows(t, live); n != 2 {
		t.Fatalf("request rows after retry = %d, want 2", n)
	}
	if got := live.Totals().Requests; got != 2 {
		t.Fatalf("totals after retry = %d, want 2", got)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || string(raw) != backupPayload {
		t.Fatalf("backup debug sidecar after retry = %q, %v", raw, err)
	}
	if got, want := debugByteCounter(live), int64(len(livePayload)+len(backupPayload)); got != want {
		t.Fatalf("debug byte counter after retry = %d, want %d", got, want)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after retry")
	}
}

// TestMergeSnapshotDebugCapDisabledMergesSidecars covers the inserted==0
// branch with the sidecar cap disabled: debug-only backup rows commit without
// recomputing request totals.
func TestMergeSnapshotDebugCapDisabledMergesSidecars(t *testing.T) {
	live := openTestStore(t)
	src := openTestStore(t)
	payload := []byte(`{"body":"neutral"}`)
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", time.Now().UnixMilli(), 0, payload); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := live.Totals()
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("debug-only merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	if got := live.Totals(); got != before {
		t.Fatalf("debug-only merge recomputed totals: %+v -> %+v", before, got)
	}
	raw, err := live.LoadDebugCapture(t.Context(), "backup-debug")
	if err != nil || string(raw) != string(payload) {
		t.Fatalf("backup debug sidecar = %q, %v", raw, err)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after debug-only merge")
	}
}

// TestMergeSnapshotExpiredMaintenanceDeadlineRollsBack pins fail-closed totals
// scanning: an already expired maintenance budget fails the totals phase and
// rolls the earlier request and debug merges back instead of publishing rows
// with stale totals.
func TestMergeSnapshotExpiredMaintenanceDeadlineRollsBack(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	opts.MaintenanceTimeout = time.Nanosecond
	live, err := Open(filepath.Join(t.TempDir(), "merge-deadline.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	const livePayload = "live-debug"
	if err := live.SaveDebugCapture(t.Context(), "live-debug", "sess", time.Now().UnixMilli(), 0, []byte(livePayload)); err != nil {
		t.Fatal(err)
	}

	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", time.Now().UnixMilli(), 0, []byte("backup-debug")); err != nil {
		t.Fatal(err)
	}
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	beforeTotals := live.Totals()
	beforeBytes := debugByteCounter(live)
	if _, _, err := live.MergeSnapshot(t.Context(), data); err == nil {
		t.Fatal("merge succeeded with an expired maintenance budget")
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("request rows after expired-deadline merge = %d, want only the live row", n)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || raw != nil {
		t.Fatalf("backup debug sidecar after expired-deadline merge = %q, %v", raw, err)
	}
	if got := live.Totals(); got != beforeTotals {
		t.Fatalf("totals after expired-deadline merge = %+v, want %+v", got, beforeTotals)
	}
	if got := debugByteCounter(live); got != beforeBytes {
		t.Fatalf("debug byte counter after expired-deadline merge = %d, want %d", got, beforeBytes)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after expired-deadline merge")
	}
}

func TestSnapshotOverlapDetaches(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	n, err := live.SnapshotOverlap(t.Context(), packedSnapshot(t, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("overlap = %d, want 1", n)
	}
	n, err = live.SnapshotOverlap(t.Context(), packedSnapshot(t, "other"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second overlap = %d, want 0 (leftover attach would fail this call)", n)
	}
}
