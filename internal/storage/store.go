package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenameProviders merges provider labels in stored history: every row whose
// provider matches an alias key is rewritten to its canonical label. Driven by
// config (provider_aliases) - a label split by a naming-scheme change is
// repaired generically, never with provider-specific code. Serialization with
// the writer goroutine comes from the single-connection write pool (the same
// shape a filtered Clear relies on); totals are label-independent, so no
// rebuild is needed. Malformed pairs are skipped (config Validate already rejects them -
// this is the last-chance deny), and each UPDATE is parameterized.
//
// All aliases commit as one transaction: a trigger or constraint failure on a
// later alias rolls the earlier rewrites back instead of leaving history split
// across old and new labels while the caller reports a failed merge. Pairs are
// applied in source-key order so the outcome (and trigger failure point) does
// not depend on Go map iteration order. An error returns 0 rows: nothing from
// the rolled-back transaction was renamed.
func (s *Store) RenameProviders(ctx context.Context, aliases map[string]string) (int64, error) {
	if s == nil || len(aliases) == 0 {
		return 0, nil
	}
	type pair struct{ from, to string }
	pairs := make([]pair, 0, len(aliases))
	for from, to := range aliases {
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if from == "" || to == "" || from == to {
			continue
		}
		pairs = append(pairs, pair{from: from, to: to})
	}
	if len(pairs) == 0 {
		return 0, nil
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].from < pairs[j].from })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, p := range pairs {
		res, err := tx.ExecContext(ctx, "UPDATE requests SET provider = ? WHERE provider = ?", p.to, p.from)
		if err != nil {
			return 0, fmt.Errorf("rename provider %q: %w", p.from, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit provider aliases: %w", err)
	}
	return total, nil
}

// ProjectionCursor pins one decoded analytical history to a consistent durable
// snapshot. RowID is a persistent high-water mark, not a request start time:
// late completions can carry arbitrarily old timestamps and still append.
type ProjectionCursor struct {
	Epoch, RowID, Schema int64
	Ready                bool
}

// StreamProjection materializes either all analytical rows or only append
// deltas under the interactive query budget. The epoch, high-water mark, and
// rows share one read transaction, so another process committing during a scan
// cannot make its cursor outrun its rows. Rewrites/removals/schema changes
// require a replacement snapshot. columns is code-owned (the same list consumed
// by scanContrib), never input from an HTTP request. No prompts, responses, or
// full records are retained.
func (s *Store) StreamProjection(ctx context.Context, columns string, cursor ProjectionCursor, fn func(*sql.Rows) error) (ProjectionCursor, bool, error) {
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	return s.streamProjection(ctx, columns, cursor, fn)
}

// StreamProjectionMaintenance is StreamProjection under the explicit
// maintenance budget. The startup preload uses it so a short interactive query
// timeout cannot abort a full-history materialization; an interactive catch-up
// must keep using StreamProjection.
func (s *Store) StreamProjectionMaintenance(ctx context.Context, columns string, cursor ProjectionCursor, fn func(*sql.Rows) error) (ProjectionCursor, bool, error) {
	ctx, cancel := s.withMaintenanceTimeout(ctx)
	defer cancel()
	return s.streamProjection(ctx, columns, cursor, fn)
}

func (s *Store) streamProjection(ctx context.Context, columns string, cursor ProjectionCursor, fn func(*sql.Rows) error) (ProjectionCursor, bool, error) {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProjectionCursor{}, false, err
	}
	defer tx.Rollback()
	var next ProjectionCursor
	if err := tx.QueryRowContext(ctx, "SELECT epoch, high_rowid FROM request_projection_state WHERE singleton = 1").Scan(&next.Epoch, &next.RowID); err != nil {
		return ProjectionCursor{}, false, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&next.Schema); err != nil {
		return ProjectionCursor{}, false, err
	}
	if next.Epoch < 0 || next.RowID < 0 {
		return ProjectionCursor{}, false, errors.New("invalid analytical projection state")
	}
	if !cursor.Ready || cursor.Schema != next.Schema {
		// A missing/replaced trigger destroys the append-only proof. Validate
		// the canonical definitions after every schema change, not every row.
		for _, tr := range projectionTriggers {
			var definition string
			err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_schema WHERE type = 'trigger' AND name = ? AND tbl_name = 'requests'", tr.name).Scan(&definition)
			want := tr.sql()
			if err != nil || definition != want {
				return ProjectionCursor{}, false, fmt.Errorf("invalid analytical projection trigger %s", tr.name)
			}
		}
	}
	full := !cursor.Ready || cursor.Epoch != next.Epoch || cursor.Schema != next.Schema || cursor.RowID > next.RowID
	query, args := "SELECT "+columns+" FROM requests", []any{}
	if !full {
		query += " WHERE rowid > ?"
		args = append(args, cursor.RowID)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return ProjectionCursor{}, false, err
	}
	for rows.Next() {
		if err := fn(rows); err != nil {
			rows.Close()
			return ProjectionCursor{}, false, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ProjectionCursor{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectionCursor{}, false, err
	}
	next.Ready = true
	return next, full, nil
}

// DataVersion identifies committed database content for aggregate memoization.
// SQLite versions are comparable only on the same connection, so never query
// this through the pool or replace a failed connection and reuse its version.
// Errors disable cache reuse; ordinary history queries can still proceed.
func (s *Store) DataVersion(ctx context.Context) (int64, error) {
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	return s.dataVersion(ctx)
}

// DataVersionMaintenance is DataVersion under the explicit maintenance
// budget: the startup preload's freshness witness belongs to the same
// full-history materialization and must not inherit the interactive timeout.
func (s *Store) DataVersionMaintenance(ctx context.Context) (int64, error) {
	ctx, cancel := s.withMaintenanceTimeout(ctx)
	defer cancel()
	return s.dataVersion(ctx)
}

func (s *Store) dataVersion(ctx context.Context) (int64, error) {
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	if s.versionClosed {
		return 0, sql.ErrConnDone
	}
	if s.versionConn == nil {
		conn, err := s.rdb.Conn(ctx)
		if err != nil {
			return 0, err
		}
		s.versionConn = conn
	}
	var version int64
	err := s.versionConn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version)
	return version, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Optional request params are pointers on metrics.Record; nil is stored as
// the zero value and restored as nil on load (see LoadRecent).
func intPtrVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func int64PtrVal(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func floatPtrVal(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func boolPtrVal(p *bool) int {
	if p == nil || !*p {
		return 0
	}
	return 1
}

func timeToMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
