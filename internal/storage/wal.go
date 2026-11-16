package storage

import (
	"context"

	"github.com/LLM-4-People/millivolt/internal/logging"
)

// checkpointWAL folds the committed database pages into the main file and
// removes the write-ahead log. secure_delete zeroes freed cells in the pages
// SQLite writes, but a deletion commit leaves those pages in WAL frames; the
// frames written before the delete still carry the old page images. A purge
// or an expired-capture sweep therefore has to checkpoint, or deleted content
// survives in the `-wal` sidecar for as long as the log lives.
//
// TRUNCATE is the strongest mode: it checkpoints every frame and shortens the
// WAL to zero bytes. A reader holding an older snapshot can still make the
// checkpoint busy; the deletion has committed regardless, so a busy or failed
// attempt only raises the pending flag for the writer's near-term tick and the
// final shutdown pass. The bounded storage query context keeps a stalled
// checkpoint from pinning the writer indefinitely.
func (s *Store) checkpointWAL() {
	if s == nil || s.db == nil {
		return
	}
	s.walCheckpointPending.Store(true)
	s.runWALCheckpoint(true)
}

// continueWALCheckpoint retries a deferred checkpoint. The writer's near-term
// tick calls it so a reader that pinned the log cannot keep deleted content
// recoverable until the next destructive pass, long after the read ended.
func (s *Store) continueWALCheckpoint() {
	if s == nil || s.db == nil || !s.walCheckpointPending.Load() {
		return
	}
	s.runWALCheckpoint(false)
}

// drainWALCheckpoint is the last attempt, run once at shutdown after the read
// pool is closed. Nothing retries afterwards, so the flag clears either way and
// one final failure keeps its log line.
func (s *Store) drainWALCheckpoint() {
	if s == nil || s.db == nil || !s.walCheckpointPending.Load() {
		return
	}
	s.runWALCheckpoint(true)
	s.walCheckpointPending.Store(false)
}

// runWALCheckpoint attempts one TRUNCATE checkpoint and reports whether the
// log is clear. logFailure distinguishes the first, operator-visible failure
// from silent retries: an unchanged hard error must not repeat every tick.
func (s *Store) runWALCheckpoint(logFailure bool) bool {
	ctx, cancel := s.withQueryTimeout(context.Background())
	defer cancel()
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		if logFailure && ctx.Err() == nil {
			logging.Printf("", "storage: checkpoint after delete: %v", err)
		}
		return false
	}
	if busy != 0 {
		if logFailure {
			logging.Printf("", "storage: checkpoint after delete deferred: %d of %d frames checkpointed", checkpointed, logFrames)
		}
		return false
	}
	s.walCheckpointPending.Store(false)
	return true
}
