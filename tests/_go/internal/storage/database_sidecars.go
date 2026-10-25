package storage

// Round 2 C5 regression: SQLite writes WAL/SHM sidecars beside the resolved
// database file. A configured db_path that is a symlink must narrow the
// target's sidecars, not nonexistent files beside the link spelling.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestRestrictDatabaseSidecarsFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
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
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(target + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(target+suffix, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The journal is deliberately absent: a missing sidecar must not fail.
	if err := restrictDatabaseSidecars(link); err != nil {
		t.Fatalf("restrictDatabaseSidecars: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Stat(target + suffix)
		if err != nil {
			t.Fatalf("stat resolved sidecar %s: %v", suffix, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("resolved %s mode = %04o, want 0600", suffix, got)
		}
		if _, err := os.Stat(link + suffix); !os.IsNotExist(err) {
			t.Fatalf("a sidecar was created beside the link spelling: %s", suffix)
		}
	}
}
