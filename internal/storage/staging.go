package storage

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

const (
	// stagingSweepMinAge is how old a known staging entry must be before the
	// startup sweep may remove it. A younger entry may belong to an in-progress
	// backup, restore or snapshot in this process, so it always survives; a
	// crash orphan younger than this is reclaimed on a later boot. Internal
	// guardrail, not operator-tunable.
	stagingSweepMinAge = 24 * time.Hour

	// stagingSweepBatchEntries bounds one directory read. Reads repeat until
	// the directory is exhausted or the per-boot scan budget is spent, so an
	// aged orphan behind the first batch is still examined in the same sweep
	// instead of being stranded permanently. Internal guardrail, not
	// operator-tunable.
	stagingSweepBatchEntries = 256

	// stagingSweepMaxEntries bounds the total directory entries one startup
	// sweep examines, and therefore the removals it can perform. The database
	// volume normally holds a handful of entries; the budget keeps a
	// pathological directory from turning boot cleanup into an unbounded
	// scan. Internal guardrail, not operator-tunable.
	stagingSweepMaxEntries = 4096
)

// isStagingName reports whether base is one of the known transient names this
// service creates on the live database's volume. The registry is deliberately
// closed: unrelated operator files and the public backup download artifact
// never match. Live-derived paths are protected separately by path identity,
// so a matching basename is still safe. A SQLite sidecar of a staging name
// matches too, so a crash between creating a staging main and removing its WAL,
// SHM or rollback journal cannot orphan the sidecar.
func (s *Store) isStagingName(base string) bool {
	if s.isBaseStagingName(base) {
		return true
	}
	for _, suffix := range snapshotSidecarSuffixes {
		if strings.HasSuffix(base, suffix) && s.isBaseStagingName(strings.TrimSuffix(base, suffix)) {
			return true
		}
	}
	return false
}

// isBaseStagingName matches the main-file/directory shapes of the closed
// staging registry. It intentionally does not know about the live path; the
// sweep applies the path-derived protection fence before consulting it.
func (s *Store) isBaseStagingName(base string) bool {
	if backup.IsStagingName(base) {
		return true
	}
	for _, pattern := range []string{
		snapshotStagingPattern,
		overlapStagingPattern,
		mergeStagingPattern,
		snapshotIncomingTempPattern(s.path),
		snapshotStateTempPattern(s.path),
	} {
		if backup.MatchesStagingPattern(pattern, base) {
			return true
		}
	}
	return false
}

// protectedLivePaths is the absolute safety fence for the startup sweep: the
// live database and every file the pending-admission protocol derives from it
// are excluded by path, regardless of whether their names happen to match a
// staging pattern. This covers a db_path whose basename itself looks like
// millivolt-snapshot-* or millivolt-backup-*, including its SQLite sidecars,
// the pending .incoming slot, the .previous rollback set, the durable state
// marker and quarantined admission evidence.
//
// The configured path is not always the live file's identity: db_path may be a
// symlink whose resolved target has a staging-shaped basename. Both identities
// are protected, and the admission-derived names are built from each resolved
// directory/name, so a sweep can never unlink the file SQLite actually opened.
// An unresolvable existing path fails the sweep closed instead of guessing.
func (s *Store) protectedLivePaths() (map[string]struct{}, error) {
	bases := []string{s.path}
	resolved, err := filepath.EvalSymlinks(s.path)
	if err == nil {
		if resolved != "" {
			bases = append(bases, resolved)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("resolve live database path %q: %w", s.path, err)
	}
	derived := make([]string, 0, len(bases)*6)
	for _, base := range bases {
		derived = append(derived,
			base,
			PendingSnapshotPath(base),
			base+pendingSnapshotPreviousSuffix,
			base+pendingSnapshotStateSuffix,
			PendingSnapshotPath(base)+pendingSnapshotInvalidSuffix,
			PendingSnapshotPath(base)+pendingSnapshotInterruptedSuffix,
		)
	}
	protected := make(map[string]struct{}, len(derived)*(len(snapshotSidecarSuffixes)+1))
	for _, base := range derived {
		protected[protectedPathKey(base)] = struct{}{}
		for _, suffix := range snapshotSidecarSuffixes {
			protected[protectedPathKey(base+suffix)] = struct{}{}
		}
	}
	// Quarantine places every sidecar beside the quarantined main, so the
	// protected set above covers the current spelling (incoming.invalid-wal).
	// Keep the earlier incoming-wal.invalid order protected too: an upgrade
	// must not sweep evidence written by a release that predates the rename.
	for _, base := range bases {
		incoming := PendingSnapshotPath(base)
		for _, quarantine := range []string{pendingSnapshotInvalidSuffix, pendingSnapshotInterruptedSuffix} {
			for _, sidecar := range snapshotSidecarSuffixes {
				protected[protectedPathKey(incoming+sidecar+quarantine)] = struct{}{}
			}
		}
	}
	return protected, nil
}

// protectedPathKey normalizes a path for identity comparison. Absolute keys
// keep a resolved symlink target comparable with directory entries even when
// db_path itself is relative and the link target is absolute.
func protectedPathKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// isProtectedLivePath reports whether path is the configured live spelling or
// resolves to the protected identity. The second check covers a database
// directory reached through a symlink: the sweep lists lexical entry names,
// while protectedLivePaths holds the real target path.
func (s *Store) isProtectedLivePath(protected map[string]struct{}, path string) bool {
	if _, ok := protected[protectedPathKey(path)]; ok {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	_, ok := protected[protectedPathKey(resolved)]
	return ok
}

// sweepOrphanedStaging reclaims known snapshot/archive staging entries left by
// a crash. SnapshotFile, overlap, merge, archive encode/decode and pending
// publish normally remove their own staging paths; only a process that dies
// between creation and defer leaves them behind. The sweep runs at startup,
// before any handler can create a new staging entry, and is bounded in both
// age and directory entries; the directory is read in bounded batches up to
// that total budget. Removal failures are logged and never fail boot.
func (s *Store) sweepOrphanedStaging(now time.Time) int {
	dir := s.stageDir()
	d, err := os.Open(dir)
	if err != nil {
		log.Printf("storage: inspect staging directory %s: %v", dir, err)
		return 0
	}
	defer d.Close()
	removed := 0
	protected, err := s.protectedLivePaths()
	if err != nil {
		// A live database whose identity cannot be resolved must never be
		// guessed at: skip the destructive sweep and leave boot working.
		log.Printf("storage: staging sweep skipped: %v", err)
		return 0
	}
	scanned := 0
	for scanned < stagingSweepMaxEntries {
		batch := min(stagingSweepBatchEntries, stagingSweepMaxEntries-scanned)
		entries, readErr := d.ReadDir(batch)
		if len(entries) == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				log.Printf("storage: read staging directory %s: %v", dir, readErr)
			}
			break
		}
		scanned += len(entries)
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(dir, name)
			// Path identity wins over name shape. A db_path named
			// millivolt-snapshot-live.db must survive even though its basename
			// matches snapshotStagingPattern, and so must a symlink target with
			// that shape. Resolving the entry also matches a lexical directory
			// alias (a symlinked db_path parent) to the resolved live identity.
			if s.isProtectedLivePath(protected, path) {
				continue
			}
			if !s.isStagingName(name) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				if !os.IsNotExist(err) {
					log.Printf("storage: inspect staging entry %s: %v", path, err)
				}
				continue
			}
			if now.Sub(info.ModTime()) < stagingSweepMinAge {
				// Recent enough to be an in-progress writer; never guess.
				continue
			}
			if info.IsDir() {
				err = os.RemoveAll(path)
			} else {
				err = os.Remove(path)
			}
			if err != nil && !os.IsNotExist(err) {
				log.Printf("storage: remove orphaned staging entry %s: %v", path, err)
				continue
			}
			removed++
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				log.Printf("storage: read staging directory %s: %v", dir, readErr)
			}
			break
		}
	}
	if scanned >= stagingSweepMaxEntries {
		log.Printf("storage: staging sweep reached its %d-entry budget in %s", stagingSweepMaxEntries, dir)
	}
	return removed
}
