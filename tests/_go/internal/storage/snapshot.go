package storage

import (
	"bytes"
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
	if err := backup.CheckDatabase(data); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Encode(backup.Archive{Database: data}); err != nil {
		t.Fatal(err)
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
