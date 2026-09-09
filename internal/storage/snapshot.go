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

// stageDir is where database-sized transient files are staged: the live
// database's own volume, which is contractually sized to hold the database
// and a packed copy of it. A generic /tmp is a small tmpfs in hardened
// deployments and must never cap the backup contract (backup_max_bytes).
func (s *Store) stageDir() string {
	return filepath.Dir(s.path)
}

// Snapshot returns a packed, integrity-checked copy of the live database,
// including committed WAL pages. VACUUM INTO is the compact consistent
// snapshot (no free pages); it is not a file copy of an open DB.
func (s *Store) Snapshot(ctx context.Context) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("storage is disabled")
	}
	if err := s.Flush(); err != nil {
		return nil, fmt.Errorf("flush before backup: %w", err)
	}
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-snapshot-*")
	if err != nil {
		return nil, fmt.Errorf("stage snapshot in %s: %w", s.stageDir(), err)
	}
	defer os.RemoveAll(dir)
	dest := filepath.Join(dir, "snapshot.db")
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return nil, fmt.Errorf("vacuum snapshot: %w", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		return nil, err
	}
	if err := backup.CheckDatabase(s.stageDir(), data); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	return data, nil
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

func admitPendingSnapshot(path string) error {
	incoming := path + pendingSnapshotSuffix
	data, err := os.ReadFile(incoming)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := backup.CheckDatabase(filepath.Dir(path), data); err != nil {
		if !errors.Is(err, backup.ErrInvalidSnapshot) {
			return fmt.Errorf("pending snapshot: %w", err)
		}
		_ = os.Rename(incoming, incoming+".invalid")
		log.Printf("storage: pending snapshot rejected: %v", err)
		return nil
	}
	if err := removeIfExists(path + "-wal"); err != nil {
		return fmt.Errorf("pending snapshot: %w", err)
	}
	if err := removeIfExists(path + "-shm"); err != nil {
		return fmt.Errorf("pending snapshot: %w", err)
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
		return 0, fmt.Errorf("storage is disabled")
	}
	if err := backup.CheckDatabase(s.stageDir(), data); err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-overlap-*")
	if err != nil {
		return 0, fmt.Errorf("stage snapshot in %s: %w", s.stageDir(), err)
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
		return 0, 0, fmt.Errorf("storage is disabled")
	}
	info, err := backup.InspectDatabase(s.stageDir(), data)
	if err != nil {
		return 0, 0, err
	}
	if err := s.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush before merge: %w", err)
	}
	dir, err := os.MkdirTemp(s.stageDir(), "millivolt-merge-*")
	if err != nil {
		return 0, 0, fmt.Errorf("stage snapshot in %s: %w", s.stageDir(), err)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return 0, 0, err
	}
	// Hold the single writer connection for ATTACH through installTotals so
	// insertBatch cannot account() a concurrent commit against pre-merge
	// aggregates the way purge installs totals on the writer goroutine.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS backup_merge", src); err != nil {
		return 0, 0, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "DETACH DATABASE backup_merge") }()
	n, err := mergeAttachedTable(ctx, conn, "requests")
	if err != nil {
		return 0, 0, err
	}
	inserted = n
	skipped = int64(info.Requests) - inserted
	if skipped < 0 {
		skipped = 0
	}
	var debugErr error
	if _, err := mergeAttachedTable(ctx, conn, "request_debug"); err != nil {
		debugErr = err
	}
	tctx, cancel := s.withQueryTimeout(context.Background())
	defer cancel()
	if err := s.computeTotals(tctx); err != nil {
		if debugErr != nil {
			return inserted, skipped, fmt.Errorf("%w; totals after backup merge: %v", debugErr, err)
		}
		return inserted, skipped, fmt.Errorf("totals after backup merge: %w", err)
	}
	if debugErr != nil {
		return inserted, skipped, debugErr
	}
	return inserted, skipped, nil
}

type mergeConn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func mergeAttachedTable(ctx context.Context, db mergeConn, table string) (int64, error) {
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
	q := "INSERT OR IGNORE INTO " + table + " (" + list + ") SELECT " + list + " FROM backup_merge." + table
	res, err := db.ExecContext(ctx, q)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func pragmaTableInfo(ctx context.Context, db mergeConn, schema, table string) (map[string]struct{}, error) {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return nil, fmt.Errorf("unknown backup schema")
	}
	if table != "requests" && table != "request_debug" {
		return nil, fmt.Errorf("unknown merge table")
	}
	q := "PRAGMA table_info(" + table + ")"
	if schema != "" {
		q = "PRAGMA " + schema + ".table_info(" + table + ")"
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
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
