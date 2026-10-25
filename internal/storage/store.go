package storage

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

// Ad-hoc SELECTs cannot materialize thousands of independent large columns
// before the result-byte check. Internal safety guardrail, not user-tunable;
// the canonical requests schema (including SELECT *) fits below this ceiling.
const queryMaxColumns = 128

// errNotSelect rejects any dashboard query that is not a single SELECT.
var errNotSelect = errors.New("only a single SELECT statement is allowed")

// ErrQueryLimit rejects the entire result; callers must never serve a prefix.
var ErrQueryLimit = errors.New("query result exceeds its byte, row, or column limit")

// ErrExportTimeout marks a log export that hit its own total budget. It is
// distinct from a caller cancellation or caller deadline so the HTTP layer and
// tests can tell "the server abandoned this export" from "the client went
// away". After the sentinel, the response may already contain a partial
// artifact; see ExportWhere.
var ErrExportTimeout = errors.New("log export exceeded its time budget")

// buildExportQuery is the one owner of the durable export statement. The
// filter predicate is the same parameterized predicate purge and count use.
// nowMs prunes expired debug captures while their request row is read.
func buildExportQuery(f PurgeFilter, nowMs int64) (string, []any) {
	// The page cursor and its debug capture ride one row from one pooled read
	// connection. Loading each capture through a second connection would make
	// enough concurrent exporters deadlock against the bounded read pool.
	q := `SELECT ` + recordColumns + `, CASE WHEN requests.debug = 0 THEN NULL ELSE (
		SELECT d.payload FROM request_debug AS d
		WHERE d.id = requests.id AND (d.expires_at = 0 OR d.expires_at > ?)) END AS debug_capture
		FROM requests`
	args := []any{nowMs}
	where, whereArgs := f.build() // the same parameterized predicate as purge/count
	if where != "" {
		q += ` WHERE ` + where
		args = append(args, whereArgs...)
	}
	q += ` ORDER BY started_at ASC, id ASC`
	return q, args
}

// exportFailure maps an export error to the caller's cancellation/deadline when
// that is what happened, otherwise to ErrExportTimeout when the internal export
// budget expired. A caller cancellation always wins over the internal deadline
// so the two outcomes stay distinguishable.
func exportFailure(parent, bounded context.Context, err error) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrExportTimeout, context.DeadlineExceeded)
	}
	return err
}

// exportRowHook is a test seam (production leaves it nil). It runs after a row
// is fetched and before it is written, so a test can stall the row source under
// the export's own deadline without racing the SQLite driver.
var exportRowHook func(context.Context)

// ExportWhere streams every stored row matching the filter (an empty filter
// = all rows - export is read-only, so "everything" is safe) as a compact
// JSON array, one record per line, oldest-first, directly to the writer -
// memory stays bounded no matter how much history exists. Returns the number
// of records written.
//
// The whole stream is bounded by the internal exportTimeout (or the caller's
// shorter deadline), because an open export read transaction holds a WAL
// snapshot for its lifetime. A deadline that expires after the first byte was
// written leaves a partial, incomplete artifact on the caller's writer: the
// HTTP handler aborts the response instead of completing a 200 download. A
// caller cancellation or caller deadline is returned as that context error;
// the internal budget surfaces as ErrExportTimeout mixed with
// context.DeadlineExceeded.
func (s *Store) ExportWhere(ctx context.Context, f PurgeFilter, w io.Writer) (int64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	bounded, cancel := s.withExportTimeout(ctx)
	defer cancel()
	q, args := buildExportQuery(f, time.Now().UnixMilli())
	rows, err := s.rdb.QueryContext(bounded, q, args...)
	if err != nil {
		return 0, exportFailure(ctx, bounded, err)
	}
	defer rows.Close()
	var n int64
	write := func(text string) error {
		if err := bounded.Err(); err != nil {
			return err
		}
		_, err := io.WriteString(w, text)
		return err
	}
	if err := write("[\n"); err != nil {
		return 0, exportFailure(ctx, bounded, err)
	}
	for rows.Next() {
		if exportRowHook != nil {
			exportRowHook(bounded)
		}
		if err := bounded.Err(); err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		r, capture, err := scanRecord(rows)
		if err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		b, err := marshalExport(r, capture)
		if err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		if n > 0 {
			if err := write(",\n"); err != nil {
				return n, exportFailure(ctx, bounded, err)
			}
		}
		if _, err := w.Write(b); err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		if err := bounded.Err(); err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, exportFailure(ctx, bounded, err)
	}
	if err := write("\n]\n"); err != nil {
		return n, exportFailure(ctx, bounded, err)
	}
	return n, nil
}

// marshalExport encodes one record for log download. Debug captures ride
// as a sibling "debug_capture" object so the ring JSON stays small. The
// capture is read with the record's own export row, never through a second
// read-pool acquisition.
func marshalExport(r *metrics.Record, capture []byte) ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	if !r.Debug || len(capture) == 0 {
		return json.Marshal(r)
	}
	type exportRec struct {
		*metrics.Record
		DebugCapture json.RawMessage `json:"debug_capture,omitempty"`
	}
	return json.Marshal(exportRec{Record: r, DebugCapture: capture})
}

// purgeableColumns is the allowlist of columns a filtered purge may target.
// Deny by default: only these exact columns may appear in a purge WHERE clause,
// and they are always bound as parameters (never interpolated), so a purge can
// never be coerced into arbitrary SQL.
var purgeableColumns = map[string]bool{
	"provider": true, "model": true, "client": true,
	"status_code": true, "error_type": true, "conversation_id": true,
	"debug": true,
}

// PurgeFilter describes a filtered purge: rows matching all non-empty fields
// are deleted. Since every request is one self-contained row (retries live in
// the row's attempts JSON, conversation/turn are just fields), deleting rows
// can never leave orphans.
type PurgeFilter struct {
	Provider       string `json:"provider"`        // exact match
	Model          string `json:"model"`           // exact match
	Client         string `json:"client"`          // exact match
	ConversationID string `json:"conversation_id"` // exact match
	ErrorType      string `json:"error_type"`      // exact match (final error type)
	StatusCode     int    `json:"status_code"`     // exact match (0 = ignore)
	HasError       bool   `json:"has_error"`       // when true, only rows with a final error
	HasDebug       bool   `json:"debug"`           // when true, only operator-debug captures
	BeforeMs       int64  `json:"before_ms"`       // started_at < BeforeMs (0 = ignore)
	AfterMs        int64  `json:"after_ms"`        // started_at >= AfterMs (0 = ignore)
}

// Null is not an omitted constraint: accepting it beside another valid field
// would silently broaden a destructive selection. Keep this rule at the filter
// type so count and deletion decode exactly the same way.
func (f *PurgeFilter) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("filter object required")
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("filter %s cannot be null", key)
		}
	}
	type plainFilter PurgeFilter
	var decoded plainFilter
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	*f = PurgeFilter(decoded)
	return f.Validate()
}

// Validate is shared by count, export, and deletion, including non-HTTP callers.
func (f PurgeFilter) Validate() error {
	if f.StatusCode < 0 || f.StatusCode > 999 {
		return errors.New("invalid status_code")
	}
	if f.BeforeMs < 0 || f.AfterMs < 0 {
		return errors.New("invalid time filter")
	}
	if f.BeforeMs != 0 && f.AfterMs >= f.BeforeMs {
		return errors.New("after_ms must be less than before_ms")
	}
	return nil
}

// MatchRecord is the in-memory predicate equivalent of the SQL filter: it
// reports whether a buffered record matches every set constraint. Used to drop
// the same records from the live ring buffer that a filtered Clear deletes
// from the database, so the two never disagree.
func (f PurgeFilter) MatchRecord(r *metrics.Record) bool {
	if r == nil {
		return false
	}
	if f.Provider != "" && r.Provider != f.Provider {
		return false
	}
	if f.Model != "" && r.Model != f.Model {
		return false
	}
	if f.Client != "" && r.Client != f.Client {
		return false
	}
	if f.ConversationID != "" && r.ConversationID != f.ConversationID {
		return false
	}
	if f.ErrorType != "" && r.ErrorType != f.ErrorType {
		return false
	}
	if f.StatusCode != 0 && r.StatusCode != f.StatusCode {
		return false
	}
	if f.HasError && !r.IsError() {
		return false
	}
	if f.HasDebug && !r.Debug {
		return false
	}
	// Time filters compare UnixMilli directly, matching how
	// the SQL side compares the stored started_at) so the in-memory predicate
	// and the SQL WHERE agree exactly.
	if f.BeforeMs != 0 && r.Start.UnixMilli() >= f.BeforeMs {
		return false
	}
	if f.AfterMs != 0 && r.Start.UnixMilli() < f.AfterMs {
		return false
	}
	return true
}

// IsEmpty reports whether the filter matches everything (no constraint set).
func (f PurgeFilter) IsEmpty() bool {
	return f.Provider == "" && f.Model == "" && f.Client == "" &&
		f.ConversationID == "" && f.ErrorType == "" && f.StatusCode == 0 &&
		!f.HasError && !f.HasDebug && f.BeforeMs == 0 && f.AfterMs == 0
}

// build assembles the parameterized WHERE clause + args for the filter. Only
// allowlisted columns are used; every value is a bound parameter.
func (f PurgeFilter) build() (string, []any) {
	var where []string
	var args []any
	eq := func(col, val string) {
		if val != "" && purgeableColumns[col] {
			where = append(where, col+" = ?")
			args = append(args, val)
		}
	}
	eq("provider", f.Provider)
	eq("model", f.Model)
	eq("client", f.Client)
	eq("conversation_id", f.ConversationID)
	eq("error_type", f.ErrorType)
	if f.StatusCode != 0 {
		where = append(where, "status_code = ?")
		args = append(args, f.StatusCode)
	}
	if f.HasDebug {
		where = append(where, "debug != 0")
	}
	if f.HasError {
		// Must stay semantically identical to metrics.Record.IsError (the
		// canonical error predicate) so a filtered purge deletes exactly the
		// rows the live view considers errors:
		//   - 429 (rate limit) is a flow event, never an error on its own;
		//   - a plain 499 (local client closed the connection - its own
		//     cancellation) is a flow event too, but a 499 carrying a
		//     structured error_type observed the upstream's own failure
		//     (an in-band stream error the client aborted around) and counts;
		//   - any other error status (>= 400) or a structured error_type counts;
		//   - an absorbed retry attempt that hit a genuine 5xx counts even when
		//     the final outcome recovered (the upstream still failed).
		// The attempts column is a JSON array; json_each scans it for a 5xx.
		where = append(where, `(
			(status_code = 429 AND EXISTS (
				SELECT 1 FROM json_each(requests.attempts)
				WHERE json_extract(value, '$.status_code') >= 500
			))
			OR (status_code = 499 AND (error_type != '' OR EXISTS (
				SELECT 1 FROM json_each(requests.attempts)
				WHERE json_extract(value, '$.status_code') >= 500
			)))
			OR (status_code NOT IN (429, 499) AND (error_type != '' OR status_code >= 400))
			OR (status_code < 400 AND EXISTS (
				SELECT 1 FROM json_each(requests.attempts)
				WHERE json_extract(value, '$.status_code') >= 500
			))
		)`)
	}
	if f.BeforeMs != 0 {
		where = append(where, "started_at < ?")
		args = append(args, f.BeforeMs)
	}
	if f.AfterMs != 0 {
		where = append(where, "started_at >= ?")
		args = append(args, f.AfterMs)
	}
	return strings.Join(where, " AND "), args
}

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

// CountWhere returns how many stored rows match the filter (for the delete
// preview). It runs on the read pool like the other filter read paths
// (ExportWhere/QueryBefore): the HasError predicate's full-table json_each
// scan must never pin the sole writer connection up to QueryTimeout and
// stall insertBatch. With WAL the preview reads the committed snapshot, so
// it may miss the last un-flushed batch - exactly like the ExportWhere it
// previews, keeping the count==download contract.
func (s *Store) CountWhere(ctx context.Context, f PurgeFilter) (int64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	q, args := buildCountQuery(f)
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	var n int64
	err := s.rdb.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// buildCountQuery is the one owner of the delete-preview count statement. It
// shares the purge/export predicate owner, so a filter can never count a
// different row set than the export or delete it previews.
func buildCountQuery(f PurgeFilter) (string, []any) {
	where, args := f.build()
	q := "SELECT COUNT(*) FROM requests"
	if where != "" {
		q += " WHERE " + where
	}
	return q, args
}

// queryRenderHook is a test seam (production leaves it nil). It reports a
// completed ad-hoc rendering: queryRenderRows once Query materializes its row
// slice, queryRenderJSON once the endpoint assembles the full JSON result. The
// endpoint regression test requires exactly one JSON rendering and no rows
// rendering, which fails if the handler re-marshals a Query row slice.
var queryRenderHook func(kind string)

const (
	queryRenderRows = "rows"
	queryRenderJSON = "json"
)

// Query owns ad-hoc SELECT execution time and result resource bounds. The
// SELECT-only HTTP restriction stays in HandleQuery. SQLite working memory
// (sorting/aggregate intermediates) is not a strict per-query heap budget.
// The shared scan already encodes each row for the byte cap; this path keeps
// only the decoded rows because it has no response body to build.
func (s *Store) Query(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	out := make([]map[string]any, 0)
	err := s.queryScan(ctx, query, nil, func(row map[string]any) {
		out = append(out, row)
	}, args...)
	if err != nil {
		return nil, err
	}
	if queryRenderHook != nil {
		queryRenderHook(queryRenderRows)
	}
	return out, nil
}

// queryJSON executes the same bounded SELECT as Query and returns the complete
// JSON array assembled from the per-row encodings the byte cap already
// requires. This is the endpoint's one full-result encode: HandleQuery writes
// these bytes directly instead of re-marshaling the rows. The array is only
// returned after every row passed the caps, so a limit failure never leaves a
// partial body.
func (s *Store) queryJSON(ctx context.Context, query string, args ...any) ([]byte, error) {
	out := make([]byte, 0, 2)
	out = append(out, '[')
	if err := s.queryScan(ctx, query, &out, nil, args...); err != nil {
		return nil, err
	}
	out = append(out, ']')
	if queryRenderHook != nil {
		queryRenderHook(queryRenderJSON)
	}
	return out, nil
}

// queryScan is the one ad-hoc SELECT execution owner shared by Query and the
// HTTP endpoint. It applies the query-only SQLite limits, scans rows, enforces
// QueryMaxRows and QueryMaxBytes against the same per-row json.Marshal length,
// and routes each row to its caller's rendering: keepRow collects the decoded
// map, jsonOut collects the JSON array, and each caller supplies exactly one.
// Engine-limit failures are mapped to ErrQueryLimit here, before either
// rendering can expose a prefix.
func (s *Store) queryScan(ctx context.Context, query string, jsonOut *[]byte, keepRow func(map[string]any), args ...any) (err error) {
	defer func() {
		if err != nil && queryEngineLimit(err) {
			err = fmt.Errorf("%w: %v", ErrQueryLimit, err)
		}
	}()
	if s.opts.QueryMaxBytes < 2 || s.opts.QueryMaxRows < 1 {
		return ErrQueryLimit
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	conn, err := s.rdb.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	type limit struct{ id, value int }
	var previous []limit
	defer func() {
		var restoreErr error
		for _, old := range previous {
			if _, e := sqlite.Limit(conn, old.id, old.value); e != nil {
				restoreErr = errors.Join(restoreErr, e)
			}
		}
		if restoreErr != nil {
			// Never return a connection with query-only limits to the shared
			// pool: internal history readers must keep their normal authority.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("restore query limits: %w", restoreErr))
		}
	}()
	for _, bound := range [...]limit{{sqlite3.SQLITE_LIMIT_LENGTH, s.opts.QueryMaxBytes}, {sqlite3.SQLITE_LIMIT_COLUMN, queryMaxColumns}} {
		old, e := sqlite.Limit(conn, bound.id, bound.value)
		if e != nil {
			return e
		}
		previous = append(previous, limit{bound.id, old})
	}
	// modernc steps the first row inside QueryContext; limits must already
	// be installed here, not merely before Rows.Next/Scan.
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	// Keys, their order and the duplicate-name collapse are decided once per
	// query; the per-row cost is one leaf encode per column and nothing else.
	// One reusable value/pointer pair and one reusable row buffer replace the
	// per-row map, its key sort and its marshal.
	proj, err := newQueryProjection(cols)
	if err != nil {
		return err
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	var row jsonSink
	rowEnc := json.NewEncoder(&row)

	resultBytes := 2 // JSON array brackets, including the empty-array case
	rowCount := 0
	for rows.Next() {
		if rowCount == s.opts.QueryMaxRows {
			return ErrQueryLimit
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row.buf = row.buf[:0]
		if err := proj.encodeRow(&row, rowEnc, values); err != nil {
			return err
		}
		separator := 0
		if rowCount > 0 {
			separator = 1
		}
		// The cap counts the encoded row, never a map slice: this is the same
		// length json.Marshal of the row map produces, so the boundary is
		// unchanged.
		if len(row.buf) > s.opts.QueryMaxBytes-resultBytes-separator {
			return ErrQueryLimit
		}
		resultBytes += len(row.buf) + separator
		rowCount++
		if jsonOut != nil {
			if separator == 1 {
				*jsonOut = append(*jsonOut, ',')
			}
			*jsonOut = append(*jsonOut, row.buf...)
		}
		if keepRow != nil {
			decoded := make(map[string]any, len(cols))
			for i, c := range cols {
				decoded[c] = values[i]
			}
			keepRow(decoded)
		}
	}
	return rows.Err()
}

func queryEngineLimit(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	if sqliteErr.Code()&0xff == sqlite3.SQLITE_TOOBIG {
		return true
	}
	// SQLite reports the COLUMN limit as generic SQLITE_ERROR, not TOOBIG.
	// Match only the engine's documented-limit diagnostics, not syntax errors.
	if sqliteErr.Code() == sqlite3.SQLITE_ERROR {
		for _, message := range [...]string{"too many columns in result set", "too many terms in ORDER BY clause", "too many terms in GROUP BY clause"} {
			if strings.Contains(sqliteErr.Error(), message) {
				return true
			}
		}
	}
	return false
}

// validateQuery enforces the dashboard query contract: a single SELECT
// statement, nothing else. The read pool is opened mode=ro as a second layer
// of defense - but mode=ro only pins the MAIN database read-only: SQLite still
// honors ATTACH on a read-only connection and opens the attached DB read-write.
// So ATTACH (and other schema/mutation keywords that can ride a SELECT-leading
// string) are denied here explicitly - the real choke point the driver can't
// provide (modernc.org/sqlite exposes no authorizer hook).
func validateQuery(q string) error {
	t := strings.TrimSpace(q)
	if t == "" {
		return errNotSelect
	}
	if !strings.HasPrefix(strings.ToUpper(t), "SELECT") {
		return errNotSelect
	}
	// Single statement only: reject any statement terminator.
	if strings.Contains(t, ";") {
		return errNotSelect
	}
	// Deny side-effecting keywords even inside a SELECT-leading string (e.g.
	// "SELECT … ATTACH …", "SELECT load_extension(...)"). Word-boundary match
	// at the ASCII boundary: ASCII letters, digits and underscores continue a
	// word, so a superstring spelling such as a column named "attachment" or
	// "attach_x" passes, while a column literally named "attach" is denied
	// with the keyword - the accepted cost of denying the bare word
	// everywhere. A byte outside ASCII ends a word even when it spells a
	// letter or digit, so an attaché-class spelling trips the keyword
	// (containsWord states the measured boundary).
	up := strings.ToUpper(t)
	for _, kw := range []string{"ATTACH", "DETACH", "PRAGMA", "LOAD_EXTENSION"} {
		if containsWord(up, kw) {
			return errNotSelect
		}
	}
	return nil
}

// containsWord reports whether s contains the keyword kw surrounded by
// non-word characters. The boundary is ASCII: ASCII letters, digits and
// underscores continue a word (isAlphaNum), so "ATTACH" is not found in
// "attachment", "attach_x" or "x_attach"; a byte outside ASCII ends a word
// even when it spells a letter or digit, so an attaché-class spelling trips
// the keyword - measured after ToUpper, "attaché", "éattach", "attachß",
// "attachﬁ" and an Arabic-Indic digit joined to the keyword are all denied,
// while "attachſ" passes only because its upper-case is the ASCII "S".
func containsWord(s, kw string) bool {
	for i := 0; ; {
		idx := strings.Index(s[i:], kw)
		if idx < 0 {
			return false
		}
		j := i + idx
		before := j == 0 || !isAlphaNum(s[j-1])
		after := j+len(kw) >= len(s) || !isAlphaNum(s[j+len(kw)])
		if before && after {
			return true
		}
		i = j + len(kw)
	}
}

func isAlphaNum(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// HandleQuery serves a JSON array of rows for a SELECT query passed as
// ?q=... (URL-decoded). Enforces SELECT-only, single-statement, and a bounded
// execution time. A nil store returns 503, never inference fallback.
// Same-origin dashboard endpoint; no CORS header.
func (s *Store) HandleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, message string) {
		adminjson.WriteErrorJSON(w, status, message)
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s == nil {
		fail(http.StatusServiceUnavailable, ErrStorageDisabled.Error())
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		fail(http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "q"); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if q.Get("q") == "" {
		fail(http.StatusBadRequest, "missing q")
		return
	}
	query := q.Get("q")
	if err := validateQuery(query); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	data, err := s.queryJSON(r.Context(), query)
	if err != nil {
		// The verbatim body serves the operator's browser (operator-gated
		// path); the server-side log is the durable record of the cause.
		log.Printf("storage: query failed: %v", err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrQueryLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		fail(status, err.Error())
		return
	}
	w.Write(data)
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
