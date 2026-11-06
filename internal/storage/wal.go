package storage

import (
	"context"
	"log"
)

// checkpointWAL folds the committed database pages into the main file and
// removes the write-ahead log. secure_delete zeroes freed cells in the pages
// SQLite writes, but a deletion commit leaves those pages in WAL frames; the
// frames written before the delete still carry the old page images. A purge
// or an expired-capture sweep therefore has to checkpoint, or deleted content
// survives in the `-wal` sidecar until SQLite's next natural checkpoint.
//
// TRUNCATE is the strongest mode: it checkpoints every frame and shortens the
// WAL to zero bytes. A reader holding an older snapshot can leave work for the
// next destructive maintenance pass; that is logged, never turned into a
// failure of the deletion that already committed. The bounded storage query
// context keeps a stalled checkpoint from pinning the writer indefinitely.
func (s *Store) checkpointWAL() {
	if s == nil || s.db == nil {
		return
	}
	ctx, cancel := s.withQueryTimeout(context.Background())
	defer cancel()
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		if ctx.Err() == nil {
			log.Printf("storage: checkpoint after delete: %v", err)
		}
		return
	}
	if busy != 0 {
		log.Printf("storage: checkpoint after delete deferred: %d of %d frames checkpointed", checkpointed, logFrames)
	}
}
