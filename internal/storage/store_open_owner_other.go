//go:build !unix

package storage

import "os"

// databaseFileOwner has no numeric file owner on this platform. The permission
// failure still names the path, current mode and the ownership requirement.
func databaseFileOwner(os.FileInfo) string {
	return unknownDatabaseFileOwner
}
