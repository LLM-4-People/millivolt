//go:build unix

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// databaseFileOwner names the owning uid/gid for a permission failure. The
// syscall stat shape carries numeric ownership on every Unix target; an
// unexpected shape falls back to the shared unknown-owner spelling so the
// error still states the ownership requirement.
func databaseFileOwner(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("uid %d/gid %d", stat.Uid, stat.Gid)
	}
	return unknownDatabaseFileOwner
}
