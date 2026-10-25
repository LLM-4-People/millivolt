package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// LoadRecent returns the most recent n records from the database, ordered by
// start time (oldest first). Used to backfill the in-memory ring buffer on
// startup so the dashboard keeps history across restarts.
// recordColumns lists every dashboard-visible field in the canonical column
// order - the one select-side source shared by LoadRecent and ExportWhere
// (the write side keeps the same order in insertBatch; schema/migrate stay in
// lockstep, so adding a field means extending all of these together).
const recordColumns = `
		id, provider, model, key_hash, user_agent, client, client_ip, client_lang,
		stream, status_code,
		started_at, duration_ms, ttft_ms, first_token_at, last_token_at, chunks,
		request_bytes, response_bytes, upstream_gzip,
		upstream_connect_ms, upstream_tls_ms, upstream_ttfb_ms, upstream_conn_reused,
		finish_reason, error_type, error_msg, error_code, tool_calls,
		input_tokens, output_tokens, total_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, client_disconnected,
		rate_limit_remaining, rate_limit_limit, cost,
		turns_user, turns_assistant, turns_tool, response_headers,
		answer_tokens, first_answer_at, first_reasoning_at, gen_tokens, had_answer_content, gen_tps,
		method, path, processing_ms, prompt_preview, response_preview,
		provider_model, provider_request_id, provider_server,
		queue_wait_ms, rate_limited, retries, retry_after_ms, tool_names,
		req_max_tokens, req_temperature, req_top_p, req_tools_count, req_tool_choice,
		req_n, req_stop, req_logprobs, req_presence_pen, req_frequency_pen,
		req_response_format, req_seed, req_parallel_tools, req_logit_bias,
		req_top_logprobs, req_service_tier, req_thinking, req_metadata_keys,
		req_stream_opts, attempts, final_attempt_at, conversation_id, parent_conversation_id,
		chars_system, chars_user, chars_assistant, chars_tool,
		images, attachments, last_turn_role, client_meta, req_reasoning_effort, req_verbosity,
		debug, debug_session_id, req_param_presence
	`

// recordScanColumns parses recordColumns into its ordered column names. Every
// column it returns is scanned by scanRecord into a non-null Go string,
// integer, float or bool, so admission preflight must reject NULL and values
// whose SQLite storage class or range cannot reach that destination.
// debug_capture is deliberately not part of recordColumns: callers add it
// separately and it is nullable.
func recordScanColumns() []string {
	parts := strings.Split(recordColumns, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// recordScanScratch holds the scan destinations that are not fields on
// metrics.Record: derived timestamps, JSON columns decoded after the scan,
// and values restored into named fields.
type recordScanScratch struct {
	startMs, durMs, ttftMs, firstMs, lastMs, firstAnswerMs int64
	respHdr, toolNames, attemptsJSON                       string
	finalAttemptMs                                         int64
	rateLimited                                            bool
	hadAnswer                                              int
	reqMaxTokens, reqN, reqTopLogprobs                     int
	reqTemp, reqTopP, reqPresencePen, reqFrequencyPen      float64
	reqSeed                                                int64
	reqParallelTools                                       bool
	clientMetaJSON                                         string
	debugFlag                                              int
	reqPresence                                            int64
}

// recordScanTargets returns the ordered scan destinations for recordColumns.
// scanRecord scans through this list, and the snapshot admission preflight
// derives each column's accepted SQLite value class from the same
// destinations. Changing a destination type therefore changes validation with
// it instead of leaving a second, hand-maintained type list behind.
func recordScanTargets(r *metrics.Record, s *recordScanScratch) []any {
	return []any{
		&r.ID, &r.Provider, &r.Model, &r.KeyHash, &r.UserAgent, &r.Client,
		&r.ClientIP, &r.ClientLang, &r.Stream,
		&r.StatusCode, &s.startMs, &s.durMs, &s.ttftMs, &s.firstMs, &s.lastMs, &r.Chunks,
		&r.RequestBytes, &r.ResponseBytes, &r.UpstreamGzip,
		&r.UpstreamConnectMs, &r.UpstreamTLSMs, &r.UpstreamTTFBMs, &r.UpstreamConnReused,
		&r.FinishReason, &r.ErrorType, &r.ErrorMsg, &r.ErrorCode, &r.ToolCalls,
		&r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.TotalTokens,
		&r.Usage.CacheReadTokens, &r.Usage.CacheWrite, &r.Usage.ReasoningTokens,
		&r.ClientDisconnected, &r.RateLimitRemaining, &r.RateLimitLimit,
		&r.Cost, &r.TurnsUser, &r.TurnsAssistant, &r.TurnsTool, &s.respHdr,
		&r.AnswerTokens, &s.firstAnswerMs, &r.FirstReasoningAt, &r.GenTokens, &s.hadAnswer, &r.DecodeTPS,
		&r.Method, &r.Path, &r.ProcessingMs, &r.PromptPreview, &r.ResponsePreview,
		&r.ProviderModel, &r.ProviderRequestID, &r.ProviderServer,
		&r.QueueWaitMs, &s.rateLimited, &r.Retries, &r.RetryAfterMs, &s.toolNames,
		&s.reqMaxTokens, &s.reqTemp, &s.reqTopP, &r.ReqToolsCount, &r.ReqToolChoice,
		&s.reqN, &r.ReqStop, &r.ReqLogprobs, &s.reqPresencePen, &s.reqFrequencyPen,
		&r.ReqResponseFormat, &s.reqSeed, &s.reqParallelTools, &r.ReqLogitBias,
		&s.reqTopLogprobs, &r.ReqServiceTier, &r.ReqThinking, &r.ReqMetadataKeys,
		&r.ReqStreamOpts, &s.attemptsJSON, &s.finalAttemptMs, &r.ConversationID, &r.ParentConversationID,
		&r.CharsSystem, &r.CharsUser, &r.CharsAssistant, &r.CharsTool,
		&r.Images, &r.Attachments, &r.LastTurnRole,
		&s.clientMetaJSON, &r.ReqReasoningEffort, &r.ReqVerbosity,
		&s.debugFlag, &r.DebugSessionID, &s.reqPresence,
	}
}

// scanRecord scans one row (in the canonical recordColumns order plus the
// caller's debug_capture column) into a metrics.Record, restoring the exact
// dashboard JSON shape (pointer request params including explicit zero/false,
// derived timestamps, parsed JSON columns). Queries that do not need a capture
// select NULL for the trailing column; ExportWhere selects the sidecar payload
// for the same row.
func scanRecord(rows *sql.Rows) (*metrics.Record, []byte, error) {
	var r metrics.Record
	var s recordScanScratch
	var debugCapture []byte
	targets := append(recordScanTargets(&r, &s), &debugCapture)
	if err := rows.Scan(targets...); err != nil {
		return nil, nil, err
	}
	// time.UnixMilli returns local time. Keep every derived Time in UTC so JSON
	// marshalling is independent of the process zone: the admitted minimum
	// millisecond is year 0 in UTC, but a negative offset would render it as
	// year -1, which time.Time.MarshalJSON rejects. The stored epoch values are
	// unchanged.
	r.Start = time.UnixMilli(s.startMs).UTC()
	r.End = r.Start.Add(time.Duration(s.durMs) * time.Millisecond)
	r.DurationMs = s.durMs // restore the stored duration; End is derived from it
	r.TTFTMs = s.ttftMs
	r.HadAnswerContent = s.hadAnswer != 0
	if s.firstMs > 0 {
		r.FirstTokenAt = time.UnixMilli(s.firstMs).UTC()
	}
	if s.lastMs > 0 {
		r.LastTokenAt = time.UnixMilli(s.lastMs).UTC()
	}
	if s.firstAnswerMs > 0 {
		r.FirstAnswerAt = time.UnixMilli(s.firstAnswerMs).UTC()
	}
	if s.finalAttemptMs > 0 {
		r.FinalAttemptAt = time.UnixMilli(s.finalAttemptMs).UTC()
	}
	if s.respHdr != "" {
		if err := json.Unmarshal([]byte(s.respHdr), &r.ResponseHeaders); err != nil {
			log.Printf("storage: parse response headers for %s: %v", r.ID, err)
		}
	}
	if names, err := DecodeToolNamesColumn([]byte(s.toolNames)); err != nil {
		log.Printf("storage: parse tool names for %s: %v", r.ID, err)
	} else {
		r.ToolNames = names
	}
	if attempts, err := DecodeAttemptsColumn([]byte(s.attemptsJSON)); err != nil {
		log.Printf("storage: parse attempts for %s: %v", r.ID, err)
	} else {
		r.Attempts = attempts
	}
	r.RateLimited = s.rateLimited
	r.Debug = s.debugFlag != 0
	r.ReqMaxTokens, r.ReqTemperature, r.ReqTopP = &s.reqMaxTokens, &s.reqTemp, &s.reqTopP
	r.ReqN, r.ReqPresencePen, r.ReqFrequencyPen = &s.reqN, &s.reqPresencePen, &s.reqFrequencyPen
	r.ReqSeed, r.ReqParallelTools, r.ReqTopLogprobs = &s.reqSeed, &s.reqParallelTools, &s.reqTopLogprobs
	restoreRequestParams(&r, s.reqPresence)
	if s.clientMetaJSON != "" && s.clientMetaJSON != "{}" {
		if err := json.Unmarshal([]byte(s.clientMetaJSON), &r.ClientMeta); err != nil {
			log.Printf("storage: parse client_meta for %s: %v", r.ID, err)
		}
	}
	return &r, debugCapture, nil
}

// decodeJSONColumn is the shared owner of the stored JSON-column decode:
// empty and "null" are the canonical no-value spellings and yield nil with
// no error; any other unmarshal failure is column corruption the caller
// logs per record (never a failed scan).
func decodeJSONColumn[T any](raw []byte) ([]T, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeAttemptsColumn decodes the stored attempts JSON column through the
// shared decodeJSONColumn contract (external consumers live in internal/web).
func DecodeAttemptsColumn(raw []byte) ([]metrics.RetryAttempt, error) {
	return decodeJSONColumn[metrics.RetryAttempt](raw)
}

// DecodeToolNamesColumn decodes the stored tool_names JSON column with the
// same empty/"null" contract as DecodeAttemptsColumn.
func DecodeToolNamesColumn(raw []byte) ([]string, error) {
	return decodeJSONColumn[string](raw)
}

func marshalClientMeta(m metrics.ClientMeta) string {
	if m.Empty() {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// LogCursor is the complete durable ordering key. A timestamp alone cannot
// distinguish requests started in the same millisecond.
type LogCursor struct {
	StartMs int64
	ID      string
}

// LogCandidate is the narrow projection one durable row contributes to the
// live-log page's authoritative Go matcher. SQL still applies every exact
// stored predicate; this struct carries only the derived inputs SQL cannot
// express (status classes, error signatures, tools, canonical models and time
// buckets) plus the ordering key. Full Record materialization is deferred to
// LogPageRecords for the rows the matcher accepts, so a rejected scan row
// never decodes prompts, previews, headers, or request parameters.
type LogCandidate struct {
	Cursor    LogCursor
	Status    int
	Model     string
	ToolNames []string
	ErrorType string
	ErrorCode string
	ErrorMsg  string
	Attempts  []metrics.RetryAttempt
}

// QueryBefore returns a newest-first page below the exclusive cursor. A nil
// cursor starts at durable newest, independently of the arrival-ordered ring.
// LogField is the closed set of exact durable columns a log page may push
// into SQL. The enum is the trust boundary: callers never supply identifiers.
type LogField int

const (
	LogProvider LogField = iota
	LogClient
	LogKeyHash
	LogConversation
	LogModel
	LogStatus
)

// LogPredicate is one exact equality (ANDed with every other predicate). The
// Go scope matcher remains authoritative; pushdown only reduces decode work.
type LogPredicate struct {
	Field LogField
	Value string
}

// LogFilter carries SQL-safe exact predicates plus optional alternative
// groups. All predicates are AND-ed. Each Any group is one dimension's OR
// alternatives, and every group is AND-ed with All and with the other groups:
// repeated provider filters remain one provider alternative set while a
// repeated client filter stays a separate, intersecting set. Zero value
// selects everything.
type LogFilter struct {
	All []LogPredicate
	Any [][]LogPredicate
}

func (f *LogFilter) where() (string, []any, error) {
	if f == nil {
		return "", nil, nil
	}
	argCount := len(f.All)
	for _, group := range f.Any {
		argCount += len(group)
	}
	args := make([]any, 0, argCount)
	columnOf := func(p LogPredicate) (string, error) {
		switch p.Field {
		case LogProvider:
			return "provider", nil
		case LogClient:
			return "client", nil
		case LogKeyHash:
			return "key_hash", nil
		case LogConversation:
			return "conversation_id", nil
		case LogModel:
			return "model", nil
		case LogStatus:
			return "status_code", nil
		default:
			// Deny by default: a typo or a future field must never silently
			// broaden the page to unfiltered rows.
			return "", fmt.Errorf("unknown log field %d", p.Field)
		}
	}
	var clause string
	for _, p := range f.All {
		column, err := columnOf(p)
		if err != nil {
			return "", nil, err
		}
		clause += " AND " + column + " = ?"
		args = append(args, p.Value)
	}
	for _, predicates := range f.Any {
		if len(predicates) == 0 {
			// A caller-built empty group has no meaning and rendering it as
			// "()" would be invalid SQL. Deny it instead of silently dropping
			// an intended constraint.
			return "", nil, fmt.Errorf("empty log predicate group")
		}
		var group strings.Builder
		for i, p := range predicates {
			column, err := columnOf(p)
			if err != nil {
				return "", nil, err
			}
			if i > 0 {
				group.WriteString(" OR ")
			}
			group.WriteString(column)
			group.WriteString(" = ?")
			args = append(args, p.Value)
		}
		clause += " AND (" + group.String() + ")"
	}
	return clause, args, nil
}

// logCandidateColumns is the closed narrow projection the live-log Go matcher
// reads. Keep it aligned with LogCandidate: every column here is consumed by
// the derived predicates, never by response materialization. Adding a field
// means extending both together.
const logCandidateColumns = `
	id, started_at, status_code, model, tool_names,
	error_type, error_code, error_msg, attempts
`

// buildBeforeQuery is the single owner of the durable log page statement
// shape: newest-first below an exclusive (started_at,id) cursor, with SQL-safe
// exact predicates applied by SQLite before decoding. idx_requests_log is the
// only request-log index, so the page walks it in cursor order and rechecks
// each equality predicate per row: one ordered scan and no sort. A filter with
// no time bound still scans from newest, so a highly selective equality pays a
// longer scan than the old per-dimension index did. Callers that need
// canonical-model or class semantics still match in Go. columns is code-owned,
// never request input.
func buildBeforeQuery(columns string, before *LogCursor, filter *LogFilter, limit int) (string, []any, error) {
	q := `SELECT ` + columns + ` FROM requests WHERE 1=1`
	var args []any
	if before != nil {
		q += ` AND (started_at, id) < (?, ?)`
		args = append(args, before.StartMs, before.ID)
	}
	push, pushArgs, err := filter.where()
	if err != nil {
		return "", nil, err
	}
	q += push
	args = append(args, pushArgs...)
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return q, args, nil
}

func buildQueryBefore(before *LogCursor, filter *LogFilter, limit int) (string, []any, error) {
	return buildBeforeQuery(recordColumns+`, NULL AS debug_capture`, before, filter, limit)
}

// QueryBefore pages durable records newest-first below an exact (time,id)
// cursor. filter predicates are applied by SQLite before decoding; callers
// that need canonical-model or class semantics still match in Go.
func (s *Store) QueryBefore(ctx context.Context, before *LogCursor, filter *LogFilter, limit int) ([]*metrics.Record, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	q, args, err := buildQueryBefore(before, filter, limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*metrics.Record
	for rows.Next() {
		r, _, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryBeforeCandidates pages the narrow matcher projection newest-first
// below the same exclusive cursor and SQL predicates as QueryBefore. It is the
// log page's scan read: one bounded statement whose rows carry only the
// derived fields the Go matcher needs, never a full Record.
func (s *Store) QueryBeforeCandidates(ctx context.Context, before *LogCursor, filter *LogFilter, limit int) ([]LogCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	q, args, err := buildBeforeQuery(logCandidateColumns, before, filter, limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogCandidate
	for rows.Next() {
		var c LogCandidate
		var toolNames, attemptsJSON string
		if err := rows.Scan(
			&c.Cursor.ID, &c.Cursor.StartMs, &c.Status, &c.Model, &toolNames,
			&c.ErrorType, &c.ErrorCode, &c.ErrorMsg, &attemptsJSON,
		); err != nil {
			return nil, err
		}
		if names, err := DecodeToolNamesColumn([]byte(toolNames)); err != nil {
			log.Printf("storage: parse tool names for %s: %v", c.Cursor.ID, err)
		} else {
			c.ToolNames = names
		}
		if attempts, err := DecodeAttemptsColumn([]byte(attemptsJSON)); err != nil {
			log.Printf("storage: parse attempts for %s: %v", c.Cursor.ID, err)
		} else {
			c.Attempts = attempts
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LogPageRecords materializes exactly the accepted log-page cursors in one
// bounded statement, preserving the scan's newest-first order. The caller
// passes at most one page of IDs; a row accepted by the candidate scan but
// missing or out of order here is an explicit consistency error, never a
// silently shortened page. The IN list cannot exceed the page limit, so this
// is a bounded key lookup rather than a second history scan. An empty ID is a
// valid exact key: the schema allows it, the primary key admits it once, and
// QueryBeforeCandidates returns it as an ordinary cursor.
func (s *Store) LogPageRecords(ctx context.Context, cursors []LogCursor) ([]*metrics.Record, error) {
	if len(cursors) == 0 {
		return nil, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	var q strings.Builder
	q.WriteString(`SELECT ` + recordColumns + `, NULL AS debug_capture FROM requests WHERE id IN (`)
	args := make([]any, 0, len(cursors))
	seen := make(map[string]struct{}, len(cursors))
	for i, c := range cursors {
		if _, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("log page records: duplicate record id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if i > 0 {
			q.WriteByte(',')
		}
		q.WriteByte('?')
		args = append(args, c.ID)
	}
	q.WriteString(`) ORDER BY started_at DESC, id DESC`)
	rows, err := s.rdb.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*metrics.Record, 0, len(cursors))
	for rows.Next() {
		r, _, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(cursors) {
		return nil, fmt.Errorf("log page records: materialized %d of %d accepted rows", len(out), len(cursors))
	}
	for i, c := range cursors {
		if out[i].ID != c.ID || out[i].Start.UnixMilli() != c.StartMs {
			return nil, fmt.Errorf("log page records: accepted row %d does not match its scan cursor", i)
		}
	}
	return out, nil
}

// LoadRecent returns up to n of the newest stored records, oldest-first. It
// powers the startup backfill (bounded by the configured history_size) so the
// dashboard survives restarts.
func (s *Store) LoadRecent(ctx context.Context, n int) ([]*metrics.Record, error) {
	// Defensive bound: SQLite treats a negative LIMIT as unlimited, so clamp a
	// negative n to 0 (return nothing) rather than dump the whole table.
	if n < 0 {
		n = 0
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+recordColumns+`, NULL AS debug_capture FROM requests ORDER BY started_at DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*metrics.Record
	for rows.Next() {
		r, _, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	// Reverse so oldest is first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}
