package storage

import (
	"bytes"
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
	// Isolated container smoke mounts /tmp at 32k so a /tmp staging
	// regression cannot hold this packed snapshot. Keep that pin honest.
	const isolatedTmpfs = 32 << 10
	if len(data) <= isolatedTmpfs {
		t.Fatalf("packed snapshot is %d bytes; container /tmp tmpfs is %d so a /tmp staging path would still pass isolated smoke", len(data), isolatedTmpfs)
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

func TestOpenAbortsAdmitWhenSidecarCannotBeRemoved(t *testing.T) {
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
			if err := StageSnapshot(dstPath, packedSnapshot(t, "incoming-row")); err != nil {
				t.Fatal(err)
			}
			blockRemove(t, dstPath+sidecar)
			if _, err := Open(dstPath, testOpts); err == nil {
				t.Fatal("Open succeeded when a WAL/SHM sidecar could not be removed")
			} else if !strings.Contains(err.Error(), "pending snapshot") {
				t.Fatalf("error %q should name the pending snapshot", err)
			}
			if _, err := os.Stat(PendingSnapshotPath(dstPath)); err != nil {
				t.Fatalf("pending snapshot was discarded: %v", err)
			}
			after, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("live database was replaced after a sidecar remove failure")
			}
		})
	}
}

func blockRemove(t *testing.T, path string) {
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

func TestMergeSnapshotTotalsAfterDebugError(t *testing.T) {
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
	_, _, err = live.MergeSnapshot(t.Context(), brokenData)
	if err == nil {
		t.Fatal("merge succeeded with incompatible request_debug")
	}
	if live.Totals().Requests != 2 {
		t.Fatalf("totals after failed debug merge = %d, want 2 (requests already inserted)", live.Totals().Requests)
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
