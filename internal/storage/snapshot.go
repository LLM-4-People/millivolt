package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

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
		_ = os.Rename(incoming, incoming+".invalid")
		log.Printf("storage: pending snapshot rejected: %v", err)
		return nil
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	return os.Rename(incoming, path)
}
