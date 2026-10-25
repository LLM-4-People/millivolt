package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

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
