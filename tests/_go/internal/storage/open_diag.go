package storage

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A CANTOPEN startup failure must be diagnosable from the log alone: it
// names the database path and the failing directory's state, not just the
// bare SQLite text. See docs/operations.md for the deployment remediation.

func TestOpenCantOpenNamesUnwritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "proxy.db")
	_, err := Open(path, testOpts)
	if err == nil {
		t.Fatal("Open succeeded in an unwritable directory")
	}
	for _, want := range []string{
		"unable to open database file",
		path,
		`directory "` + dir + `" is not writable by uid ` + strconv.Itoa(os.Getuid()),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestOpenCantOpenNamesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	path := filepath.Join(dir, "proxy.db")
	_, err := Open(path, testOpts)
	if err == nil {
		t.Fatal("Open succeeded with a missing database directory")
	}
	for _, want := range []string{
		"unable to open database file",
		path,
		`directory "` + dir + `" does not exist`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
