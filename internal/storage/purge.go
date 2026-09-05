package storage

import (
	"context"
	"errors"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type queuedRecord struct {
	record *metrics.Record
	seq    uint64
}

// Recorder couples the live ring and durable enqueue at their shared, short
// publication fence. It does not wait for storage: channel overflow still drops
// only the durable copy. Lifecycle events remain exclusively in the live buffer.
func (s *Store) Recorder(buffer *metrics.Buffer) metrics.Recorder {
	return liveRecorder{s, buffer}
}

type liveRecorder struct {
	store  *Store
	buffer *metrics.Buffer
}

func (r liveRecorder) Record(record *metrics.Record) { r.store.record(record, r.buffer) }
func (r liveRecorder) PublishLive(phase string, record *metrics.Record) {
	r.buffer.PublishLive(phase, record)
}

type PurgeResult struct {
	Deleted       int64 `json:"deleted"`
	BufferRemoved int   `json:"buffer_removed"`
}

type purgeOutcome struct {
	result PurgeResult
	err    error
}

type purgeCommand struct {
	ctx      context.Context
	filter   *PurgeFilter
	accepted uint64
	buffer   *metrics.Buffer
	boundary metrics.RemovalBoundary
	result   chan purgeOutcome
}

// Clear deletes finalized history through a single ring/enqueue fence. A nil
// filter explicitly selects all history; a supplied empty filter is rejected.
// Admin callers serialize outside sendMu. The only work under the producer lock
// is capturing the in-memory boundary and publishing one command; SQL and waits
// are exclusively on the writer/admin goroutines.
func (s *Store) Clear(ctx context.Context, filter *PurgeFilter, buffer *metrics.Buffer) (PurgeResult, error) {
	if filter != nil {
		copy := *filter
		filter = &copy
		if err := filter.Validate(); err != nil {
			return PurgeResult{}, err
		}
		if filter.IsEmpty() {
			return PurgeResult{}, errors.New("empty purge filter (send no body to delete everything)")
		}
	}
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return PurgeResult{}, err
	}
	cmd := &purgeCommand{ctx: ctx, filter: filter, buffer: buffer, result: make(chan purgeOutcome, 1)}
	s.sendMu.Lock()
	if s.shut {
		s.sendMu.Unlock()
		return PurgeResult{}, errors.New("storage: closed")
	}
	cmd.accepted = s.accepted
	if buffer != nil {
		cmd.boundary = buffer.RemovalBoundary()
	}
	s.pendingPurge.Store(cmd)
	select {
	case s.purgeWake <- struct{}{}:
	default:
	}
	s.sendMu.Unlock()
	// Once published, wait for the actual transaction outcome. Cancellation is
	// passed into SQL; it must not turn a committed delete into a reported failure
	// before the matching ring removal and aggregate publication finish.
	out := <-cmd.result
	return out.result, out.err
}

func (s *Store) purgeTransaction(cmd *purgeCommand) (PurgeResult, error) {
	tx, err := s.db.BeginTx(cmd.ctx, nil)
	if err != nil {
		return PurgeResult{}, err
	}
	defer tx.Rollback()
	where := ""
	var args []any
	if cmd.filter != nil {
		where, args = cmd.filter.build()
		if where == "" {
			return PurgeResult{}, errors.New("purge filter produced no constraint")
		}
		where = " WHERE " + where
	}
	// Captures for requests still finalizing have no durable row yet and must
	// survive. Orphan/expired capture cleanup remains owned by the TTL path.
	if _, err = tx.ExecContext(cmd.ctx, "DELETE FROM request_debug WHERE id IN (SELECT id FROM requests"+where+")", args...); err != nil {
		return PurgeResult{}, err
	}
	res, err := tx.ExecContext(cmd.ctx, "DELETE FROM requests"+where, args...)
	if err != nil {
		return PurgeResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PurgeResult{}, err
	}
	totals, err := readTotals(cmd.ctx, tx)
	if err != nil {
		return PurgeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PurgeResult{}, err
	}
	// Do not consult ctx after commit: these in-memory effects are mandatory.
	s.installTotals(totals)
	result := PurgeResult{Deleted: n}
	if cmd.buffer != nil {
		var match func(*metrics.Record) bool
		if cmd.filter != nil {
			match = cmd.filter.MatchRecord
		}
		result.BufferRemoved = cmd.buffer.RemoveThrough(cmd.boundary, match)
	}
	return result, nil
}
