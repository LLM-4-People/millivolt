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

// debugExpireBatch bounds rows deleted per statement; a sweep repeats until a
// short batch, so a large expired set never holds the writer connection for
// one unbounded DELETE.
const debugExpireBatch = 256

// debugExpireInterval is how often the writer opportunistically sweeps expired
// sidecars. The TTL hides expired rows from reads immediately; this bounds the
// disk they can otherwise retain until the next restart.
const debugExpireInterval = 10 * time.Minute

// ExpireDebugCaptures deletes sidecar rows whose expires_at is before beforeMs.
// The delete runs in bounded batches so one sweep can never hold the single
// writer connection for an unbounded time; it stops when the batch is short or
// the context is done.
func (s *Store) ExpireDebugCaptures(ctx context.Context, beforeMs int64) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var total int64
	for {
		// Select bounded rowids and their byte sizes first. Holding debugMu
		// across select+delete keeps a concurrent capture from replacing a row
		// between the two statements, and makes the counter subtraction exact.
		s.debugMu.Lock()
		rows, err := conn.QueryContext(ctx,
			`SELECT rowid, LENGTH(CAST(payload AS BLOB)) FROM request_debug
			WHERE expires_at > 0 AND expires_at < ? ORDER BY rowid LIMIT ?`,
			beforeMs, debugExpireBatch)
		if err != nil {
			s.debugMu.Unlock()
			return total, err
		}
		var ids []int64
		var freed int64
		for rows.Next() {
			var id, n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				s.debugMu.Unlock()
				return total, err
			}
			ids = append(ids, id)
			freed += n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			s.debugMu.Unlock()
			return total, err
		}
		rows.Close()
		if len(ids) == 0 {
			s.debugMu.Unlock()
			break
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
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			s.debugMu.Unlock()
			return total, err
		}
		total += n
		if s.debugSeeded {
			s.debugBytes = max(0, s.debugBytes-freed)
		}
		s.debugMu.Unlock()
		if n < debugExpireBatch || ctx.Err() != nil {
			break
		}
	}
	return total, nil
}

// CountDebugSession returns how many sidecar rows belong to sessionID.
func (s *Store) CountDebugSession(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	var n int64
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_debug WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}
