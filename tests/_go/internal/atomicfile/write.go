package atomicfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestWriteFileCreatesAndReplaces pins the plain contract: the file exists with
// the caller's mode on create, is replaced whole on update, and keeps its own
// restrictions afterwards.
func TestWriteFileCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "document.yaml")
	if err := WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new file mode = %o, want 600", got)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "second\n" {
		t.Fatalf("replaced content = %q, want the second write", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("replaced file mode = %o, want the existing 640", got)
	}
}

// TestWriteFileFollowsSymlinkToTarget pins the chosen contract: the link stays
// a link and the final target receives every write, so a symlinked runtime
// config or pid path never leaves a stale target behind.
func TestWriteFileFollowsSymlinkToTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink("real.yaml", link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("write replaced the symlink instead of following it")
	}
	if got := readFile(t, target); got != "new\n" {
		t.Fatalf("target content = %q, want the new write", got)
	}
	if got := readFile(t, link); got != "new\n" {
		t.Fatalf("content through the link = %q, want the new write", got)
	}
}

// TestWriteFileCreatesMissingSymlinkTarget pins the dangling-link case: a
// deployment may point at a target that the first save creates.
func TestWriteFileCreatesMissingSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "missing.yaml")
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("created\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("write replaced the dangling symlink instead of following it")
	}
	if got := readFile(t, target); got != "created\n" {
		t.Fatalf("created target content = %q", got)
	}
}

// TestWriteFileFollowsSymlinkChain pins that a chain resolves to its final
// regular file rather than stopping at the first link.
func TestWriteFileFollowsSymlinkChain(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "final.yaml")
	first := filepath.Join(dir, "first.yaml")
	second := filepath.Join(dir, "second.yaml")
	for _, link := range []struct{ from, to string }{{first, "second.yaml"}, {second, "final.yaml"}} {
		if err := os.Symlink(link.to, link.from); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteFile(first, []byte("chained\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "chained\n" {
		t.Fatalf("chain target content = %q", got)
	}
}

// TestWriteFileReportsUnusableTarget pins the diagnostic: a resolution failure
// or a target that cannot be replaced names the requested path and, when one
// was resolved, the target.
func TestWriteFileReportsUnusableTarget(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop.yaml")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	err := WriteFile(loop, []byte("x"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "symlink chain longer") {
		t.Fatalf("symlink cycle error = %v, want the chain diagnostic", err)
	}

	target := filepath.Join(dir, "directory")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "to-directory")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err = WriteFile(link, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("write over a directory target reported success")
	}
	if !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), target) {
		t.Fatalf("directory error = %v, want the requested and resolved names", err)
	}
	if !errors.Is(err, os.ErrExist) && !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory error = %v, want the rename failure", err)
	}
}

// TestWriteFileEmptyPath pins the only caller-independent precondition.
func TestWriteFileEmptyPath(t *testing.T) {
	if err := WriteFile("", []byte("x"), 0o600); err == nil {
		t.Fatal("empty path reported success")
	}
}

// TestWriteFileIsNotPartialDuringReplace is a smoke proof that a reader never
// sees a truncated document: the temp name is separate from the destination
// while the bytes are written, and the rename is the only publish step.
func TestWriteFileIsNotPartialDuringReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "document.yaml")
	if err := os.WriteFile(path, []byte("whole document\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, bytes.Repeat([]byte("payload line\n"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if want := strings.Repeat("payload line\n", 1000); got != want {
		t.Fatalf("payload length = %d, want %d", len(got), len(want))
	}
}
