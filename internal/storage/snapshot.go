package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
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
// file is a derived index that is meaningless without its WAL.
var snapshotSidecarSuffixes = []string{"-wal", "-shm"}

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
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-state-*")
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

// PendingSnapshotPath is the staged SQLite file Open admits for path.
func PendingSnapshotPath(path string) string {
	return path + pendingSnapshotSuffix
}

// DBPath is the running database's own file: the boot-time path the store
// opened, not the reloadable db_path config value (db_path is startup-bound,
// so a reload that changes it leaves the live database at its boot path
// until the process restarts). Its directory is the volume that owns
// database-sized transient staging; see stageDir.
func (s *Store) DBPath() string {
	return s.path
}

// stageDir is where database-sized transient files are staged: the live
// database's own volume, which is contractually sized to hold the database
// and a packed copy of it. A generic /tmp is a small tmpfs in hardened
// deployments and must never cap the backup contract (backup_max_bytes).
func (s *Store) stageDir() string {
	return filepath.Dir(s.path)
}

// SnapshotFile returns the path of a packed, integrity-checked copy of the
// live database and the cleanup that removes its private staging directory.
// VACUUM INTO is the compact consistent snapshot (no free pages); it is not a
// file copy of an open DB.
//
// SnapshotFile is the one owner of the temporary snapshot directory. Backup
// encoding streams this file in bounded chunks, so the caller chooses file
// backing instead of a whole-database heap copy. The caller must call cleanup
// after its last read of path, on every path, including errors from the work
// that consumes the file. On error SnapshotFile removes the directory itself
// and returns an empty path and no cleanup.
func (s *Store) SnapshotFile(ctx context.Context) (path string, cleanup func(), err error) {
	if s == nil {
		return "", nil, ErrStorageDisabled
	}
	if err := s.Flush(); err != nil {
		return "", nil, fmt.Errorf("flush before backup: %w", err)
	}
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-snapshot-*")
	if err != nil {
		return "", nil, backup.StageSnapshotError(s.stageDir(), err)
	}
	cleanup = func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("storage: remove snapshot staging %s: %v", dir, err)
		}
	}
	dest := filepath.Join(dir, "snapshot.db")
	// VACUUM INTO is a read-only operation on the source (SQLite opens the
	// destination separately), so run it on the read pool: the single writer
	// connection stays free for request accounting during the copy. Flush above
	// already committed every accepted record.
	if _, err := s.rdb.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("vacuum snapshot: %w", err)
	}
	// Integrity-check the VACUUM dest in place. CheckDatabase would
	// WriteFile a second packed copy while dest still exists.
	if err := backup.CheckDatabaseFile(dest); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("snapshot: %w", err)
	}
	return dest, cleanup, nil
}

// Snapshot returns a packed, integrity-checked copy of the live database as
// bytes. It is the byte-returning wrapper over SnapshotFile for callers that
// need the whole copy; backup encoding uses SnapshotFile so it never retains
// the database on the heap.
func (s *Store) Snapshot(ctx context.Context) ([]byte, error) {
	path, cleanup, err := s.SnapshotFile(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return os.ReadFile(path)
}

// StageSnapshot writes an integrity-checked SQLite snapshot that Open will
// admit in place of path. The live database is not touched. It is the
// byte-buffered wrapper over the shared publish owner; restore uses
// StageSnapshotFile so the validated database member is never copied into heap.
func StageSnapshot(path string, data []byte) error {
	if path == "" {
		return fmt.Errorf("database path is empty")
	}
	if err := backup.CheckDatabase(filepath.Dir(path), data); err != nil {
		return err
	}
	return publishSnapshot(path, func(w io.Writer) error {
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			return io.ErrShortWrite
		}
		return err
	})
}

// StageSnapshotFile stages an already-validated private packed snapshot at
// path by streaming it into the destination directory's temporary file and
// atomically renaming it to PendingSnapshotPath. Admission runs in place on
// source, so the file-backed restore never writes a database-sized heap copy.
// The source file is left to its owner.
func StageSnapshotFile(path, source string) error {
	if path == "" {
		return fmt.Errorf("database path is empty")
	}
	if source == "" {
		return fmt.Errorf("snapshot source path is empty")
	}
	if err := backup.CheckDatabaseFile(source); err != nil {
		return err
	}
	return publishSnapshot(path, func(w io.Writer) error {
		f, err := os.Open(source)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if _, err := io.CopyN(w, f, info.Size()); err != nil {
			return err
		}
		var extra [1]byte
		if n, err := f.Read(extra[:]); n != 0 {
			return fmt.Errorf("snapshot source changed size while staging")
		} else if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	})
}

// publishSnapshot is the one atomic publish owner for both snapshot wrappers:
// a private temporary file in the destination directory, 0600 permissions, a
// sync before the rename, and the PendingSnapshotPath name Open admits. The
// destination directory is the staging volume, so the rename stays atomic.
func publishSnapshot(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	incoming := path + pendingSnapshotSuffix
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-incoming-*")
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
	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The incoming file is a self-contained rollback-journal database. Remove
	// any sidecar belonging to a superseded incoming snapshot before the new
	// main is renamed into place, so a crash can never pair the new database
	// with stale WAL frames.
	for _, suffix := range snapshotSidecarSuffixes {
		if err := removeIfExists(incoming + suffix); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, incoming); err != nil {
		return err
	}
	committed = true
	return syncSnapshotDir(dir)
}

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

// preflightPendingSnapshot initializes the staged database in place before any
// live file is touched. It opens read-write with the standard bounded busy
// wait and switches to a rollback journal: admission renames only the main
// file over the live path, so a WAL sidecar left behind would either be lost
// or apply to the wrong database. The initializer is the same one Open runs,
// so a snapshot admitted here cannot fail Open's schema sequence later.
func preflightPendingSnapshot(path string) error {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)", path, sqliteBusyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		db.Close()
		return fmt.Errorf("journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "delete") {
		db.Close()
		return fmt.Errorf("journal_mode: got %q, want delete", mode)
	}
	if err := initializeDatabase(db, path); err != nil {
		db.Close()
		return err
	}
	// Schema/migrate alone does not prove the writer can write this database.
	// Run the exact writer INSERT (same column list, conflict clause and value
	// list) inside a transaction and roll it back. An extra required column or
	// an id without a unique key fails here, before the live file is touched.
	if err := probeWriterInsert(db); err != nil {
		db.Close()
		return fmt.Errorf("writer insert probe: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close preflight database: %w", err)
	}
	// A clean close after switching to DELETE leaves no sidecar. Remove any
	// stale one only now: deleting a WAL while it is open would discard its
	// committed pages.
	for _, suffix := range snapshotSidecarSuffixes {
		if err := removeIfExists(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

// writerProbeIDAttempts bounds the sentinel-id search for the admission
// insert probe. Sixteen collisions on a reserved prefix are not a real schema,
// only tampering or a pathological fixture.
const writerProbeIDAttempts = 16

// probeWriterInsert proves the staged database accepts the writer's exact
// INSERT: the shared column list, conflict target and value ordering. The row
// is rolled back, so preflight leaves no data behind.
func probeWriterInsert(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert := batchInsert{tx: tx}
	defer insert.close()
	// The probe must actually execute the INSERT: ON CONFLICT(id) DO NOTHING
	// would make an existing sentinel id a silent no-op that never proves the
	// table's required columns. Pick a free sentinel inside the transaction.
	probeID := "millivolt-preflight"
	free := false
	for suffix := 0; suffix < writerProbeIDAttempts; suffix++ {
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM requests WHERE id = ?)`, probeID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			free = true
			break
		}
		probeID = fmt.Sprintf("millivolt-preflight-%d", suffix)
	}
	if !free {
		return fmt.Errorf("no free preflight request id")
	}
	probe := &metrics.Record{ID: probeID, StatusCode: 200, Start: time.Unix(0, 0)}
	values := appendRecordValues(nil, probe, jsonNull, jsonNull, jsonNull)
	inserted, err := insert.exec(values)
	if err != nil {
		return err
	}
	if !inserted {
		return fmt.Errorf("writer insert probe did not insert its row")
	}
	return tx.Rollback()
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
		if err := removeSnapshotDebris(previous); err != nil {
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
	if err := removeSnapshotDebris(previous); err != nil {
		return err
	}
	return syncSnapshotDir(filepath.Dir(path))
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

// quarantinePendingSnapshot preserves rejected bytes under a diagnostic name
// instead of deleting them. Callers decide whether a failed rename is fatal:
// an integrity/preflight rejection can leave the file for the next boot, while
// interrupted-admission recovery must not fall through to admitting it again.
// Sidecars move first: a crash leaves the main at its original name so the next
// boot classifies the whole set again, and no stale WAL can attach to a later
// incoming file.
func quarantinePendingSnapshot(incoming, suffix string) error {
	for _, sidecar := range snapshotSidecarSuffixes {
		if err := os.Rename(incoming+sidecar, incoming+sidecar+suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(incoming, incoming+suffix)
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// SnapshotOverlap is how many request ids in a packed snapshot already exist
// in the live store. Used by restore inspect. It is the byte-buffered wrapper
// over SnapshotOverlapFile for callers that hold the members in heap.
func (s *Store) SnapshotOverlap(ctx context.Context, data []byte) (int64, error) {
	if s == nil {
		return 0, ErrStorageDisabled
	}
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-overlap-*")
	if err != nil {
		return 0, backup.StageSnapshotError(s.stageDir(), err)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return 0, err
	}
	return s.SnapshotOverlapFile(ctx, src)
}

// SnapshotOverlapFile inspects an already-staged private packed snapshot in
// place and counts the request ids it shares with the live store. It performs
// the same admission as CheckDatabaseFile without writing a second copy, and
// attaches the caller's file read-only.
func (s *Store) SnapshotOverlapFile(ctx context.Context, path string) (int64, error) {
	if s == nil {
		return 0, ErrStorageDisabled
	}
	if err := backup.CheckDatabaseFile(path); err != nil {
		return 0, err
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	conn, err := s.rdb.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS backup_inspect", path); err != nil {
		return 0, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "DETACH DATABASE backup_inspect") }()
	var n int64
	err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_inspect.requests WHERE id IN (SELECT id FROM requests)`).Scan(&n)
	return n, err
}

// MergeSnapshot inserts snapshot rows whose ids are not already in the live
// store (INSERT OR IGNORE). Existing live rows win. The live file is not
// replaced; no restart is required. It is the byte-buffered wrapper over
// MergeSnapshotFile for callers that hold the members in heap.
func (s *Store) MergeSnapshot(ctx context.Context, data []byte) (inserted, skipped int64, err error) {
	if s == nil {
		return 0, 0, ErrStorageDisabled
	}
	// Admin mutations serialize in MergeSnapshotFile, the one merge owner:
	// purge holds the same mutex, so a merge can never queue behind (or race) a
	// delete and starve the writer twice. Staging the caller's bytes does not
	// need the lock.
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-merge-*")
	if err != nil {
		return 0, 0, backup.StageSnapshotError(s.stageDir(), err)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return 0, 0, err
	}
	return s.MergeSnapshotFile(ctx, src)
}

// MergeSnapshotFile inserts the rows of an already-staged private packed
// snapshot whose ids are not already in the live store. The caller's file is
// attached in place and is not copied, removed, or otherwise owned by the
// store. The transaction, live-row-wins rule, debug cap, totals and rollback
// semantics are the merge owner's; the byte wrapper only stages a file.
func (s *Store) MergeSnapshotFile(ctx context.Context, path string) (inserted, skipped int64, err error) {
	if s == nil {
		return 0, 0, ErrStorageDisabled
	}
	var debugDelta int64
	// Admin mutations serialize: purge holds the same mutex, so a merge can
	// never queue behind (or race) a delete and starve the writer twice.
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	if _, err := backup.InspectDatabaseFile(path); err != nil {
		return 0, 0, err
	}
	if err := s.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush before merge: %w", err)
	}
	// Hold the single writer connection for ATTACH through the merge
	// transaction and installTotals so insertBatch cannot account() a
	// concurrent commit against pre-merge aggregates. ATTACH stays outside
	// the transaction; the merge writes only the main database, and the
	// transaction's own view includes its uncommitted rows for the totals scan.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS backup_merge", path); err != nil {
		return 0, 0, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "DETACH DATABASE backup_merge") }()
	// Prove the backup's writable shape before touching any row. INNER-style
	// column intersection alone would silently drop required columns or an
	// incoming NOT NULL column that has no live home; both are data loss.
	if err := preflightMergeSchema(ctx, conn); err != nil {
		return 0, 0, fmt.Errorf("preflight backup merge: %w", err)
	}
	// Request rows, debug sidecars and the totals document commit as one
	// transaction: a failure in any later phase rolls the earlier merges back
	// instead of publishing rows whose totals were never persisted.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	skipped, err = countMergeDuplicates(ctx, tx, "requests")
	if err != nil {
		return 0, 0, fmt.Errorf("count duplicate request rows from backup: %w", err)
	}
	n, err := mergeAttachedTable(ctx, tx, "requests")
	if err != nil {
		return 0, 0, fmt.Errorf("merge request rows from backup: %w", err)
	}
	inserted = n
	if s.debugMaxBytes > 0 {
		// The net sidecar delta is applied under debugMu after commit. Never
		// publish an absolute total computed in this transaction: a capture
		// save cannot interleave while the writer connection is held, but the
		// delta contract is shared with purge so both owners stay exact.
		debugBefore, err := debugByteTotal(ctx, tx)
		if err != nil {
			return 0, 0, fmt.Errorf("debug byte counter before backup merge: %w", err)
		}
		if _, err := s.mergeDebugTable(ctx, tx); err != nil {
			return 0, 0, fmt.Errorf("merge debug captures from backup: %w", err)
		}
		debugAfter, err := debugByteTotal(ctx, tx)
		if err != nil {
			return 0, 0, fmt.Errorf("debug byte counter after backup merge: %w", err)
		}
		debugDelta = debugAfter - debugBefore
	} else if _, err := mergeAttachedTable(ctx, tx, "request_debug"); err != nil {
		return 0, 0, fmt.Errorf("merge debug captures from backup: %w", err)
	}
	var totals Totals
	var totalsEpoch, totalsHighRowID int64
	if inserted > 0 {
		// The post-merge totals scan is maintenance work on the held writer
		// connection: it legitimately reads all history and must not inherit the
		// interactive dashboard query timeout.
		tctx, cancel := s.withMaintenanceTimeout(context.Background())
		defer cancel()
		// The transaction sees its own merged rows, so this scan is exact.
		totals, err = readTotals(tctx, tx)
		if err != nil {
			return inserted, skipped, fmt.Errorf("totals after backup merge: %w", err)
		}
		totalsEpoch, err = projectionEpoch(tctx, tx)
		if err != nil {
			return inserted, skipped, fmt.Errorf("projection epoch after backup merge: %w", err)
		}
		totalsHighRowID, err = projectionHighRowID(tctx, tx)
		if err != nil {
			return inserted, skipped, fmt.Errorf("projection high rowid after backup merge: %w", err)
		}
		if err := saveTotals(tctx, tx, totals, false, totalsEpoch, totalsHighRowID); err != nil {
			return inserted, skipped, fmt.Errorf("persist totals after backup merge: %w", err)
		}
	}
	if err := s.commitDebugDelta(tx, debugDelta); err != nil {
		return inserted, skipped, fmt.Errorf("commit backup merge: %w", err)
	}
	// Committed: these in-memory publications are mandatory and never consult
	// the caller's context.
	if inserted > 0 {
		s.installTotals(totals, totalsEpoch, totalsHighRowID)
	}
	return inserted, skipped, nil
}

type mergeConn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// mergeColumnDetails is the PRAGMA table_info view merge preflight needs:
// whether a column is required and whether SQLite can supply it when omitted.
type mergeColumnDetails struct {
	name       string
	notNull    bool
	hasDefault bool
}

// tableColumnDetails reads one table's columns with their required/default
// metadata. The schema/table arguments are allowlisted because the query is
// built by concatenation.
func tableColumnDetails(ctx context.Context, db mergeConn, schema, table string) ([]mergeColumnDetails, error) {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return nil, fmt.Errorf("unknown backup schema")
	}
	if table != "requests" && table != "request_debug" {
		return nil, fmt.Errorf("unknown merge table")
	}
	query := "PRAGMA table_info(" + table + ")"
	if schema != "" {
		query = "PRAGMA " + schema + ".table_info(" + table + ")"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []mergeColumnDetails
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, mergeColumnDetails{
			name:       name,
			notNull:    notNull != 0,
			hasDefault: dflt.Valid,
		})
	}
	return cols, rows.Err()
}

// preflightMergeSchema rejects a backup whose request or debug table cannot
// accept the live writer's required values before the merge transaction
// touches any row. Two directions matter: a live required column the backup
// omits would insert NULL (or disappear), and an incoming required column the
// live schema does not have would be silently dropped.
func preflightMergeSchema(ctx context.Context, db mergeConn) error {
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name='requests'`).Scan(&name); err != nil {
		return fmt.Errorf("backup requests table: %w", err)
	}
	if err := preflightMergeTable(ctx, db, "requests"); err != nil {
		return err
	}
	haveAttempts := false
	incoming, err := tableColumnDetails(ctx, db, "backup_merge", "requests")
	if err != nil {
		return err
	}
	for _, col := range incoming {
		if col.name == "attempts" {
			haveAttempts = true
			break
		}
	}
	if haveAttempts {
		var malformed int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_merge.requests
			WHERE attempts IS NOT NULL AND attempts <> '' AND json_valid(attempts) = 0`).Scan(&malformed); err != nil {
			return fmt.Errorf("backup requests.attempts: %w", err)
		}
		if malformed != 0 {
			return fmt.Errorf("backup requests.attempts contains %d malformed JSON values", malformed)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name='request_debug'`).Scan(&name); err == nil {
		if err := preflightMergeTable(ctx, db, "request_debug"); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func preflightMergeTable(ctx context.Context, db mergeConn, table string) error {
	live, err := tableColumnDetails(ctx, db, "", table)
	if err != nil {
		return err
	}
	incoming, err := tableColumnDetails(ctx, db, "backup_merge", table)
	if err != nil {
		return err
	}
	liveSet := make(map[string]mergeColumnDetails, len(live))
	for _, col := range live {
		liveSet[col.name] = col
	}
	incomingSet := make(map[string]mergeColumnDetails, len(incoming))
	for _, col := range incoming {
		incomingSet[col.name] = col
	}
	if _, ok := incomingSet["id"]; !ok {
		return fmt.Errorf("backup %s is missing the id column", table)
	}
	for _, col := range live {
		if !col.notNull || col.hasDefault {
			continue
		}
		if _, ok := incomingSet[col.name]; !ok {
			return fmt.Errorf("backup %s is missing required column %q", table, col.name)
		}
	}
	for _, col := range incoming {
		if _, ok := liveSet[col.name]; ok {
			continue
		}
		if col.notNull && !col.hasDefault {
			return fmt.Errorf("backup %s has unsupported required column %q", table, col.name)
		}
	}
	return nil
}

// countMergeDuplicates counts incoming ids that already exist live. The count
// is the explicit skip count; constraint failures are no longer folded into
// it, because INSERT OR IGNORE is gone.
func countMergeDuplicates(ctx context.Context, db mergeConn, table string) (int64, error) {
	if table != "requests" && table != "request_debug" {
		return 0, fmt.Errorf("unknown merge table")
	}
	var n int64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_merge.`+table+` AS incoming
		WHERE EXISTS (SELECT 1 FROM main.`+table+` AS live WHERE live.id = incoming.id)`).Scan(&n)
	return n, err
}

// mergeDebugTable enforces the sidecar byte cap during a restore merge. With
// an active cap it counts only fetchable incoming rows and skips the whole
// debug set (logging the amount) when it cannot fit; request rows still merge.
func (s *Store) mergeDebugTable(ctx context.Context, db mergeConn) (int64, error) {
	if s.debugMaxBytes <= 0 {
		return mergeAttachedTable(ctx, db, "request_debug")
	}
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name='request_debug'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	incoming, err := pragmaTableInfo(ctx, db, "backup_merge", "request_debug")
	if err != nil {
		return 0, err
	}
	where := ""
	var args []any
	if _, ok := incoming["expires_at"]; ok {
		where = `(incoming."expires_at" = 0 OR incoming."expires_at" > ?)`
		args = append(args, time.Now().UnixMilli())
	}
	var current int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug`).Scan(&current); err != nil {
		return 0, err
	}
	statsQuery := `SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM backup_merge.request_debug AS incoming`
	if where != "" {
		statsQuery += " WHERE " + where
	}
	var incomingRows, incomingBytes int64
	if err := db.QueryRowContext(ctx, statsQuery, args...).Scan(&incomingRows, &incomingBytes); err != nil {
		return 0, err
	}
	if remaining := s.debugMaxBytes - current; incomingBytes > remaining {
		log.Printf("storage: backup merge skipped %d debug captures (%d bytes) over the %d-byte store cap",
			incomingRows, incomingBytes, s.debugMaxBytes)
		return 0, nil
	}
	return mergeAttachedTableWhere(ctx, db, "request_debug", where, args...)
}

func mergeAttachedTable(ctx context.Context, db mergeConn, table string) (int64, error) {
	return mergeAttachedTableWhere(ctx, db, table, "", nil)
}

func mergeAttachedTableWhere(ctx context.Context, db mergeConn, table, where string, whereArgs ...any) (int64, error) {
	if table != "requests" && table != "request_debug" {
		return 0, fmt.Errorf("unknown merge table")
	}
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	live, err := pragmaTableInfo(ctx, db, "", table)
	if err != nil {
		return 0, err
	}
	incoming, err := pragmaTableInfo(ctx, db, "backup_merge", table)
	if err != nil {
		return 0, err
	}
	cols := intersectColumns(live, incoming)
	if len(cols) == 0 {
		return 0, fmt.Errorf("backup %s has no columns in common", table)
	}
	selects := make([]string, len(cols))
	for i, col := range cols {
		quoted := `incoming."` + col + `"`
		// Merge owns canonicalization of JSON-bearing columns: the legacy
		// empty spelling becomes the canonical no-value document, while a
		// non-empty value already passed preflight's json_valid check and is
		// copied byte-for-byte.
		if table == "requests" && col == "attempts" {
			quoted = `CASE WHEN incoming."attempts" IS NULL OR incoming."attempts" = '' THEN 'null' ELSE incoming."attempts" END`
		}
		selects[i] = quoted
	}
	list := quoteIdents(cols)
	// No INSERT OR IGNORE: the explicit NOT EXISTS owns duplicate suppression,
	// and every other constraint failure must abort the merge transaction
	// instead of silently discarding an incompatible row.
	q := "INSERT INTO " + table + " (" + list + ") SELECT " + strings.Join(selects, ", ") +
		" FROM backup_merge." + table + " AS incoming" +
		" WHERE NOT EXISTS (SELECT 1 FROM main." + table + " AS live WHERE live.id = incoming.id)"
	if where != "" {
		q += " AND (" + where + ")"
	}
	res, err := db.ExecContext(ctx, q, whereArgs...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pragmaTableInfo guards the schema/table allowlists (the query is built by
// concatenation, so the guards are load-bearing) and delegates the shared
// PRAGMA read to tableColumnSet.
func pragmaTableInfo(ctx context.Context, db mergeConn, schema, table string) (map[string]struct{}, error) {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return nil, fmt.Errorf("unknown backup schema")
	}
	if table != "requests" && table != "request_debug" {
		return nil, fmt.Errorf("unknown merge table")
	}
	return tableColumnSet(ctx, db, schema, table)
}

func intersectColumns(a, b map[string]struct{}) []string {
	var out []string
	for name := range a {
		if _, ok := b[name]; ok && sqliteIdent(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func quoteIdents(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return strings.Join(quoted, ", ")
}

func sqliteIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r != '_' && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}
