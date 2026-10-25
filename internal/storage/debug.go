package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// SaveDebugCapture writes one request's debug document (sidecar). Synchronous
// on the writer connection so a drawer fetch after the request finalizes
// cannot miss the row.
func (s *Store) SaveDebugCapture(ctx context.Context, id, sessionID string, capturedAt, expiresAt int64, payload []byte) error {
	if id == "" {
		return errors.New("debug capture id required")
	}
	if s.debugMaxBytes > 0 {
		// Reserve the sole writer connection BEFORE taking debugMu. Purge and
		// merge hold that connection while recomputing the counter, so this
		// order cannot deadlock; taking the lock first would.
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		s.debugMu.Lock()
		defer s.debugMu.Unlock()
		if !s.debugSeeded {
			var n int64
			if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug`).Scan(&n); err != nil {
				return err
			}
			s.debugBytes, s.debugSeeded = n, true
		}
		previous, err := s.debugCaptureBytes(ctx, conn, id)
		if err != nil {
			return err
		}
		next := s.debugBytes - previous + int64(len(payload))
		if next > s.debugMaxBytes {
			// Expired rows are invisible to reads but still counted until the
			// periodic sweep. Reclaim them now, on this held connection and
			// under debugMu, instead of refusing a capture the cap already
			// allows on disk. The row being replaced may itself have expired,
			// so re-read its committed length after the sweep.
			_, more, sweepErr := s.expireDebugCapturesLocked(ctx, conn, time.Now().UnixMilli(), debugExpireBatchesPerSweep)
			if more {
				s.debugExpirePending.Store(true)
			}
			if sweepErr != nil {
				return sweepErr
			}
			previous, err = s.debugCaptureBytes(ctx, conn, id)
			if err != nil {
				return err
			}
			next = s.debugBytes - previous + int64(len(payload))
			if next > s.debugMaxBytes {
				return fmt.Errorf("debug capture store limit reached (%d of %d bytes)", s.debugBytes, s.debugMaxBytes)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT OR REPLACE INTO request_debug(id, session_id, captured_at, expires_at, payload) VALUES (?, ?, ?, ?, ?)`,
			id, sessionID, capturedAt, expiresAt, string(payload)); err != nil {
			return err
		}
		s.debugBytes = next
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO request_debug(id, session_id, captured_at, expires_at, payload) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, capturedAt, expiresAt, string(payload))
	return err
}

// debugCaptureBytes reads one committed capture's payload length on the held
// writer connection. An absent row contributes zero.
func (s *Store) debugCaptureBytes(ctx context.Context, conn *sql.Conn, id string) (int64, error) {
	var previous int64
	err := conn.QueryRowContext(ctx, `SELECT LENGTH(CAST(payload AS BLOB)) FROM request_debug WHERE id = ?`, id).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return previous, err
}

// LoadDebugCapture returns the payload for id, or nil if absent or expired.
func (s *Store) LoadDebugCapture(ctx context.Context, id string) ([]byte, error) {
	if id == "" {
		return nil, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	var payload string
	err := s.rdb.QueryRowContext(ctx,
		`SELECT payload FROM request_debug WHERE id = ? AND (expires_at = 0 OR expires_at > ?)`,
		id, time.Now().UnixMilli()).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(payload), nil
}

// debugExpireBatch bounds rows deleted per statement; each batch is one
// select plus one DELETE, so a large expired set never holds the writer
// connection for one unbounded statement.
const debugExpireBatch = 256

// debugExpireBatchesPerSweep bounds one sweep invocation (an explicit call or
// one writer tick). A large expired set continues on the writer's near-term
// debugExpireRetryInterval tick instead of monopolizing the writer.
const debugExpireBatchesPerSweep = 4

// debugExpireRetryInterval is the writer's near-term continuation cadence when
// a sweep stopped at debugExpireBatchesPerSweep. It is deliberately independent
// of storage_flush_interval, which may be configured much longer.
const debugExpireRetryInterval = 250 * time.Millisecond

// debugExpireInterval is how often the writer opportunistically sweeps expired
// sidecars. The TTL hides expired rows from reads immediately; this bounds the
// disk they can otherwise retain until the next restart.
const debugExpireInterval = 10 * time.Minute

// debugExpireQuery owns the expired-row selection for both byte-accounting
// modes. The cap-active form carries each payload length so a sweep can
// subtract freed bytes exactly; with the cap disabled lengths are never read.
// Neither form orders rows: batch order is not semantic, and ORDER BY would add
// a temp B-tree to every batch.
func debugExpireQuery(withBytes bool) string {
	if withBytes {
		return `SELECT rowid, LENGTH(CAST(payload AS BLOB)) FROM request_debug
			WHERE expires_at > 0 AND expires_at < ? LIMIT ?`
	}
	return `SELECT rowid FROM request_debug
		WHERE expires_at > 0 AND expires_at < ? LIMIT ?`
}

// ExpireDebugCaptures deletes expired sidecar rows in one bounded sweep.
// Expired rows are already invisible to reads; when another increment remains,
// the writer continues it on its near-term tick. beforeMs is the caller's
// clock.
func (s *Store) ExpireDebugCaptures(ctx context.Context, beforeMs int64) (int64, error) {
	total, more, err := s.expireDebugCaptures(ctx, beforeMs, debugExpireBatchesPerSweep)
	if more {
		s.debugExpirePending.Store(true)
	}
	return total, err
}

// expireDebugCaptures deletes at most maxBatches batches on the writer
// connection. It holds debugMu for the whole bounded sweep: a concurrent save
// cannot run without the same writer connection, so the lock only orders the
// counter updates and makes the cap-active subtraction exact.
func (s *Store) expireDebugCaptures(ctx context.Context, beforeMs int64, maxBatches int) (int64, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Close()
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	return s.expireDebugCapturesLocked(ctx, conn, beforeMs, maxBatches)
}

// expireDebugCapturesLocked is the one bounded expiry implementation. Callers
// must hold debugMu and own conn.
func (s *Store) expireDebugCapturesLocked(ctx context.Context, conn *sql.Conn, beforeMs int64, maxBatches int) (int64, bool, error) {
	withBytes := s.debugMaxBytes > 0
	var total int64
	for batch := 0; batch < maxBatches; batch++ {
		if err := ctx.Err(); err != nil {
			return total, false, err
		}
		rows, err := conn.QueryContext(ctx, debugExpireQuery(withBytes), beforeMs, debugExpireBatch)
		if err != nil {
			return total, false, err
		}
		var ids []int64
		var freed int64
		for rows.Next() {
			var id int64
			if withBytes {
				var n int64
				if err := rows.Scan(&id, &n); err != nil {
					rows.Close()
					return total, false, err
				}
				freed += n
			} else if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, false, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, false, err
		}
		rows.Close()
		if len(ids) == 0 {
			return total, false, nil
		}
		placeholders := make([]byte, 0, len(ids)*2)
		args := make([]any, 0, len(ids))
		for i, id := range ids {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		res, err := conn.ExecContext(ctx, `DELETE FROM request_debug WHERE rowid IN (`+string(placeholders)+`)`, args...)
		if err != nil {
			return total, false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, false, err
		}
		total += n
		exact := n == int64(len(ids))
		if withBytes && s.debugSeeded {
			// Under debugMu no in-process save can replace a selected row, so
			// the subtraction is exact. A mismatched row count means an
			// unsupported external writer changed rows; recompute once.
			if exact {
				s.debugBytes = max(0, s.debugBytes-freed)
			} else {
				// The DELETE above autocommitted, so this fresh read is the
				// exact committed sidecar total. If it fails, stay unseeded so
				// the next save reseeds instead of enforcing a stale counter.
				reseeded, rerr := debugByteTotal(ctx, conn)
				if rerr != nil {
					s.debugSeeded = false
					return total, false, rerr
				}
				s.debugBytes = reseeded
			}
		}
		if len(ids) < debugExpireBatch {
			return total, false, nil
		}
	}
	return total, true, nil
}

// CountDebugSession returns how many sidecar rows belong to sessionID.
func (s *Store) CountDebugSession(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	var n int64
	err := s.rdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM request_debug WHERE session_id = ? AND (expires_at = 0 OR expires_at > ?)`,
		sessionID, time.Now().UnixMilli()).Scan(&n)
	return n, err
}

// rowQuerier is the one-row read surface shared by sidecar byte accounting on
// a raw pool, a transaction or a held connection.
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// debugByteTotalHook is a test seam (production nil). It can force the
// compute-only sidecar read to fail so merge's commit-blocking accounting path
// stays testable without a fault-injection driver.
var debugByteTotalHook func() error

// debugByteTotal is the compute-only sidecar read on the caller's
// transaction/connection. It never publishes the counter: a transaction's
// uncommitted state may still roll back, so only its committer may adjust the
// counter afterwards.
func debugByteTotal(ctx context.Context, q rowQuerier) (int64, error) {
	if debugByteTotalHook != nil {
		if err := debugByteTotalHook(); err != nil {
			return 0, err
		}
	}
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug`).Scan(&n)
	return n, err
}

// installDebugBytes publishes a committed sidecar byte total. Callers must
// establish that the backing transaction (if any) committed: publishing an
// in-transaction total before commit lets a rollback silently bypass the cap.
func (s *Store) installDebugBytes(n int64) {
	s.debugMu.Lock()
	s.debugBytes, s.debugSeeded = n, true
	s.debugMu.Unlock()
}

// debugAfterCommitHook is a test seam (production nil). It runs after a
// maintenance transaction commits and before its sidecar delta is published,
// widening the exact window a concurrent capture must not lose.
var debugAfterCommitHook func()

// commitDebugDelta commits a maintenance transaction and applies its net
// sidecar byte delta while holding debugMu. Holding the lock across commit
// keeps a concurrent SaveDebugCapture (which needs the same writer connection)
// from incrementing the counter between the committed state and its
// publication, so the absolute publish bug becomes an exact delta applied to
// the current value. When the counter is unseeded the delta is deliberately
// dropped: the next save reseeds from durable rows and enforces the cap.
func (s *Store) commitDebugDelta(tx *sql.Tx, delta int64) error {
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	if err := tx.Commit(); err != nil {
		// A COMMIT error leaves the outcome uncertain. Never keep enforcing
		// the old absolute counter: the next save reseeds from durable rows.
		s.debugSeeded = false
		return err
	}
	if debugAfterCommitHook != nil {
		debugAfterCommitHook()
	}
	if s.debugSeeded {
		s.debugBytes = max(0, s.debugBytes+delta)
	}
	return nil
}

// seedDebugBytes initializes the sidecar byte counter from durable rows. The
// writer connection is free at Open; a failure leaves it unseeded so
// SaveDebugCapture retries before enforcing the cap.
func (s *Store) seedDebugBytes(ctx context.Context) error {
	n, err := debugByteTotal(ctx, s.db)
	if err != nil {
		return err
	}
	s.installDebugBytes(n)
	return nil
}

// expireDebugCapturesIfDue runs the expired-sidecar sweep from the writer
// goroutine at a bounded cadence. Expired rows are already invisible to reads;
// without the sweep their bytes stay on disk until the next restart. A sweep
// stopped at its per-tick bound keeps the pending flag set so the next tick
// continues immediately instead of waiting a full interval.
func (s *Store) expireDebugCapturesIfDue() {
	now := time.Now()
	if !s.debugExpirePending.Load() && !s.lastDebugExpire.IsZero() && now.Sub(s.lastDebugExpire) < debugExpireInterval {
		return
	}
	s.sweepDebugCaptures(now)
}

// continueDebugExpire drains the next bounded increment when the previous
// sweep stopped at its per-tick limit. The writer's dedicated ticker calls it
// even when the configured flush cadence is long.
func (s *Store) continueDebugExpire() {
	if !s.debugExpirePending.Load() {
		return
	}
	s.sweepDebugCaptures(time.Now())
}

// sweepDebugCaptures runs exactly one bounded sweep on the writer goroutine and
// records whether another increment remains.
func (s *Store) sweepDebugCaptures(now time.Time) {
	s.lastDebugExpire = now
	ctx, cancel := s.withQueryTimeout(context.Background())
	defer cancel()
	_, more, err := s.expireDebugCaptures(ctx, now.UnixMilli(), debugExpireBatchesPerSweep)
	s.debugExpirePending.Store(err == nil && more)
	if err != nil && ctx.Err() == nil {
		log.Printf("storage: expire debug captures: %v", err)
	}
}
