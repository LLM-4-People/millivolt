package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
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
	dir, err := os.MkdirTemp(s.stageDir(), snapshotStagingPattern)
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

// ValidatePendingSnapshot runs the shared admission preflight against the
// pending snapshot staged for path, without installing it. A restore calls it
// immediately after StageSnapshotFile so a replacement the next boot would
// reject (an extra required column, a missing id key, a trigger that aborts
// the writer INSERT) fails while the config member is still unwritten and the
// pending guard can roll back. The live database is untouched. The staged file
// may be migrated in place and its stale sidecars removed, exactly as a boot
// admission would do; the caller owns removing or quarantining it on failure.
func ValidatePendingSnapshot(path string) error {
	if path == "" {
		return fmt.Errorf("database path is empty")
	}
	return preflightPendingSnapshot(PendingSnapshotPath(path))
}

// publishSnapshot is the one atomic publish owner for both snapshot wrappers:
// a private temporary file in the destination directory, 0600 permissions, a
// sync before the rename, and the PendingSnapshotPath name Open admits. The
// destination directory is the staging volume, so the rename stays atomic.
func publishSnapshot(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	incoming := path + pendingSnapshotSuffix
	tmp, err := os.CreateTemp(dir, snapshotIncomingTempPattern(path))
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
	// with stale WAL frames or replay a hot rollback journal over it.
	if err := removeSnapshotSidecars(incoming); err != nil {
		return err
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

// preflightPendingSnapshot initializes the staged database in place before any
// live file is touched. It opens read-write with the standard bounded busy
// wait and switches to a rollback journal: admission renames only the main
// file over the live path, so a WAL sidecar left behind would either be lost
// or apply to the wrong database. The initializer is the same one Open runs,
// so a snapshot admitted here cannot fail Open's schema sequence later.
func preflightPendingSnapshot(path string) error {
	// The packed file is self-contained; sidecars beside it are debris from an
	// interrupted earlier preflight. Remove them before opening: a hot journal
	// would otherwise be applied to this (possibly different) snapshot and
	// silently replace its pages with older data. A clean preflight close
	// leaves none of its own, so the post-close pass below is a fence for a
	// filesystem that retained the unlink.
	if err := removeSnapshotSidecars(path); err != nil {
		return err
	}
	params := url.Values{}
	params.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeout.Milliseconds()))
	db, err := sql.Open("sqlite", backup.SQLiteURI(path, params))
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
	// initializeDatabase reconciles the four owned analytical triggers but
	// leaves every other trigger in place. An unowned write-path trigger on
	// requests could delete or rewrite accepted rows after the writer has
	// accounted them, so replacement admission rejects the whole set instead
	// of trusting the writer probe (which reads its own sentinel back).
	if err := preflightRequestTriggers(context.Background(), db); err != nil {
		db.Close()
		return err
	}
	// The initializer proves the writer INSERT executes, but a NULL request id
	// or another stored value scanRecord cannot read into its destination
	// would still pass that probe and then make every later history scan fail.
	// SQLite permits NULL in a TEXT PRIMARY KEY, including tables created
	// before the fresh schema gained NOT NULL, and it stores any value class in
	// any non-STRICT column (stream='abc', stream=2), so check the staged rows
	// before any live file is touched.
	if err := preflightRequestScanValues(context.Background(), db, "", "pending snapshot"); err != nil {
		db.Close()
		return err
	}
	// The legacy empty attempts spelling is a valid admission input (scans
	// treat it as no attempts), but HasError/count/export/purge run json_each
	// over the raw column. Canonicalize it to the writer's own no-value
	// document in the staged file before any live file is touched; merge does
	// the equivalent during its INSERT.
	if err := canonicalizePendingAttempts(context.Background(), db); err != nil {
		db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close preflight database: %w", err)
	}
	// A clean close after switching to DELETE leaves no sidecar. Remove any
	// stale one only now: deleting a WAL while it is open would discard its
	// committed pages.
	return removeSnapshotSidecars(path)
}

// preflightRequestTriggers rejects every trigger on requests that is not one of
// the four analytical triggers this package owns. ensureProjectionTriggers has
// already restored the owned definitions, so any other name is foreign: a
// replacement snapshot is a database file, and an unowned INSERT/UPDATE/DELETE
// trigger could silently delete or rewrite writes that insertBatch still counts.
func preflightRequestTriggers(ctx context.Context, db mergeConn) error {
	owned := make(map[string]struct{}, len(projectionTriggers))
	for _, tr := range projectionTriggers {
		owned[tr.name] = struct{}{}
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'trigger' AND tbl_name = 'requests'`)
	if err != nil {
		return fmt.Errorf("pending snapshot triggers: %w", err)
	}
	defer rows.Close()
	var foreign []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, ok := owned[name]; !ok {
			foreign = append(foreign, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(foreign) != 0 {
		sort.Strings(foreign)
		return fmt.Errorf("pending snapshot requests has %d unowned trigger(s) (%s); only the analytical projection triggers are permitted",
			len(foreign), strings.Join(foreign, ", "))
	}
	return nil
}

// canonicalizePendingAttempts turns the legacy empty attempts spelling into the
// writer's canonical no-value document in a staged replacement. The empty
// spelling is admission-compatible for scans, but HasError's json_each over the
// raw column would fail on it. A missing legacy attempts column needs no work.
func canonicalizePendingAttempts(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumnSet(ctx, db, "", "requests")
	if err != nil {
		return err
	}
	if _, ok := columns["attempts"]; !ok {
		return nil
	}
	if _, err := db.ExecContext(ctx, `UPDATE requests SET attempts = 'null' WHERE attempts = ''`); err != nil {
		return fmt.Errorf("canonicalize pending attempts: %w", err)
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
	// RowsAffected alone proves only that SQLite executed the INSERT. Read the
	// sentinel back inside the same transaction so a trigger that deletes or
	// rewrites the inserted row fails admission here, where the writer would
	// otherwise account a durable row that is not in history.
	var gotID string
	var gotStatus int
	if err := tx.QueryRow(`SELECT id, status_code FROM requests WHERE id = ?`, probeID).Scan(&gotID, &gotStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("writer insert probe row is missing after insert")
		}
		return err
	}
	if gotID != probeID || gotStatus != probe.StatusCode {
		return fmt.Errorf("writer insert probe row changed after insert (id %q status %d)", gotID, gotStatus)
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

// SnapshotOverlap is how many request ids in a packed snapshot already exist
// in the live store. Used by restore inspect. It is the byte-buffered wrapper
// over SnapshotOverlapFile for callers that hold the members in heap.
func (s *Store) SnapshotOverlap(ctx context.Context, data []byte) (int64, error) {
	if s == nil {
		return 0, ErrStorageDisabled
	}
	dir, err := os.MkdirTemp(s.stageDir(), overlapStagingPattern)
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

// attachSnapshotDatabase attaches one packed snapshot under a code-owned alias
// as a read-only schema. The URI comes from backup.SQLiteURI so a path
// containing ?, #, %, spaces or non-ASCII bytes stays one literal filename
// (ATTACH parses URI filenames, and a bare path would split at those bytes).
// mode=ro makes every write through the attached schema fail; the main schema
// on this connection stays writable, which is what a merge needs.
func attachSnapshotDatabase(ctx context.Context, conn *sql.Conn, alias, path string) error {
	if alias != "backup_inspect" && alias != "backup_merge" {
		return fmt.Errorf("unknown backup schema")
	}
	if path == "" {
		return fmt.Errorf("snapshot path is required")
	}
	params := url.Values{"mode": {"ro"}}
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS "+alias, backup.SQLiteURI(path, params)); err != nil {
		return err
	}
	return nil
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
	if err := attachSnapshotDatabase(ctx, conn, "backup_inspect", path); err != nil {
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
	dir, err := os.MkdirTemp(s.stageDir(), mergeStagingPattern)
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
	if err := attachSnapshotDatabase(ctx, conn, "backup_merge", path); err != nil {
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
	if err := preflightRequestScanValues(ctx, db, "backup_merge", "backup"); err != nil {
		return err
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

// recordScanValueKind is the SQLite storage-class family one recordScanTargets
// destination accepts. It is derived from the destination Go type, never from
// a second hand-maintained column list.
type recordScanValueKind int

const (
	recordScanText recordScanValueKind = iota
	recordScanInteger
	recordScanReal
	recordScanBool
)

// recordScanValueKindOf maps one scan destination pointer to the value class
// its Go type accepts, plus the type's bit width for range checks. An unknown
// destination fails closed: a new scan type must teach the preflight its
// accepted representation instead of silently escaping validation.
func recordScanValueKindOf(target any) (recordScanValueKind, int, error) {
	t := reflect.TypeOf(target)
	if t == nil || t.Kind() != reflect.Pointer {
		return 0, 0, fmt.Errorf("scan destination %T is not a pointer", target)
	}
	switch t.Elem().Kind() {
	case reflect.String:
		return recordScanText, 0, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return recordScanInteger, t.Elem().Bits(), nil
	case reflect.Float32, reflect.Float64:
		return recordScanReal, t.Elem().Bits(), nil
	case reflect.Bool:
		return recordScanBool, 0, nil
	default:
		return 0, 0, fmt.Errorf("unsupported scan destination type %s", t.Elem())
	}
}

// recordScanCheck is one scan-critical column's rejection predicate and the
// operator-facing description of the values scanRecord accepts.
type recordScanCheck struct {
	column   string
	reject   string
	accepted string
}

// recordScanMillisMin and recordScanMillisMax bound every stored millisecond
// timestamp that scanRecord turns into a time.Time. time.Time.MarshalJSON
// rejects years outside [0,9999], so a value outside this range is accepted by
// SQLite and every later dashboard encode fails. The bounds are the first and
// last millisecond of that representable window.
const (
	recordScanMillisMin = int64(-62167219200000) // 0000-01-01T00:00:00Z
	recordScanMillisMax = int64(253402300799999) // 9999-12-31T23:59:59.999Z
	// recordScanDurationMaxMs is the largest duration_ms whose conversion to
	// time.Duration in scanRecord cannot overflow: duration_ms * time.Millisecond
	// must fit in int64 nanoseconds.
	recordScanDurationMaxMs = int64(math.MaxInt64) / int64(time.Millisecond)
)

// recordScanMillisColumns are the columns scanRecord converts through
// time.UnixMilli. Every field serialized as time.Time must be listed here.
var recordScanMillisColumns = []string{
	"started_at", "first_token_at", "last_token_at", "first_answer_at", "final_attempt_at",
}

// aggregateMetricColumns are the REAL columns whose values feed the checked
// aggregate domain (metrics.SumValues): the KPI cost total and the throughput
// sums. A finite but negative, or non-finite, value here poisons every
// aggregate and JSON encode, so restore admission applies the same domain the
// writer's accounting applies.
var aggregateMetricColumns = []string{"cost", "gen_tps", "overall_tps"}

// aggregateCountColumns are the INTEGER request fields Totals.Add folds with
// metrics.SumCounts. A negative row would make the checked fold fail, and a
// column sum above int64 promotes to a SQLite REAL total that cannot be
// represented in Totals.
var aggregateCountColumns = []string{
	"input_tokens", "output_tokens", "cache_read_tokens", "reasoning_tokens", "ttft_ms",
}

// recordScanChecks derives one value check per recordColumns entry from the
// scan destinations scanRecord itself uses. Every numeric destination accepts
// the schema's own storage classes only: INTEGER for an integer or bool
// destination, INTEGER or REAL for a real destination. Text and blob coercions
// are deliberately rejected even where database/sql would parse them, because
// the writer never produces them and accepting them would hide a poisoned
// snapshot. A text destination accepts the TEXT storage class only: SQLite
// permits a BLOB in a TEXT-affinity column, and the log cursor binds exact
// filter keys as TEXT, so a BLOB id scans as a Go string but can never be
// materialized by LogPageRecords.
func recordScanChecks() ([]recordScanCheck, error) {
	columns := recordScanColumns()
	var record metrics.Record
	var scratch recordScanScratch
	targets := recordScanTargets(&record, &scratch)
	if len(targets) != len(columns) {
		return nil, fmt.Errorf("scan destination count %d does not match record column count %d", len(targets), len(columns))
	}
	checks := make([]recordScanCheck, len(columns))
	for i, column := range columns {
		kind, bits, err := recordScanValueKindOf(targets[i])
		if err != nil {
			return nil, fmt.Errorf("record column %s: %w", column, err)
		}
		quoted := `"` + column + `"`
		check := recordScanCheck{column: column, reject: quoted + ` IS NULL`}
		switch kind {
		case recordScanText:
			check.accepted = "a text value"
			check.reject += ` OR typeof(` + quoted + `) <> 'text'`
		case recordScanInteger:
			check.accepted = "an integer"
			check.reject += ` OR typeof(` + quoted + `) <> 'integer'`
			if bits < 64 {
				minValue := -(int64(1) << (bits - 1))
				maxValue := int64(1)<<(bits-1) - 1
				check.reject += fmt.Sprintf(` OR %s < %d OR %s > %d`, quoted, minValue, quoted, maxValue)
			}
			if slices.Contains(recordScanMillisColumns, column) {
				check.accepted = "an integer millisecond timestamp within the RFC 3339 range"
				check.reject += fmt.Sprintf(` OR %s < %d OR %s > %d`,
					quoted, recordScanMillisMin, quoted, recordScanMillisMax)
			}
			if slices.Contains(aggregateCountColumns, column) {
				check.accepted = "a nonnegative integer"
				check.reject += ` OR ` + quoted + ` < 0`
			}
		case recordScanReal:
			check.accepted = "a number"
			check.reject += ` OR typeof(` + quoted + `) NOT IN ('integer', 'real')`
			if bits < 64 {
				// ParseFloat with the destination width rejects values beyond
				// the type's finite range; spell the bound exactly so the SQL
				// comparison matches the conversion.
				maxValue := strconv.FormatFloat(float64(math.MaxFloat32), 'g', -1, 64)
				check.reject += fmt.Sprintf(` OR %s > %s OR %s < -%s`, quoted, maxValue, quoted, maxValue)
			}
			if slices.Contains(aggregateMetricColumns, column) {
				// SQLite stores +Inf/-Inf as REAL and NaN as NULL. The generic
				// NULL and storage-class checks above already catch NaN; the
				// finite/nonnegative border below plus the MIN/MAX domain check
				// in preflightAggregateValues rejects the infinite and negative
				// values metrics.SumValues refuses.
				check.accepted = "a finite nonnegative number"
				maxValue := strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64)
				check.reject += fmt.Sprintf(` OR %s < 0 OR %s > %s OR %s < -%s`,
					quoted, quoted, maxValue, quoted, maxValue)
			}
		case recordScanBool:
			check.accepted = "an integer 0 or 1"
			check.reject += ` OR typeof(` + quoted + `) <> 'integer' OR ` + quoted + ` NOT IN (0, 1)`
		default:
			return nil, fmt.Errorf("record column %s: unsupported scan value kind", column)
		}
		checks[i] = check
	}
	return checks, nil
}

// preflightRequestScanValues rejects a request table whose rows contain a
// value scanRecord cannot scan into its destination: NULL, a non-numeric
// storage class in a numeric or bool column, an out-of-range integer, or a
// bool other than 0/1. id is the classic NULL case because
// SQLite permits NULL in a legacy TEXT PRIMARY KEY, but stored values matter
// just as much: stream='abc' or stream=2 passes integrity, schema and writer
// probes and then makes every history read fail after a merge commits or a
// replacement is installed. The check runs before a merge transaction starts
// or a replacement is installed, so a rejected source leaves live history
// untouched. Empty strings, empty JSON columns, 0/1 booleans, and ordinary
// integers and reals remain valid.
func preflightRequestScanValues(ctx context.Context, db mergeConn, schema, source string) error {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return fmt.Errorf("unknown backup schema")
	}
	table := "requests"
	if schema != "" {
		table = schema + ".requests"
	}
	details, err := tableColumnDetails(ctx, db, schema, "requests")
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(details))
	for _, col := range details {
		present[col.name] = struct{}{}
	}
	all, err := recordScanChecks()
	if err != nil {
		return err
	}
	checks := make([]recordScanCheck, 0, len(all))
	for _, check := range all {
		if _, ok := present[check.column]; ok {
			checks = append(checks, check)
		}
	}
	if len(checks) == 0 {
		return fmt.Errorf("%s requests has no scan-required columns", source)
	}
	conditions := make([]string, len(checks))
	for i, check := range checks {
		conditions[i] = check.reject
	}
	var invalid int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE `+strings.Join(conditions, " OR ")).Scan(&invalid); err != nil {
		return err
	}
	if invalid == 0 {
		if err := preflightRequestDuration(ctx, db, table, source, present); err != nil {
			return err
		}
		if err := preflightAggregateValues(ctx, db, table, source, present); err != nil {
			return err
		}
		if _, ok := present["attempts"]; ok {
			var malformed int64
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+
				` WHERE attempts IS NOT NULL AND attempts <> '' AND json_valid(attempts) = 0`).Scan(&malformed); err != nil {
				return fmt.Errorf("%s requests.attempts: %w", source, err)
			}
			if malformed != 0 {
				return fmt.Errorf("%s requests.attempts contains %d malformed JSON values", source, malformed)
			}
		}
		return nil
	}
	// Only a failing table pays the per-column scans; the error names the first
	// scan-critical column that is actually invalid so an operator can inspect
	// the offending snapshot directly.
	for _, check := range checks {
		var count int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE `+check.reject).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		var nulls int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE "`+check.column+`" IS NULL`).Scan(&nulls); err != nil {
			return err
		}
		if nulls != 0 {
			return fmt.Errorf("%s requests contains %d rows with a NULL %s; history rows require non-NULL values for every scanRecord column", source, nulls, check.column)
		}
		return fmt.Errorf("%s requests contains %d rows with an invalid %s value; scanRecord requires %s", source, count, check.column, check.accepted)
	}
	return fmt.Errorf("%s requests contains values scanRecord cannot read", source)
}

// preflightAggregateValues applies the aggregate domain to the actual stored
// totals of every aggregate-bearing column. Per-row checks prove each value is
// representable; this check proves the full column sum the rebuild and writer
// accounting will produce is also representable. REAL totals go through
// metrics.SumValues, and INTEGER totals must remain a nonnegative int64 through
// metrics.SumCounts. SQLite returns NULL for an empty column, a REAL for an
// integer SUM that overflowed int64, and an integer otherwise.
func preflightAggregateValues(ctx context.Context, db mergeConn, table, source string, present map[string]struct{}) error {
	for _, column := range aggregateMetricColumns {
		if _, ok := present[column]; !ok {
			continue
		}
		var total sql.NullFloat64
		if err := db.QueryRowContext(ctx,
			`SELECT SUM("`+column+`") FROM `+table).Scan(&total); err != nil {
			return fmt.Errorf("%s requests.%s total: %w", source, column, err)
		}
		if !total.Valid {
			continue
		}
		if _, err := metrics.SumValues(total.Float64); err != nil {
			return fmt.Errorf("%s requests contains an invalid %s aggregate; totals require a finite nonnegative sum", source, column)
		}
	}
	for _, column := range aggregateCountColumns {
		if _, ok := present[column]; !ok {
			continue
		}
		var total any
		var kind string
		if err := db.QueryRowContext(ctx,
			`SELECT SUM("`+column+`"), typeof(SUM("`+column+`")) FROM `+table).Scan(&total, &kind); err != nil {
			// This SQLite build can report integer overflow from SUM itself
			// instead of promoting the result to REAL. Both mean the checked
			// int64 total is unrepresentable, so report the same domain error.
			if strings.Contains(err.Error(), "integer overflow") {
				return fmt.Errorf("%s requests contains an invalid %s aggregate; totals require a nonnegative int64 sum", source, column)
			}
			return fmt.Errorf("%s requests.%s total: %w", source, column, err)
		}
		if total == nil {
			continue
		}
		value, ok := total.(int64)
		if !ok || kind != "integer" {
			return fmt.Errorf("%s requests contains an invalid %s aggregate; totals require a nonnegative int64 sum", source, column)
		}
		if _, err := metrics.SumCounts(value); err != nil {
			return fmt.Errorf("%s requests contains an invalid %s aggregate; totals require a nonnegative int64 sum", source, column)
		}
	}
	return nil
}

// preflightRequestDuration applies the derived-End domain shared by replace and
// merge. scanRecord multiplies duration_ms by time.Millisecond and adds it to
// started_at, so a negative or too-large duration either wraps the Duration or
// moves End outside the RFC 3339 window even when the multiplication is safe.
// The subtraction form keeps started_at + duration_ms from overflowing SQLite
// integers; the standalone bounds already reject values that would make the
// subtraction itself unsafe.
func preflightRequestDuration(ctx context.Context, db mergeConn, table, source string, present map[string]struct{}) error {
	if _, ok := present["duration_ms"]; !ok {
		return nil
	}
	maxDuration := strconv.FormatInt(recordScanDurationMaxMs, 10)
	invalid := `typeof("duration_ms") <> 'integer' OR "duration_ms" < 0 OR "duration_ms" > ` + maxDuration
	if _, ok := present["started_at"]; ok {
		invalid += ` OR "started_at" < ` + strconv.FormatInt(recordScanMillisMin, 10) + ` - "duration_ms"` +
			` OR "started_at" > ` + strconv.FormatInt(recordScanMillisMax, 10) + ` - "duration_ms"`
	}
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+invalid).Scan(&count); err != nil {
		return fmt.Errorf("%s requests.duration_ms: %w", source, err)
	}
	if count != 0 {
		return fmt.Errorf("%s requests contains %d rows with an invalid duration_ms value; duration must be nonnegative, representable as time.Duration, and keep End within the RFC 3339 range", source, count)
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
	if table == "requests" {
		// A nullable live column cannot guarantee a non-NULL value when the
		// incoming table omits it: its default may itself be NULL or an
		// expression that yields one, and the intersection INSERT would then
		// write NULL into every merged row and poison scanRecord. Reject that
		// shape before the transaction. A required live column with a default
		// still admits a pre-column backup: SQLite's NOT NULL constraint
		// guarantees the omitted value cannot be NULL.
		for _, name := range recordScanColumns() {
			liveCol, ok := liveSet[name]
			if !ok {
				return fmt.Errorf("live requests is missing scan-required column %q", name)
			}
			if _, ok := incomingSet[name]; ok {
				continue
			}
			if !liveCol.notNull {
				return fmt.Errorf("backup requests is missing scan-required column %q; the live column is nullable and omission would insert NULL", name)
			}
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
