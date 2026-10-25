package storage

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// snapshotDirSyncHook is a test seam (production nil) for observing or failing
// directory durability. It runs wherever the snapshot protocol syncs the
// database directory.
var snapshotDirSyncHook func(string) error

// syncSnapshotDir makes rename and unlink ordering durable. A directory entry
// is not persistent until the containing directory is synced, so an admission
// that trusts its renames without this can be reverted by a power loss.
func syncSnapshotDir(dir string) error {
	if snapshotDirSyncHook != nil {
		return snapshotDirSyncHook(dir)
	}
	f, err := os.Open(dir)
	if os.IsNotExist(err) {
		// No directory means no snapshot entry can have been created there;
		// Open still owns the canonical missing-directory diagnostic.
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// writeSnapshotState publishes the admission state as one atomic, durable
// marker: temp file, file sync, rename, directory sync.
func writeSnapshotState(path, state string) error {
	target := path + pendingSnapshotStateSuffix
	tmp, err := os.CreateTemp(filepath.Dir(path), snapshotStateTempPattern(path))
	if err != nil {
		return err
	}
	staged := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(staged)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.WriteString(state + "\n"); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return err
	}
	committed = true
	return syncSnapshotDir(filepath.Dir(path))
}

// readSnapshotState returns the durable admission state, or "" when no marker
// exists. A malformed marker fails closed instead of guessing a transition.
func readSnapshotState(path string) (string, error) {
	raw, err := os.ReadFile(path + pendingSnapshotStateSuffix)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	state := strings.TrimSpace(string(raw))
	switch state {
	case snapshotStatePending, snapshotStateCommitted:
		return state, nil
	default:
		return "", fmt.Errorf("invalid pending snapshot state %q", state)
	}
}

// removeSnapshotState clears the marker only after the state it describes is
// itself durable. The directory sync keeps a later boot from seeing a stale
// marker against a newer file set.
func removeSnapshotState(path string) error {
	if err := removeIfExists(path + pendingSnapshotStateSuffix); err != nil {
		return err
	}
	return syncSnapshotDir(filepath.Dir(path))
}

// recoverInterruptedSnapshot is the boot fence for an admission that crashed
// before or during commit. The durable state marker, not the presence of one
// sidecar, decides the transition:
//
//   - pending: the new database was never committed. While the previous main
//     is still aside, every file at the live name is the uncommitted incoming
//     set: its sidecars are removed before its main, then the previous
//     main+sidecar set is restored and the staged incoming file is
//     quarantined. This also covers a crash inside an earlier recovery: a main
//     already restored at path is left alone while its remaining sidecars move
//     back, so a committed WAL frame is never mistaken for commit debris.
//   - committed: the replacement is permanent. Any surviving rollback set is
//     a stale directory entry and is removed without reverting live rows.
//   - no marker: a live database wins over a stale rollback entry; restore is
//     only possible when the main file is missing.
func recoverInterruptedSnapshot(path string) error {
	state, err := readSnapshotState(path)
	if err != nil {
		return err
	}
	previous := path + pendingSnapshotPreviousSuffix
	switch state {
	case snapshotStateCommitted:
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("committed snapshot state but live database %s is missing", path)
			}
			return err
		}
		if err := removeSnapshotDebris(previous); err != nil {
			return err
		}
		return syncSnapshotDir(filepath.Dir(path))
	case snapshotStatePending:
		return restorePendingSnapshot(path, previous, state)
	}
	if _, err := os.Stat(path); err == nil {
		// The live main file exists, so a leftover rollback entry cannot be the
		// authoritative copy. Remove it instead of reverting committed rows.
		if err := removeMarkerlessSnapshotDebris(previous); err != nil {
			return err
		}
		return syncSnapshotDir(filepath.Dir(path))
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(previous); err == nil {
		return restorePendingSnapshot(path, previous, state)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := removeMarkerlessSnapshotDebris(previous); err != nil {
		return err
	}
	return syncSnapshotDir(filepath.Dir(path))
}

// removeMarkerlessSnapshotDebris removes a rollback set when no durable
// admission marker exists, warning before it discards a non-empty
// `.previous-wal`. Markerless state is the pre-marker admission layout: a live
// main plus `.previous-wal` can be an interrupted recovery that restored the
// main before its WAL, so those frames may be committed. Without a marker they
// cannot be classified, and the conservative no-marker policy keeps the live
// main; the warning makes the possible data loss visible instead of silent.
// Marked pending and committed recovery never uses this helper.
func removeMarkerlessSnapshotDebris(previous string) error {
	wal := previous + "-wal"
	if info, err := os.Stat(wal); err == nil {
		if info.Size() > 0 {
			log.Printf("storage: markerless snapshot recovery: discarding non-empty rollback WAL %s (%d bytes); without a state marker its committed frames cannot be distinguished from debris and may be lost", wal, info.Size())
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return removeSnapshotDebris(previous)
}

// restorePendingSnapshot restores the previous database set.
//
// When the previous main still exists, the live-set removal phase did not
// finish. Admission moves the live sidecars before the live main, so every
// file at the live name then belongs to the uncommitted incoming set (or is an
// orphaned incoming sidecar): its sidecars are removed before its main. That
// covers a recovery that removed the incoming main but crashed before its WAL;
// a surviving WAL must never look like an incoming sibling for the restored
// previous main.
//
// The one exception is a staged incoming main, which proves the replacement
// never reached the live name. A pending marker from the earlier
// main-before-sidecars ordering can therefore have a committed previous WAL
// still at the live name; it is moved back into the rollback set rather than
// deleted.
//
// Once the previous main moved back, a crash before its sidecars leaves a live
// main plus `.previous-wal`. Those remaining sidecars are renamed back, never
// discarded, so committed WAL frames survive.
//
// Markerless recovery (an admission from before the durable marker existed)
// cannot distinguish a previous WAL left at the live name by the old
// main-before-sidecars ordering from an incoming WAL left behind by the old
// recovery. It keeps live sidecars, which favors committed frames over
// discarding them; the marked states above are unambiguous.
func restorePendingSnapshot(path, previous, state string) error {
	previousMain := false
	if _, err := os.Stat(previous); err == nil {
		previousMain = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if previousMain && state == snapshotStatePending {
		incoming := path + pendingSnapshotSuffix
		if _, err := os.Stat(incoming); err == nil {
			// The staged incoming main was never installed, so live sidecars
			// cannot belong to it. Move them into the rollback set: the
			// earlier main-before-sidecars ordering could have left a
			// committed previous WAL at the live name.
			for _, suffix := range snapshotSidecarSuffixes {
				src := path + suffix
				if _, err := os.Stat(src); err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return err
				}
				dst := previous + suffix
				if _, err := os.Stat(dst); err == nil {
					return fmt.Errorf("both live and rollback snapshot sidecars exist: %s", suffix)
				} else if !os.IsNotExist(err) {
					return err
				}
				if err := os.Rename(src, dst); err != nil {
					return err
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		} else {
			// Remove the incoming sidecars before the incoming main so a crash
			// can never leave an uncommitted WAL behind with no main for the
			// next boot to classify.
			for _, suffix := range snapshotSidecarSuffixes {
				if err := removeIfExists(path + suffix); err != nil {
					return err
				}
			}
			if err := removeIfExists(path); err != nil {
				return err
			}
		}
	}
	if previousMain {
		// The previous main moves back before its sidecars. A crash between
		// the two leaves a live main plus `.previous-wal`, which the next boot
		// resumes instead of treating the WAL as debris.
		if err := os.Rename(previous, path); err != nil {
			return err
		}
	}
	for _, suffix := range snapshotSidecarSuffixes {
		src := previous + suffix
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.Rename(src, path+suffix); err != nil {
			return err
		}
	}
	if err := syncSnapshotDir(filepath.Dir(path)); err != nil {
		return err
	}
	incoming := path + pendingSnapshotSuffix
	if _, err := os.Stat(incoming); err == nil {
		// Failing here must not fall through to admission: re-admitting the
		// interrupted file is exactly what this fence exists to prevent.
		if err := quarantinePendingSnapshot(incoming, pendingSnapshotInterruptedSuffix); err != nil {
			return fmt.Errorf("quarantine interrupted pending snapshot: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if state != "" {
		if err := removeSnapshotState(path); err != nil {
			return err
		}
	}
	log.Printf("storage: restored database from interrupted pending snapshot at %s", path)
	return nil
}

// removeSnapshotDebris removes one rollback set: main file first, then its
// sidecars. Sidecar removal is not fatal once the main marker is gone.
func removeSnapshotDebris(previous string) error {
	if err := removeIfExists(previous); err != nil {
		return err
	}
	for _, suffix := range snapshotSidecarSuffixes {
		if err := removeIfExists(previous + suffix); err != nil {
			log.Printf("storage: remove rollback sidecar %s: %v", previous+suffix, err)
		}
	}
	return nil
}
