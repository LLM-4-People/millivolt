package storage

import (
	"context"
	"database/sql"
	"errors"
)

const (
	pauseMetaKey    = "pause"
	throttleMetaKey = "throttle"
	debugMetaKey    = "debug"
	// attemptsRepairMetaKey marks the one-time attempts-'' repair as done
	// (migrate writes it after a clean probe or after the repair itself), so
	// later boots skip the unindexed full-table probe.
	attemptsRepairMetaKey = "attempts_repaired"
)

// SaveMeta writes a durable key/value (operator state, not request rows).
// Synchronous on the writer connection so a reboot sees the latest value.
func (s *Store) SaveMeta(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("meta key required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, key, value)
	return err
}

// LoadMeta returns the value for key, or "" if the key is absent.
func (s *Store) LoadMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SavePause persists operator-pause JSON. Implements proxy.pausePersist.
func (s *Store) SavePause(ctx context.Context, raw []byte) error {
	return s.SaveMeta(ctx, pauseMetaKey, string(raw))
}

// LoadPause returns persisted operator-pause JSON, or nil if none.
func (s *Store) LoadPause(ctx context.Context) ([]byte, error) {
	v, err := s.LoadMeta(ctx, pauseMetaKey)
	if err != nil || v == "" {
		return nil, err
	}
	return []byte(v), nil
}

// SaveThrottle persists provider-throttle JSON. Implements proxy.pausePersist.
func (s *Store) SaveThrottle(ctx context.Context, raw []byte) error {
	return s.SaveMeta(ctx, throttleMetaKey, string(raw))
}

// LoadThrottle returns persisted provider-throttle JSON, or nil if none.
func (s *Store) LoadThrottle(ctx context.Context) ([]byte, error) {
	v, err := s.LoadMeta(ctx, throttleMetaKey)
	if err != nil || v == "" {
		return nil, err
	}
	return []byte(v), nil
}

// ListClients returns distinct non-empty Record.Client values from history.
func (s *Store) ListClients(ctx context.Context) ([]string, error) {
	return s.listDistinct(ctx, `SELECT DISTINCT client FROM requests WHERE client != '' ORDER BY client`)
}

// ListProviders returns distinct non-empty Record.Provider values from history.
func (s *Store) ListProviders(ctx context.Context) ([]string, error) {
	return s.listDistinct(ctx, `SELECT DISTINCT provider FROM requests WHERE provider != '' ORDER BY provider`)
}

// ListModels returns distinct non-empty Record.Model values from history.
func (s *Store) ListModels(ctx context.Context) ([]string, error) {
	return s.listDistinct(ctx, `SELECT DISTINCT model FROM requests WHERE model != '' ORDER BY model`)
}

// SaveDebugSessions persists operator-debug session JSON. Implements proxy.pausePersist.
func (s *Store) SaveDebugSessions(ctx context.Context, raw []byte) error {
	return s.SaveMeta(ctx, debugMetaKey, string(raw))
}

// LoadDebugSessions returns persisted operator-debug session JSON, or nil if none.
func (s *Store) LoadDebugSessions(ctx context.Context) ([]byte, error) {
	v, err := s.LoadMeta(ctx, debugMetaKey)
	if err != nil || v == "" {
		return nil, err
	}
	return []byte(v), nil
}

func (s *Store) listDistinct(ctx context.Context, q string) ([]string, error) {
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
