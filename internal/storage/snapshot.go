package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

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
