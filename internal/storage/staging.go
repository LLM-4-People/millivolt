package storage

import (
	"errors"
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

	// stagingSweepMaxEntries bounds one startup directory read. The database
	// volume normally holds a handful of entries; the bound keeps a pathological
	// directory from turning boot cleanup into an unbounded scan. Internal
	// guardrail, not operator-tunable.
	stagingSweepMaxEntries = 256
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
func (s *Store) protectedLivePaths() map[string]struct{} {
	bases := []string{
		s.path,
		PendingSnapshotPath(s.path),
		s.path + pendingSnapshotPreviousSuffix,
		s.path + pendingSnapshotStateSuffix,
		PendingSnapshotPath(s.path) + pendingSnapshotInvalidSuffix,
		PendingSnapshotPath(s.path) + pendingSnapshotInterruptedSuffix,
	}
	protected := make(map[string]struct{}, len(bases)*(len(snapshotSidecarSuffixes)+1))
	for _, base := range bases {
		protected[filepath.Clean(base)] = struct{}{}
		for _, suffix := range snapshotSidecarSuffixes {
			protected[filepath.Clean(base+suffix)] = struct{}{}
		}
	}
	return protected
}

// sweepOrphanedStaging reclaims known snapshot/archive staging entries left by
// a crash. SnapshotFile, overlap, merge, archive encode/decode and pending
// publish normally remove their own staging paths; only a process that dies
// between creation and defer leaves them behind. The sweep runs at startup,
// before any handler can create a new staging entry, and is bounded in both
// age and directory entries. Removal failures are logged and never fail boot.
func (s *Store) sweepOrphanedStaging(now time.Time) int {
	dir := s.stageDir()
	d, err := os.Open(dir)
	if err != nil {
		log.Printf("storage: inspect staging directory %s: %v", dir, err)
		return 0
	}
	defer d.Close()
	entries, err := d.ReadDir(stagingSweepMaxEntries)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Printf("storage: read staging directory %s: %v", dir, err)
		return 0
	}
	removed := 0
	protected := s.protectedLivePaths()
	for _, entry := range entries {
		name := entry.Name()
		// Path identity wins over name shape. A db_path named
		// millivolt-snapshot-live.db must survive even though its basename
		// matches snapshotStagingPattern.
		if _, ok := protected[filepath.Clean(filepath.Join(dir, name))]; ok {
			continue
		}
		if !s.isStagingName(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("storage: inspect staging entry %s: %v", filepath.Join(dir, name), err)
			}
			continue
		}
		if now.Sub(info.ModTime()) < stagingSweepMinAge {
			// Recent enough to be an in-progress writer; never guess.
			continue
		}
		path := filepath.Join(dir, name)
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
	return removed
}
