package storage

import (
	"os"
	"path/filepath"
)

// pendingSnapshotSuffix is the validated SQLite file a Settings restore
// writes next to db_path. Open admits it before the live file so a database
// restore cannot mutate an open store.
const pendingSnapshotSuffix = ".incoming"

const (
	// pendingSnapshotPreviousSuffix is the rollback sibling an admission moves
	// the live database (and its sidecars) to before installing the incoming
	// file. Its presence marks an uncommitted admission: boot restores it.
	pendingSnapshotPreviousSuffix = ".previous"
	// pendingSnapshotInvalidSuffix quarantines a staged file that failed
	// integrity or in-place initialization. The live database stays live.
	pendingSnapshotInvalidSuffix = ".invalid"
	// pendingSnapshotInterruptedSuffix quarantines a staged file left behind
	// by an admission that crashed before commit; recovery restores the
	// previous database and preserves the unused bytes for inspection.
	pendingSnapshotInterruptedSuffix = ".invalid-interrupted"
	// pendingSnapshotStateSuffix is the durable admission-state marker. It is
	// written and directory-synced before any rollback-set rename, and it is
	// what recovery consults: the presence or absence of `.previous` alone
	// cannot distinguish a pending restore from commit debris. A committed
	// marker is left in place after a successful admission so a stale rollback
	// entry resurrected by a lost directory update is still classified as
	// debris instead of reverting committed rows.
	pendingSnapshotStateSuffix = ".snapshot-state"
)

// Admission state values stored in pendingSnapshotStateSuffix.
const (
	snapshotStatePending   = "pending"
	snapshotStateCommitted = "committed"
)

// snapshotSidecarSuffixes is the ordered set of SQLite sidecar suffixes the
// admission protocol moves, removes, and restores. WAL precedes SHM: the SHM
// file is a derived index that is meaningless without its WAL. A rollback
// journal is also a sidecar here: preflight switches the staged file to
// journal_mode=DELETE, so a crash mid-preflight can leave a hot journal that
// would otherwise be applied to a later, different pending snapshot.
var snapshotSidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// Database-sized transient staging patterns this package creates on the live
// database's volume. They are the only storage-owned names the startup sweep
// may reclaim. The sweep also excludes every path derived from the live
// database by identity, so a db_path whose basename matches one of these
// patterns, its sidecars, the pending slot, the rollback set and the state
// marker remain protected regardless of name shape.
const (
	snapshotStagingPattern = "millivolt-snapshot-*"
	overlapStagingPattern  = "millivolt-overlap-*"
	mergeStagingPattern    = "millivolt-merge-*"
)

// snapshotIncomingTempPattern and snapshotStateTempPattern are the two
// atomic-publish temp names in the live database's directory.
func snapshotIncomingTempPattern(path string) string {
	return "." + filepath.Base(path) + "-incoming-*"
}

func snapshotStateTempPattern(path string) string {
	return "." + filepath.Base(path) + "-state-*"
}

// PendingSnapshotPath is the staged SQLite file Open admits for path.
func PendingSnapshotPath(path string) string {
	return path + pendingSnapshotSuffix
}

// RemovePendingSnapshot removes a staged pending snapshot and every SQLite
// sidecar it owns. Sidecars go first so a crash cannot leave a stale WAL, SHM
// or hot rollback journal beside a missing main file. It is the one owner of
// pending-slot cleanup outside the admission protocol itself.
func RemovePendingSnapshot(path string) error {
	if err := removeSnapshotSidecars(path); err != nil {
		return err
	}
	return removeIfExists(path)
}

// quarantinePendingSnapshot preserves rejected bytes under a diagnostic name
// instead of deleting them. Callers decide whether a failed rename is fatal:
// an integrity/preflight rejection can leave the file for the next boot, while
// interrupted-admission recovery must not fall through to admitting it again.
// Sidecars move first: a crash leaves the main at its original name so the next
// boot classifies the whole set again, and no stale WAL can attach to a later
// incoming file. The quarantine suffix goes on the main name before the SQLite
// sidecar suffix (incoming.invalid-wal), so the whole quarantined set shares
// the protected base spelling the startup sweep's path fence knows.
func quarantinePendingSnapshot(incoming, suffix string) error {
	quarantined := incoming + suffix
	for _, sidecar := range snapshotSidecarSuffixes {
		if err := os.Rename(incoming+sidecar, quarantined+sidecar); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(incoming, quarantined)
}

// removeSnapshotSidecars removes every SQLite sidecar for path. It is the one
// owner of the "packed snapshot is self-contained" cleanup: publication,
// preflight and admission all discard sidecars before opening their main file,
// so a stale journal or WAL can never attach to a different snapshot.
func removeSnapshotSidecars(path string) error {
	for _, suffix := range snapshotSidecarSuffixes {
		if err := removeIfExists(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}
