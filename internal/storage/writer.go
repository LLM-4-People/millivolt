package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The column order matches the one record-value list in insertBatch. Every
// column is written explicitly; SQL defaults never determine retained values.
// It is an append-only INSERT: a repeated id keeps the first durable row and
// is reported as not inserted, so totals can count exactly the rows on disk.
const insertColumns = `INSERT INTO requests
		(id, provider, model, key_hash, user_agent, client, client_ip, client_lang,
		 stream, status_code,
		 started_at, duration_ms, ttft_ms, first_token_at, last_token_at, chunks,
		 request_bytes, response_bytes, upstream_gzip,
		 upstream_connect_ms, upstream_tls_ms, upstream_ttfb_ms, upstream_conn_reused,
		 finish_reason, error_type, error_msg, error_code, tool_calls,
		 input_tokens, output_tokens, total_tokens, cache_read_tokens,
		 cache_write_tokens, reasoning_tokens, client_disconnected,
		 rate_limit_remaining, rate_limit_limit, cost,
		 turns_user, turns_assistant, turns_tool, response_headers,
		 answer_tokens, first_answer_at, first_reasoning_at, gen_tokens, had_answer_content, gen_tps, overall_tps,
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
		 debug, debug_session_id, req_param_presence)`

// insertConflict makes the writer idempotent without rewriting an existing
// row. A duplicate id is a no-op: the first durable row wins and RowsAffected
// reports zero, which is what keeps the append-only totals exact.
const insertConflict = " ON CONFLICT(id) DO NOTHING"

// batchInsert retains at most one prepared statement. A batch starts with
// placeholders only for nonzero values, and promotes additional columns as
// needed. Once promoted a column stays bound, including later zero values.
// Thus the number of recompilations is bounded by the number of columns, not
// the number of records. There is no retained cross-batch shape cache.
//
// Most finalized records populate only a fraction of the wide schema. Binding
// every absent value made modernc scan/allocate a wide argument vector on
// every row. Numeric-zero and empty-text literals preserve those exact values;
// ALL other values remain parameters (including arbitrary strings and invalid
// numbers, whose existing SQLite constraint behavior must remain intact).
type batchInsert struct {
	tx    *sql.Tx
	stmt  *sql.Stmt
	bound []bool
}

func (p *batchInsert) close() {
	if p.stmt != nil {
		p.stmt.Close()
	}
}

// exec inserts one record and reports whether it actually created a row. A
// duplicate id is skipped by insertConflict and returns false, so the caller's
// batch totals add exactly the durable rows.
func (p *batchInsert) exec(values []any) (bool, error) {
	if p.bound == nil {
		p.bound = make([]bool, len(values))
	}
	changed := p.stmt == nil
	for i, value := range values {
		if !p.bound[i] && !insertZero(value) {
			p.bound[i] = true
			changed = true
		}
	}
	if changed {
		if p.stmt != nil {
			if err := p.stmt.Close(); err != nil {
				return false, err
			}
			p.stmt = nil
		}
		var query strings.Builder
		query.WriteString(insertColumns)
		query.WriteString(" VALUES (")
		for i, value := range values {
			if i != 0 {
				query.WriteByte(',')
			}
			if p.bound[i] {
				query.WriteByte('?')
			} else if _, ok := value.(string); ok {
				query.WriteString("''")
			} else {
				query.WriteByte('0')
			}
		}
		query.WriteByte(')')
		query.WriteString(insertConflict)
		stmt, err := p.tx.Prepare(query.String())
		if err != nil {
			return false, err
		}
		p.stmt = stmt
	}
	args := values[:0]
	for i, value := range values {
		if p.bound[i] {
			args = append(args, value)
		}
	}
	res, err := p.stmt.Exec(args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func insertZero(value any) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case int:
		return v == 0
	case int64:
		return v == 0
	case float64:
		return v == 0
	default:
		return false
	}
}

func (s *Store) insertBatch(records []*metrics.Record) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insert := batchInsert{tx: tx}
	defer insert.close()
	// Read the base only after Begin: the sole writer connection serializes
	// this batch against merge (which holds it through its own totals install).
	// Only rows this batch actually inserts are added below, so a duplicate
	// id cannot make totals claim a row that does not exist.
	next, degraded := s.totalsBase()

	// One values slice is reused for every row: a fresh 98-element []any per
	// record was the batch's largest avoidable allocation.
	values := make([]any, 0, 100)
	for _, r := range records {
		// Absent optional documents keep the exact stored JSON ("null", or
		// "{}" for a non-nil empty header map) without running the marshaler
		// and allocating a new byte slice per row.
		hdrJSON := jsonNull
		if r.ResponseHeaders != nil {
			if len(r.ResponseHeaders) == 0 {
				hdrJSON = jsonEmptyObject
			} else if b, err := json.Marshal(r.ResponseHeaders); err == nil {
				hdrJSON = string(b)
			} else {
				log.Printf("storage: marshal response headers for %s: %v", r.ID, err)
				hdrJSON = ""
			}
		}
		toolNamesJSON := jsonNull
		if len(r.ToolNames) > 0 {
			if b, err := json.Marshal(r.ToolNames); err == nil {
				toolNamesJSON = string(b)
			} else {
				log.Printf("storage: marshal tool names for %s: %v", r.ID, err)
				toolNamesJSON = ""
			}
		}
		attemptsJSON := jsonNull
		if len(r.Attempts) > 0 {
			if b, err := json.Marshal(r.Attempts); err == nil {
				attemptsJSON = string(b)
			} else {
				log.Printf("storage: marshal attempts for %s: %v", r.ID, err)
				// 'null', never '': the attempts column must hold valid JSON -
				// migrate repairs pre-attempts '' rows for exactly this reason.
				attemptsJSON = jsonNull
			}
		}
		values = appendRecordValues(values[:0], r, hdrJSON, toolNamesJSON, attemptsJSON)
		inserted, err := insert.exec(values)
		if err != nil {
			return err
		}
		if inserted {
			next.Add(r)
		}
	}
	epoch, err := projectionEpoch(context.Background(), tx)
	if err != nil {
		return fmt.Errorf("read projection epoch: %w", err)
	}
	highRowID, err := projectionHighRowID(context.Background(), tx)
	if err != nil {
		return fmt.Errorf("read projection high rowid: %w", err)
	}
	if err := saveTotals(context.Background(), tx, next, degraded, epoch, highRowID); err != nil {
		return fmt.Errorf("persist totals: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.account(records, &next)
	return nil
}

var (
	// Stored TEXT constants; package-level strings avoid one conversion per
	// row for the common no-headers/no-tools/no-attempts case.
	jsonNull        = "null"
	jsonEmptyObject = "{}"
)

// appendRecordValues appends one record's column values in insertColumns
// order to dst. The caller reuses dst across rows.
func appendRecordValues(dst []any, r *metrics.Record, hdrJSON, toolNamesJSON, attemptsJSON string) []any {
	return append(dst,
		r.ID, r.Provider, r.Model, r.KeyHash, r.UserAgent, r.Client, r.ClientIP,
		r.ClientLang, boolToInt(r.Stream),
		r.StatusCode, r.Start.UnixMilli(), r.DurationMs,
		r.TTFTMs, timeToMilli(r.FirstTokenAt), timeToMilli(r.LastTokenAt), r.Chunks,
		r.RequestBytes, r.ResponseBytes, boolToInt(r.UpstreamGzip),
		r.UpstreamConnectMs, r.UpstreamTLSMs, r.UpstreamTTFBMs, boolToInt(r.UpstreamConnReused),
		r.FinishReason, r.ErrorType, r.ErrorMsg, r.ErrorCode, r.ToolCalls,
		r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.TotalTokens,
		r.Usage.CacheReadTokens, r.Usage.CacheWrite, r.Usage.ReasoningTokens,
		boolToInt(r.ClientDisconnected),
		r.RateLimitRemaining, r.RateLimitLimit, r.Cost,
		r.TurnsUser, r.TurnsAssistant, r.TurnsTool, hdrJSON,
		r.AnswerTokens, timeToMilli(r.FirstAnswerAt), r.FirstReasoningAt, r.GenTokens, boolToInt(r.HadAnswerContent), r.DecodeTPS, r.OverallTPS,
		r.Method, r.Path, r.ProcessingMs, r.PromptPreview, r.ResponsePreview,
		r.ProviderModel, r.ProviderRequestID, r.ProviderServer,
		r.QueueWaitMs, boolToInt(r.RateLimited), r.Retries, r.RetryAfterMs,
		toolNamesJSON,
		intPtrVal(r.ReqMaxTokens), floatPtrVal(r.ReqTemperature), floatPtrVal(r.ReqTopP),
		r.ReqToolsCount, r.ReqToolChoice,
		intPtrVal(r.ReqN), r.ReqStop, boolToInt(r.ReqLogprobs),
		floatPtrVal(r.ReqPresencePen), floatPtrVal(r.ReqFrequencyPen),
		r.ReqResponseFormat, int64PtrVal(r.ReqSeed), boolPtrVal(r.ReqParallelTools),
		r.ReqLogitBias, intPtrVal(r.ReqTopLogprobs), r.ReqServiceTier,
		boolToInt(r.ReqThinking), r.ReqMetadataKeys, boolToInt(r.ReqStreamOpts),
		attemptsJSON, timeToMilli(r.FinalAttemptAt), r.ConversationID, r.ParentConversationID,
		r.CharsSystem, r.CharsUser, r.CharsAssistant, r.CharsTool,
		r.Images, r.Attachments, r.LastTurnRole,
		marshalClientMeta(r.ClientMeta), r.ReqReasoningEffort, r.ReqVerbosity,
		boolToInt(r.Debug), r.DebugSessionID, requestParamPresence(r),
	)
}
