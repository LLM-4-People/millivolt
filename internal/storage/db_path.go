package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// maxSymlinkHops bounds the manual final-component resolution below; the
// kernel's own limit is smaller, so this is only a loop guard.
const maxSymlinkHops = 40

// ResolveDatabasePath returns the file identity SQLite actually opens for a
// configured db_path. db_path may be a symlink: SQLite follows it, while
// os.Rename operates on the directory entry, so admission derived from the
// link spelling would replace the link and leave the real file (and its row
// data) untouched. Resolving once at every boundary that derives admission
// names keeps the pending slot, rollback set and state marker beside the real
// file, and leaves the operator's link in place pointing at the replacement.
//
// A final symlink whose target does not exist yet is resolved through
// os.Readlink so a first boot creates the configured target. An ordinary
// missing file is returned unchanged (Open owns its directory diagnosis).
// Symlink loops and unreadable components fail closed instead of guessing.
func ResolveDatabasePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("database path is empty")
	}
	path = filepath.Clean(path)
	for range maxSymlinkHops {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve database path %q: %w", path, err)
		}
		target, linkErr := os.Readlink(path)
		if linkErr != nil {
			if errors.Is(linkErr, fs.ErrNotExist) || errors.Is(linkErr, fs.ErrInvalid) {
				return path, nil
			}
			return "", fmt.Errorf("resolve database path %q: %w", path, linkErr)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return "", fmt.Errorf("resolve database path: more than %d symlink levels", maxSymlinkHops)
}
