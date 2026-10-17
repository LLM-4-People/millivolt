package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SaveDebugCapture writes one request's debug document (sidecar). Synchronous
// on the writer connection so a drawer fetch after the request finalizes
// cannot miss the row.
func (s *Store) SaveDebugCapture(ctx context.Context, id, sessionID string, capturedAt, expiresAt int64, payload []byte) error {
	if id == "" {
		return errors.New("debug capture id required")
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
	var total int64
	for {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM request_debug WHERE rowid IN (
				SELECT rowid FROM request_debug
				WHERE expires_at > 0 AND expires_at < ? LIMIT ?)`,
			beforeMs, debugExpireBatch)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < debugExpireBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
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
