package storage

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

// snapshotAdmission is an installed pending snapshot. Open commits it only
// after the store is fully constructed and the writer goroutine starts; any
// earlier error rolls it back to the previous database.
type snapshotAdmission struct {
	path      string
	installed bool // incoming now lives at path
	previous  bool // the live database (and sidecars) moved to .previous
}

// admitPendingSnapshot installs a staged replacement database before Open.
// Nothing destructive happens until the incoming file passes both the shared
// integrity check and the same schema/trigger/migrate initializer Open runs,
// so a structurally valid but incompatible file is quarantined while the live
// database stays untouched. Its errors are returned bare: Open owns the
// single "pending snapshot:" label.
func admitPendingSnapshot(path string) (*snapshotAdmission, error) {
	if err := recoverInterruptedSnapshot(path); err != nil {
		return nil, err
	}
	incoming := path + pendingSnapshotSuffix
	// The pending slot is one private file, not a path to follow. Check its
	// type and header before backup.CheckDatabaseFile opens it: a FIFO would
	// block that open forever, while a directory or symlink must be quarantined
	// without touching the live database or the symlink target.
	if err := checkPendingSnapshotFile(incoming); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if !errors.Is(err, backup.ErrInvalidSnapshot) {
			return nil, err
		}
		if qerr := quarantinePendingSnapshot(incoming, pendingSnapshotInvalidSuffix); qerr != nil {
			log.Printf("storage: quarantine pending snapshot %s: %v", incoming, qerr)
		}
		log.Printf("storage: pending snapshot rejected: %v", err)
		return nil, nil
	}
	// A packed pending snapshot is a self-contained file. Any sidecar beside
	// it is crash debris from an interrupted preflight, not part of the
	// snapshot: a hot rollback journal would either fail the read-only
	// integrity open or, worse, replay pages from an unrelated snapshot when
	// opened read-write. Remove the set before inspecting the main file.
	if err := removeSnapshotSidecars(incoming); err != nil {
		return nil, err
	}
	if err := backup.CheckDatabaseFile(incoming); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		if !errors.Is(err, backup.ErrInvalidSnapshot) {
			return nil, err
		}
		if qerr := quarantinePendingSnapshot(incoming, pendingSnapshotInvalidSuffix); qerr != nil {
			log.Printf("storage: quarantine pending snapshot %s: %v", incoming, qerr)
		}
		log.Printf("storage: pending snapshot rejected: %v", err)
		return nil, nil
	}
	if err := preflightPendingSnapshot(incoming); err != nil {
		// An incoming file that cannot be initialized in place is no more
		// usable than an integrity-invalid one: preserve its bytes as
		// quarantine and keep serving the live database.
		if qerr := quarantinePendingSnapshot(incoming, pendingSnapshotInvalidSuffix); qerr != nil {
			log.Printf("storage: quarantine pending snapshot %s: %v", incoming, qerr)
		}
		log.Printf("storage: pending snapshot rejected: %v", err)
		return nil, nil
	}
	a := &snapshotAdmission{path: path}
	if err := a.install(); err != nil {
		return nil, err
	}
	return a, nil
}

// install moves the live database aside and renames the preflighted incoming
// file over it. Any failure restores the previous set before returning. The
// pending state marker is durable before the first rename, so a crash at any
// step is recoverable without guessing from the files that happen to exist.
// Sidecars move before the main: while a previous main exists, every sidecar
// at the live name is then identifiable as incoming debris.
func (a *snapshotAdmission) install() error {
	previous := a.path + pendingSnapshotPreviousSuffix
	incoming := a.path + pendingSnapshotSuffix
	// A rollback sibling before the pending marker is written means the file
	// set is already inconsistent. Fail without publishing a pending state:
	// the next boot must not treat the live database as an uncommitted set.
	if _, err := os.Stat(previous); err == nil {
		return fmt.Errorf("rollback sibling already exists: %s", previous)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := writeSnapshotState(a.path, snapshotStatePending); err != nil {
		return fmt.Errorf("write pending snapshot state: %w", err)
	}
	if _, err := os.Stat(a.path); err == nil {
		// The rollback flag covers sidecar-only progress: if a sidecar move
		// fails before the main moves, rollback still has a set to restore.
		a.previous = true
		for _, suffix := range snapshotSidecarSuffixes {
			live := a.path + suffix
			if _, err := os.Stat(live); err == nil {
				if err := os.Rename(live, previous+suffix); err != nil {
					return errors.Join(err, a.restorePrevious())
				}
			} else if !os.IsNotExist(err) {
				return errors.Join(err, a.restorePrevious())
			}
		}
		if err := os.Rename(a.path, previous); err != nil {
			return errors.Join(err, a.restorePrevious())
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		// A sidecar without its live database cannot belong to the incoming
		// file; remove it so it cannot apply to the database after the rename.
		for _, suffix := range snapshotSidecarSuffixes {
			if err := removeIfExists(a.path + suffix); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(incoming, a.path); err != nil {
		return errors.Join(err, a.restorePrevious())
	}
	a.installed = true
	if err := syncSnapshotDir(filepath.Dir(a.path)); err != nil {
		return errors.Join(err, a.restorePrevious())
	}
	return nil
}

// restorePrevious undoes an installed or partially installed admission. The
// uncommitted sidecars are removed before the uncommitted main, so a crash can
// never leave an incoming WAL behind to attach to a restored main. The previous
// main is restored before its sidecars: a crash between those renames leaves a
// live main plus remaining `.previous-*` files, which the next boot resumes.
//
// An installed incoming set must be fully removed before the previous set is
// touched. If any live sidecar or main removal fails, the previous phase is
// skipped: renaming the previous main over a live incoming main while an
// incoming WAL still occupies the live name would leave those frames attached
// to the restored database. Flags, marker and previous set all survive for the
// next Open to retry.
//
// Every phase keeps its flag while it is incomplete, and the pending state
// marker is cleared only after every required removal, rename, and directory
// sync succeeds. A failure therefore leaves the next Open the full evidence
// and fence it needs to retry without discarding committed WAL frames.
func (a *snapshotAdmission) restorePrevious() error {
	var errs []error
	if a.installed {
		removed := true
		for _, suffix := range snapshotSidecarSuffixes {
			if err := removeIfExists(a.path + suffix); err != nil {
				errs = append(errs, err)
				removed = false
			}
		}
		if removed {
			if err := removeIfExists(a.path); err != nil {
				errs = append(errs, err)
				removed = false
			}
		}
		if !removed {
			return errors.Join(errs...)
		}
		a.installed = false
	}
	if a.previous {
		previousMain := true
		src := a.path + pendingSnapshotPreviousSuffix
		if _, err := os.Stat(src); err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
				previousMain = false
			}
		} else if err := os.Rename(src, a.path); err != nil {
			errs = append(errs, err)
			previousMain = false
		}
		if previousMain {
			remaining := false
			for _, suffix := range snapshotSidecarSuffixes {
				src := a.path + pendingSnapshotPreviousSuffix + suffix
				if _, err := os.Stat(src); err != nil {
					if !os.IsNotExist(err) {
						errs = append(errs, err)
						remaining = true
					}
					continue
				}
				if err := os.Rename(src, a.path+suffix); err != nil {
					errs = append(errs, err)
					remaining = true
				}
			}
			if !remaining {
				a.previous = false
			}
		}
	}
	if err := syncSnapshotDir(filepath.Dir(a.path)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		if err := removeSnapshotState(a.path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rollback restores the pre-admission database. It is safe on a nil admission
// and on a store that never installed anything.
func (a *snapshotAdmission) rollback() error {
	if a == nil {
		return nil
	}
	return a.restorePrevious()
}

// commit makes the admission permanent. The committed marker is written and
// directory-synced before the rollback set is removed: recovery then has a
// durable fact to consult, instead of inferring commit from a missing file.
// Once the marker is durable the replacement is live, so cleanup failures are
// logged and left to the next boot rather than failing a serving store.
func (a *snapshotAdmission) commit() error {
	if a == nil {
		return nil
	}
	if err := writeSnapshotState(a.path, snapshotStateCommitted); err != nil {
		return err
	}
	if a.previous {
		if err := removeSnapshotDebris(a.path + pendingSnapshotPreviousSuffix); err != nil {
			log.Printf("storage: remove pending snapshot rollback set: %v", err)
		} else {
			a.previous = false
		}
	}
	if err := syncSnapshotDir(filepath.Dir(a.path)); err != nil {
		log.Printf("storage: sync database directory after snapshot commit: %v", err)
	}
	return nil
}
