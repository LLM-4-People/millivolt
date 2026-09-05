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

// ExpireDebugCaptures deletes sidecar rows whose expires_at is before beforeMs.
func (s *Store) ExpireDebugCaptures(ctx context.Context, beforeMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_debug WHERE expires_at > 0 AND expires_at < ?`, beforeMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
