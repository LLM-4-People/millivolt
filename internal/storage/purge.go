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

// PurgeResult carries the store-level deletion counts Clear returns: durable
// rows deleted and live-ring records removed. Purely internal - the purge
// response dropped its wire keys ({"ok": true} only, pinned by the strict
// response decoder), and the fields are the store-level contract the storage
// tests assert against.
type PurgeResult struct {
	Deleted       int64
	BufferRemoved int
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
	// An administrative purge rewrites all (or all matching) history and its
	// totals in one transaction; it runs on the maintenance budget, never the
	// interactive dashboard query timeout. A shorter caller deadline wins.
	ctx, cancel := s.withMaintenanceTimeout(ctx)
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
	// passed into SQL; it must not turn a committed delete into a reported
	// failure before the matching ring removal and aggregate publication finish.
	// The command owns the purgeMu until it retires: returning on ctx.Done
	// would release the lock with the command still pending, letting a second
	// Clear overwrite it and silently lose one of the two deletions. The writer
	// owns the buffered result channel, so waiting can never block it.
	out := <-cmd.result
	return out.result, out.err
}

// purgePredicate returns the allowlisted WHERE clause shared by both purge
// deletes. A nil filter means "all rows" and returns an empty clause; a
// non-nil filter that produces no constraint is rejected instead of being
// widened to a full delete.
func purgePredicate(filter *PurgeFilter) (string, []any, error) {
	if filter == nil {
		return "", nil, nil
	}
	where, args := filter.build()
	if where == "" {
		return "", nil, errors.New("purge filter produced no constraint")
	}
	return " WHERE " + where, args, nil
}

// buildPurgeDebugDelete is the one owner of the sidecar delete statement. A
// full purge removes only captures whose request row exists: a capture can
// precede its request's durable row and must survive, so its DELETE uses an
// EXISTS guard. A filtered purge deletes captures through an IN subquery over
// the request rows matching the same filter.
func buildPurgeDebugDelete(filter *PurgeFilter) (string, []any, error) {
	if filter == nil {
		return `DELETE FROM request_debug WHERE id IN (
			SELECT d.id FROM request_debug AS d
			WHERE EXISTS (SELECT 1 FROM requests AS r WHERE r.id = d.id))`, nil, nil
	}
	where, args, err := purgePredicate(filter)
	if err != nil {
		return "", nil, err
	}
	return "DELETE FROM request_debug WHERE id IN (SELECT id FROM requests" + where + ")", args, nil
}

// buildPurgeRequestsDelete is the one owner of the request-row delete
// statement. buildPurgeDebugDelete must run first in the same transaction so
// matched sidecars cannot outlive their rows.
func buildPurgeRequestsDelete(filter *PurgeFilter) (string, []any, error) {
	where, args, err := purgePredicate(filter)
	if err != nil {
		return "", nil, err
	}
	return "DELETE FROM requests" + where, args, nil
}

func (s *Store) purgeTransaction(cmd *purgeCommand) (PurgeResult, error) {
	tx, err := s.db.BeginTx(cmd.ctx, nil)
	if err != nil {
		return PurgeResult{}, err
	}
	defer tx.Rollback()
	// Captures for requests still finalizing have no durable row yet and must
	// survive. Orphan/expired capture cleanup remains owned by the TTL path.
	// buildPurgeDebugDelete issues the one sidecar DELETE: a full purge keeps
	// the EXISTS guard so captures without a durable request row survive, and
	// a filtered purge deletes exactly the captures whose request row matches
	// the same predicate as the request delete. Clearing every capture is
	// wrong: a capture can precede its request's durable row.
	var debugDelta int64
	debugBefore := int64(0)
	if s.debugMaxBytes > 0 {
		debugBefore, err = debugByteTotal(cmd.ctx, tx)
		if err != nil {
			return PurgeResult{}, err
		}
	}
	debugDelete, debugArgs, err := buildPurgeDebugDelete(cmd.filter)
	if err != nil {
		return PurgeResult{}, err
	}
	if _, err = tx.ExecContext(cmd.ctx, debugDelete, debugArgs...); err != nil {
		return PurgeResult{}, err
	}
	// The net sidecar delta, not an absolute total, is what a purge may
	// publish: a capture save can land after this transaction commits, and an
	// absolute install would overwrite its increment. The delta is applied
	// under debugMu by commitDebugDelta, after the committed state is durable.
	if s.debugMaxBytes > 0 {
		debugAfter, err := debugByteTotal(cmd.ctx, tx)
		if err != nil {
			return PurgeResult{}, err
		}
		debugDelta = debugAfter - debugBefore
	}
	requestsDelete, requestsArgs, err := buildPurgeRequestsDelete(cmd.filter)
	if err != nil {
		return PurgeResult{}, err
	}
	res, err := tx.ExecContext(cmd.ctx, requestsDelete, requestsArgs...)
	if err != nil {
		return PurgeResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PurgeResult{}, err
	}
	// A zero-row purge changed no aggregate. A full purge leaves no rows, so
	// its aggregate is exactly zero without scanning; a filtered purge scans
	// the survivors once. Both write the document in this transaction.
	var totals Totals
	var totalsEpoch, totalsHighRowID int64
	hasTotals := n > 0
	if hasTotals {
		if cmd.filter != nil {
			totals, err = readTotals(cmd.ctx, tx)
			if err != nil {
				return PurgeResult{}, err
			}
		}
		totalsEpoch, err = projectionEpoch(cmd.ctx, tx)
		if err != nil {
			return PurgeResult{}, err
		}
		totalsHighRowID, err = projectionHighRowID(cmd.ctx, tx)
		if err != nil {
			return PurgeResult{}, err
		}
		if err := saveTotals(cmd.ctx, tx, totals, false, totalsEpoch, totalsHighRowID); err != nil {
			return PurgeResult{}, err
		}
	}
	if err := s.commitDebugDelta(tx, debugDelta); err != nil {
		return PurgeResult{}, err
	}
	// Do not consult ctx after commit: these in-memory effects are mandatory.
	if hasTotals {
		s.installTotals(totals, totalsEpoch, totalsHighRowID)
	}
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
