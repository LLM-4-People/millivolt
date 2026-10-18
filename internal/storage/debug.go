package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		var previous int64
		err = conn.QueryRowContext(ctx, `SELECT LENGTH(CAST(payload AS BLOB)) FROM request_debug WHERE id = ?`, id).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		next := s.debugBytes - previous + int64(len(payload))
		if next > s.debugMaxBytes {
			return fmt.Errorf("debug capture store limit reached (%d of %d bytes)", s.debugBytes, s.debugMaxBytes)
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

// expireDebugCaptures deletes at most maxBatches batches and reports whether a
// full final batch may leave more expired rows. Holding debugMu across each
// select+delete keeps a concurrent capture from replacing a row between the two
// statements and makes the cap-active counter subtraction exact.
func (s *Store) expireDebugCaptures(ctx context.Context, beforeMs int64, maxBatches int) (int64, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Close()
	withBytes := s.debugMaxBytes > 0
	var total int64
	for batch := 0; batch < maxBatches; batch++ {
		if err := ctx.Err(); err != nil {
			return total, false, err
		}
		s.debugMu.Lock()
		rows, err := conn.QueryContext(ctx, debugExpireQuery(withBytes), beforeMs, debugExpireBatch)
		if err != nil {
			s.debugMu.Unlock()
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
					s.debugMu.Unlock()
					return total, false, err
				}
				freed += n
			} else if err := rows.Scan(&id); err != nil {
				rows.Close()
				s.debugMu.Unlock()
				return total, false, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			s.debugMu.Unlock()
			return total, false, err
		}
		rows.Close()
		if len(ids) == 0 {
			s.debugMu.Unlock()
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
			s.debugMu.Unlock()
			return total, false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			s.debugMu.Unlock()
			return total, false, err
		}
		total += n
		exact := n == int64(len(ids))
		needsReseed := false
		if withBytes && s.debugSeeded {
			// Under debugMu no in-process save can replace a selected row, so
			// the subtraction is exact. A mismatched row count means an
			// unsupported external writer changed rows; recompute once.
			if exact {
				s.debugBytes = max(0, s.debugBytes-freed)
			} else {
				needsReseed = true
			}
		}
		s.debugMu.Unlock()
		if needsReseed {
			if err := s.resetDebugBytes(ctx, conn); err != nil {
				return total, false, err
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
