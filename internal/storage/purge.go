package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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
