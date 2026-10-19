package storage

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
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
// closed: the live database, PendingSnapshotPath, the .previous rollback set,
// the admission state marker and unrelated operator files never match.
func (s *Store) isStagingName(base string) bool {
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
	for _, entry := range entries {
		name := entry.Name()
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
