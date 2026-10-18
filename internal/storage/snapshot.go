package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// admit in place of path. The live database is not touched.
func StageSnapshot(path string, data []byte) error {
	if path == "" {
		return fmt.Errorf("database path is empty")
	}
	if err := backup.CheckDatabase(filepath.Dir(path), data); err != nil {
		return err
	}
	incoming := path + pendingSnapshotSuffix
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-incoming-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), incoming)
}

// admitPendingSnapshot installs a staged replacement database before Open.
// Its errors are returned bare: Open owns the single "pending snapshot:"
// label (every cause, including the final rename, carries exactly one
// prefix from that one wrap).
func admitPendingSnapshot(path string) error {
	incoming := path + pendingSnapshotSuffix
	if err := backup.CheckDatabaseFile(incoming); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		if !errors.Is(err, backup.ErrInvalidSnapshot) {
			return err
		}
		_ = os.Rename(incoming, incoming+".invalid")
		log.Printf("storage: pending snapshot rejected: %v", err)
		return nil
	}
	if err := removeIfExists(path + "-wal"); err != nil {
		return err
	}
	if err := removeIfExists(path + "-shm"); err != nil {
		return err
	}
	return os.Rename(incoming, path)
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// SnapshotOverlap is how many request ids in a packed snapshot already exist
// in the live store. Used by restore inspect.
func (s *Store) SnapshotOverlap(ctx context.Context, data []byte) (int64, error) {
	if s == nil {
		return 0, ErrStorageDisabled
	}
	if err := backup.CheckDatabase(s.stageDir(), data); err != nil {
		return 0, err
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
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	conn, err := s.rdb.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS backup_inspect", src); err != nil {
		return 0, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "DETACH DATABASE backup_inspect") }()
	var n int64
	err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_inspect.requests WHERE id IN (SELECT id FROM requests)`).Scan(&n)
	return n, err
}

// MergeSnapshot inserts snapshot rows whose ids are not already in the live
// store (INSERT OR IGNORE). Existing live rows win. The live file is not
// replaced; no restart is required.
func (s *Store) MergeSnapshot(ctx context.Context, data []byte) (inserted, skipped int64, err error) {
	if s == nil {
		return 0, 0, ErrStorageDisabled
	}
	// Admin mutations serialize: purge holds the same mutex, so a merge can
	// never queue behind (or race) a delete and starve the writer twice.
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	info, err := backup.InspectDatabase(s.stageDir(), data)
	if err != nil {
		return 0, 0, err
	}
	if err := s.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush before merge: %w", err)
	}
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-merge-*")
	if err != nil {
		return 0, 0, backup.StageSnapshotError(s.stageDir(), err)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return 0, 0, err
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
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS backup_merge", src); err != nil {
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
		if err := saveTotals(tctx, tx, totals, false, epoch); err != nil {
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
