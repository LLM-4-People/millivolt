package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// walCarries reports whether the marker is still readable in either the main
// database file or its write-ahead log. secure_delete makes the live pages
// clean; only a checkpoint removes the pre-delete frames that still hold the
// old page images.
func walCarries(t *testing.T, path, marker string) (main, wal bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	main = bytes.Contains(raw, []byte(marker))
	raw, err = os.ReadFile(path + "-wal")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	wal = bytes.Contains(raw, []byte(marker))
	return main, wal
}

// A purge must not leave the deleted rows recoverable from the WAL sidecar.
// The fixture proves the marker really is in the log before the delete, so a
// missing checkpoint cannot pass because SQLite happened to checkpoint early.
func TestPurgeCheckpointsWAL(t *testing.T) {
	const marker = "wal-residue-provider-7f3a"
	path := filepath.Join(t.TempDir(), "purge-wal.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Record(&metrics.Record{ID: "wal-row", Provider: marker, Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if main, wal := walCarries(t, path, marker); !wal {
		t.Fatalf("fixture: marker missing from the WAL (main=%v)", main)
	}

	if _, err := s.Clear(t.Context(), &PurgeFilter{Provider: marker}, nil); err != nil {
		t.Fatal(err)
	}
	if main, wal := walCarries(t, path, marker); main || wal {
		t.Fatalf("purge left the deleted provider readable (main=%v wal=%v)", main, wal)
	}
}

// The expired-capture sweep is the other destructive path: it must reclaim the
// payload from the WAL, not only hide the row from reads.
func TestDebugExpireCheckpointsWAL(t *testing.T) {
	const marker = "wal-residue-capture-9c51"
	path := filepath.Join(t.TempDir(), "debug-wal.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	if err := s.SaveDebugCapture(t.Context(), "expired-wal", "sess",
		now.UnixMilli(), now.Add(-time.Minute).UnixMilli(), []byte(marker)); err != nil {
		t.Fatal(err)
	}
	if main, wal := walCarries(t, path, marker); !wal {
		t.Fatalf("fixture: marker missing from the WAL (main=%v)", main)
	}

	deleted, err := s.ExpireDebugCaptures(t.Context(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expired captures deleted = %d, want 1", deleted)
	}
	if main, wal := walCarries(t, path, marker); main || wal {
		t.Fatalf("expiry left the capture payload readable (main=%v wal=%v)", main, wal)
	}
}
