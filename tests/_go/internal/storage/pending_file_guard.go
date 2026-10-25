package storage

// Round 2 C1/C2/C3 regressions: the pending snapshot path is a private file
// slot, not a path to follow. A symlink, directory, empty file or headerless
// file must be quarantined without touching the live database, and the shared
// preflight must reject the same shapes before opening SQLite.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func openLiveRow(t *testing.T, path, id string) {
	t.Helper()
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(&metrics.Record{ID: id, Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func requireLiveRow(t *testing.T, path, id string) {
	t.Helper()
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("live rows = %#v, want %s", rows, id)
	}
}

func TestAdmissionRejectsSymlinkedPendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	openLiveRow(t, dstPath, "keep-old")

	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, packedSnapshot(t, "replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	incoming := PendingSnapshotPath(dstPath)
	if err := os.Symlink(target, incoming); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after symlinked pending snapshot: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(dstPath); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("live path became a symlink: info=%v err=%v", info, err)
	}
	requireLiveRow(t, dstPath, "keep-old")
	if _, err := os.Lstat(incoming); !os.IsNotExist(err) {
		t.Fatalf("symlinked pending snapshot was not consumed: %v", err)
	}
	if _, err := os.Lstat(incoming + pendingSnapshotInvalidSuffix); err != nil {
		t.Fatalf("symlinked pending snapshot was not quarantined: %v", err)
	}
}

func TestAdmissionRejectsDirectoryPendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	openLiveRow(t, dstPath, "keep-old")
	incoming := PendingSnapshotPath(dstPath)
	if err := os.Mkdir(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatalf("Open after directory pending snapshot: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	requireLiveRow(t, dstPath, "keep-old")
	if _, err := os.Lstat(incoming + pendingSnapshotInvalidSuffix); err != nil {
		t.Fatalf("directory pending snapshot was not quarantined: %v", err)
	}
}

func TestValidatePendingSnapshotRejectsEmptyAndHeaderless(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		message string
	}{
		{"empty", nil, "empty"},
		{"headerless", []byte("not a sqlite database"), "SQLite header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dstPath := filepath.Join(dir, "dst.db")
			incoming := PendingSnapshotPath(dstPath)
			if err := os.WriteFile(incoming, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			err := ValidatePendingSnapshot(dstPath)
			if err == nil {
				t.Fatal("ValidatePendingSnapshot accepted an unusable pending file")
			}
			if !errors.Is(err, backup.ErrInvalidSnapshot) {
				t.Fatalf("validation error = %v, want ErrInvalidSnapshot", err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("validation error = %v, want %q", err, tc.message)
			}
			if _, statErr := os.Stat(incoming); statErr != nil {
				t.Fatalf("validation consumed the rejected pending file: %v", statErr)
			}
		})
	}
}
