package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

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
