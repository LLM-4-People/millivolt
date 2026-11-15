// Package atomicfile owns the one atomic whole-file replacement used by the
// process writers that must never expose a partial or truncated document.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// maxSymlinkDepth bounds a symlink chain so a cycle reports instead of
// spinning.
const maxSymlinkDepth = 40

// WriteFile atomically replaces the file path names with data. A symlink is
// followed to its final target, so the link keeps pointing at the file that was
// actually updated instead of being replaced by a regular file and leaving a
// stale target behind. The temporary file is created beside the target, so the
// publish rename stays within one directory; the target's existing permission
// bits are preserved and a new target gets perm.
//
// The diagnostic names both the requested path and the resolved target when a
// symlink led elsewhere, so a write that lands in an unexpected place is
// visible from the error alone.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	if path == "" {
		return errors.New("write file: empty path")
	}
	target, err := resolveTarget(path)
	if err != nil {
		return writeError(path, "", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+"-*")
	if err != nil {
		return writeError(path, target, err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if info, err := os.Stat(target); err == nil {
		// Preserve the destination's restrictions: a config may hold secrets,
		// and a pid file is world-readable by convention.
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			return writeError(path, target, err)
		}
	} else if !os.IsNotExist(err) {
		return writeError(path, target, err)
	} else if err := tmp.Chmod(perm); err != nil {
		return writeError(path, target, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return writeError(path, target, err)
	}
	if err := tmp.Close(); err != nil {
		return writeError(path, target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return writeError(path, target, err)
	}
	return nil
}

// resolveTarget follows every symlink in path's final component. A missing
// final target is valid (the write creates it); a missing directory in the
// chain is not, and the later CreateTemp reports it.
func resolveTarget(path string) (string, error) {
	for range maxSymlinkDepth {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	return "", fmt.Errorf("symlink chain longer than %d", maxSymlinkDepth)
}

// writeError labels the failing operation with the requested and resolved
// paths. The target is omitted when resolution itself failed, so the message
// never claims a resolution it does not have.
func writeError(path, target string, err error) error {
	if target == "" || target == path {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return fmt.Errorf("write %s (symlink target %s): %w", path, target, err)
}
