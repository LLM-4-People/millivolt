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
	dir, err := os.MkdirTemp("", "millivolt-snapshot-*")
	if err != nil {
		return nil, err
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
	if err := backup.CheckDatabase(data); err != nil {
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
	if err := backup.CheckDatabase(data); err != nil {
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
	if err := backup.CheckDatabase(data); err != nil {
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
	if err := backup.CheckDatabase(data); err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp("", "millivolt-overlap-*")
	if err != nil {
		return 0, err
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
	defer conn.ExecContext(ctx, "DETACH DATABASE backup_inspect")
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
	info, err := backup.InspectDatabase(data)
	if err != nil {
		return 0, 0, err
	}
	if err := s.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush before merge: %w", err)
	}
	dir, err := os.MkdirTemp("", "millivolt-merge-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return 0, 0, err
	}
	if _, err := s.db.ExecContext(ctx, "ATTACH DATABASE ? AS backup_merge", src); err != nil {
		return 0, 0, err
	}
	defer s.db.ExecContext(ctx, "DETACH DATABASE backup_merge")
	n, err := mergeAttachedTable(ctx, s.db, "requests")
	if err != nil {
		return 0, 0, err
	}
	inserted = n
	if _, err := mergeAttachedTable(ctx, s.db, "request_debug"); err != nil {
		return inserted, 0, err
	}
	skipped = int64(info.Requests) - inserted
	if skipped < 0 {
		skipped = 0
	}
	tctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	if err := s.computeTotals(tctx); err != nil {
		log.Printf("storage: totals after backup merge: %v", err)
	}
	return inserted, skipped, nil
}

func mergeAttachedTable(ctx context.Context, db *sql.DB, table string) (int64, error) {
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
	live, err := columnSet(db, table)
	if err != nil {
		return 0, err
	}
	incoming, err := attachedColumnSet(db, "backup_merge", table)
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

func attachedColumnSet(db *sql.DB, schema, table string) (map[string]struct{}, error) {
	if schema != "backup_merge" && schema != "backup_inspect" {
		return nil, fmt.Errorf("unknown backup schema")
	}
	if table != "requests" && table != "request_debug" {
		return nil, fmt.Errorf("unknown merge table")
	}
	rows, err := db.Query("PRAGMA " + schema + ".table_info(" + table + ")")
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
