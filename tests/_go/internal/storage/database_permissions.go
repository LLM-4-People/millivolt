package storage

// Round 2 C4/C6 regression: an already-private 0600 database must not be
// chmod'd again, while a broader foreign-owned file reports the owner, mode and
// the ownership requirement instead of an opaque chmod failure.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDatabaseFilePermissionNarrowing(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private.db")
	if err := os.WriteFile(private, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if databaseFileNeedsNarrowing(info) {
		t.Fatal("a 0600 database was scheduled for chmod")
	}
	if err := prepareDatabaseFile(private); err != nil {
		t.Fatalf("prepare private database: %v", err)
	}
	if info, err := os.Stat(private); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private mode changed: info=%v err=%v", info, err)
	}

	wide := filepath.Join(dir, "wide.db")
	if err := os.WriteFile(wide, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(wide)
	if err != nil {
		t.Fatal(err)
	}
	if !databaseFileNeedsNarrowing(info) {
		t.Fatal("a 0644 database was not scheduled for narrowing")
	}
	if err := prepareDatabaseFile(wide); err != nil {
		t.Fatalf("prepare broad database: %v", err)
	}
	if info, err := os.Stat(wide); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("broad mode was not narrowed: info=%v err=%v", info, err)
	}
}

func TestDatabaseFilePermissionErrorNamesOwnerAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	if err := os.WriteFile(path, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	err = databaseFilePermissionError(path, info, syscall.EPERM)
	for _, want := range []string{
		path,
		"uid ",
		"/gid ",
		"0644",
		"runtime must own the database",
		"pre-set to 0600",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("permission error %q lacks %q", err, want)
		}
	}
}
