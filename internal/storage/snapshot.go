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
)

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
	if err := os.Rename(staged, incoming); err != nil {
		return err
	}
	committed = true
	return nil
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
	if err := db.Close(); err != nil {
		return fmt.Errorf("close preflight database: %w", err)
	}
	// A clean close after switching to DELETE leaves no sidecar. Remove any
	// stale one only now: deleting a WAL while it is open would discard its
	// committed pages.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := removeIfExists(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

// recoverInterruptedSnapshot is the boot fence for an admission that crashed
// before commit. A .previous main file means the new database was never
// committed, so its path/sidecars are removed and the previous set restored.
// A staged .incoming from that attempt is quarantined rather than re-admitted
// (an operator can inspect and re-stage it deliberately).
func recoverInterruptedSnapshot(path string) error {
	previous := path + pendingSnapshotPreviousSuffix
	if _, err := os.Stat(previous); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// A previous sidecar without its main file is commit debris: commit
		// removes the main sibling first, then the sidecars.
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := removeIfExists(previous + suffix); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		// The incoming file already replaced path: remove the uncommitted new
		// set before restoring the previous one.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := removeIfExists(path + suffix); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		// Crash between renaming the live database and renaming the incoming
		// file: sidecars still at path belong to the previous database.
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(path + suffix); err == nil {
				if err := os.Rename(path+suffix, previous+suffix); err != nil {
					return err
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
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
	log.Printf("storage: restored database from interrupted pending snapshot at %s", path)
	return nil
}

// install moves the live database aside and renames the preflighted incoming
// file over it. Any failure restores the previous set before returning.
func (a *snapshotAdmission) install() error {
	previous := a.path + pendingSnapshotPreviousSuffix
	incoming := a.path + pendingSnapshotSuffix
	if _, err := os.Stat(a.path); err == nil {
		if _, err := os.Stat(previous); err == nil {
			return fmt.Errorf("rollback sibling already exists: %s", previous)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(a.path, previous); err != nil {
			return err
		}
		a.previous = true
		for _, suffix := range []string{"-wal", "-shm"} {
			live := a.path + suffix
			if _, err := os.Stat(live); err == nil {
				if err := os.Rename(live, previous+suffix); err != nil {
					return errors.Join(err, a.restorePrevious())
				}
			} else if !os.IsNotExist(err) {
				return errors.Join(err, a.restorePrevious())
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		// A sidecar without its live database cannot belong to the incoming
		// file; remove it so it cannot apply to the database after the rename.
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := removeIfExists(a.path + suffix); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(incoming, a.path); err != nil {
		return errors.Join(err, a.restorePrevious())
	}
	a.installed = true
	return nil
}

// restorePrevious undoes an installed or partially installed admission. The
// uncommitted new file is removed first; then every previous sibling that
// exists is renamed back over the live name.
func (a *snapshotAdmission) restorePrevious() error {
	var errs []error
	if a.installed {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := removeIfExists(a.path + suffix); err != nil {
				errs = append(errs, err)
			}
		}
		a.installed = false
	}
	if a.previous {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			src := a.path + pendingSnapshotPreviousSuffix + suffix
			if _, err := os.Stat(src); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				errs = append(errs, err)
				continue
			}
			if err := os.Rename(src, a.path+suffix); err != nil {
				errs = append(errs, err)
			}
		}
		a.previous = false
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

// commit makes the admission permanent. The previous main file is removed
// first: once it is gone the admission cannot be rolled back, so a later
// sidecar cleanup failure is logged instead of failing a serving store.
func (a *snapshotAdmission) commit() error {
	if a == nil || !a.previous {
		return nil
	}
	if err := removeIfExists(a.path + pendingSnapshotPreviousSuffix); err != nil {
		return err
	}
	a.previous = false
	for _, suffix := range []string{"-wal", "-shm"} {
		stale := a.path + pendingSnapshotPreviousSuffix + suffix
		if err := removeIfExists(stale); err != nil {
			log.Printf("storage: remove pending snapshot rollback sidecar %s: %v", stale, err)
		}
	}
	return nil
}

// quarantinePendingSnapshot preserves rejected bytes under a diagnostic name
// instead of deleting them. Callers decide whether a failed rename is fatal:
// an integrity/preflight rejection can leave the file for the next boot, while
// interrupted-admission recovery must not fall through to admitting it again.
func quarantinePendingSnapshot(incoming, suffix string) error {
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
	// Admin mutations serialize: purge holds the same mutex, so a merge can
	// never queue behind (or race) a delete and starve the writer twice.
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	info, err := backup.InspectDatabaseFile(path)
	if err != nil {
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
	// Request rows, debug sidecars and the totals document commit as one
	// transaction: a failure in any later phase rolls the earlier merges back
	// instead of publishing rows whose totals were never persisted.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	n, err := mergeAttachedTable(ctx, tx, "requests")
	if err != nil {
		return 0, 0, fmt.Errorf("merge request rows from backup: %w", err)
	}
	inserted = n
	skipped = int64(info.Requests) - inserted
	if skipped < 0 {
		skipped = 0
	}
	var debugErr error
	var debugBytes int64
	debugBytesKnown := false
	if _, err := s.mergeDebugTable(ctx, tx); err != nil {
		debugErr = err
	} else if s.debugMaxBytes > 0 {
		// The held writer connection makes this transaction's sidecar sum the
		// exact committed state. Read it now, publish it only after commit.
		debugBytes, err = debugByteTotal(ctx, tx)
		if err != nil {
			debugErr = fmt.Errorf("debug byte counter after backup merge: %w", err)
		} else {
			debugBytesKnown = true
		}
	}
	var totals Totals
	if inserted > 0 {
		// The post-merge totals scan is maintenance work on the held writer
		// connection: it legitimately reads all history and must not inherit the
		// interactive dashboard query timeout.
		tctx, cancel := s.withMaintenanceTimeout(context.Background())
		defer cancel()
		// The transaction sees its own merged rows, so this scan is exact.
		totals, err = readTotals(tctx, tx)
		if err != nil {
			return inserted, skipped, mergePhaseError(debugErr, "totals after backup merge", err)
		}
		epoch, err := projectionEpoch(tctx, tx)
		if err != nil {
			return inserted, skipped, mergePhaseError(debugErr, "projection epoch after backup merge", err)
		}
		highRowID, err := projectionHighRowID(tctx, tx)
		if err != nil {
			return inserted, skipped, mergePhaseError(debugErr, "projection high rowid after backup merge", err)
		}
		if err := saveTotals(tctx, tx, totals, false, epoch, highRowID); err != nil {
			return inserted, skipped, mergePhaseError(debugErr, "persist totals after backup merge", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, skipped, mergePhaseError(debugErr, "commit backup merge", err)
	}
	// Committed: these in-memory publications are mandatory and never consult
	// the caller's context.
	if debugBytesKnown {
		s.installDebugBytes(debugBytes)
	}
	if inserted > 0 {
		s.installTotals(totals)
	}
	if debugErr != nil {
		return inserted, skipped, debugErr
	}
	return inserted, skipped, nil
}

// mergePhaseError attaches a non-fatal debug merge error to the fatal phase
// error, preserving both for errors.Is.
func mergePhaseError(debugErr error, phase string, fatal error) error {
	if debugErr != nil {
		return fmt.Errorf("%w; %s: %w", debugErr, phase, fatal)
	}
	return fmt.Errorf("%s: %w", phase, fatal)
}

type mergeConn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
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
	if _, ok := incoming["payload"]; !ok {
		return mergeAttachedTable(ctx, db, "request_debug")
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
	list := quoteIdents(cols)
	q := "INSERT OR IGNORE INTO " + table + " (" + list + ") SELECT " + list +
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
