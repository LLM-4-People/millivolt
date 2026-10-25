package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

// requestDebugDDL is the single owner of the request_debug schema: the
// fresh-database schema below and migrate's ensure pass for databases that
// predate the table share it byte-for-byte, so the two paths can never drift.
const requestDebugDDL = `CREATE TABLE IF NOT EXISTS request_debug (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL DEFAULT '',
	captured_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	payload TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_request_debug_expires ON request_debug (expires_at);
CREATE INDEX IF NOT EXISTS idx_request_debug_session ON request_debug (session_id);`

// requestLogIndexDDL is the single owner of the ordered log index. migrate
// drops every superseded request-log index before running it, so a legacy
// database ends up with exactly this one secondary log index.
//
// (started_at, id) is the durable log order. Filtered pages and exports still
// apply their exact equality predicates in SQL, but the planner scans this
// index in cursor order and rechecks each predicate per row; equality-only
// counts and purges scan the table. Either way the predicate is unchanged, so
// results, order and pagination stay exact without six parallel
// (dimension, started_at, id) copies whose bytes dominated the durable
// footprint. Only the access path differs.
const requestLogIndexDDL = `CREATE INDEX IF NOT EXISTS idx_requests_log ON requests (started_at, id);`

// supersededRequestLogIndexes is the one list of request-log indexes migrate
// removes. The three legacy spellings predate the ordered composites; the six
// dimension composites are what the previous schema maintained. Their presence
// on an existing database is also the one-shot trigger for reclaiming the pages
// they leave behind, so a fresh or already-migrated database never vacuums.
var supersededRequestLogIndexes = []string{
	"idx_requests_started_at",
	"idx_requests_provider",
	"idx_requests_model",
	"idx_requests_provider_log",
	"idx_requests_model_log",
	"idx_requests_client_log",
	"idx_requests_key_log",
	"idx_requests_conversation_log",
	"idx_requests_status_log",
}

var schema = `
CREATE TABLE IF NOT EXISTS requests (
	id TEXT NOT NULL PRIMARY KEY,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	key_hash TEXT NOT NULL,
	user_agent TEXT NOT NULL,
	client TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	client_lang TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL,
	status_code INTEGER NOT NULL,
	started_at INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	ttft_ms INTEGER NOT NULL,
	first_token_at INTEGER NOT NULL,
	last_token_at INTEGER NOT NULL,
	chunks INTEGER NOT NULL DEFAULT 0,
	request_bytes INTEGER NOT NULL DEFAULT 0,
	response_bytes INTEGER NOT NULL DEFAULT 0,
	upstream_gzip INTEGER NOT NULL DEFAULT 0,
	upstream_connect_ms INTEGER NOT NULL DEFAULT 0,
	upstream_tls_ms INTEGER NOT NULL DEFAULT 0,
	upstream_ttfb_ms INTEGER NOT NULL DEFAULT 0,
	upstream_conn_reused INTEGER NOT NULL DEFAULT 0,
	finish_reason TEXT NOT NULL,
	error_type TEXT NOT NULL,
	error_msg TEXT NOT NULL,
	error_code TEXT NOT NULL,
	tool_calls INTEGER NOT NULL,
	input_tokens INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL,
	total_tokens INTEGER NOT NULL,
	cache_read_tokens INTEGER NOT NULL,
	cache_write_tokens INTEGER NOT NULL,
	reasoning_tokens INTEGER NOT NULL,
	client_disconnected INTEGER NOT NULL,
	rate_limit_remaining INTEGER NOT NULL,
	rate_limit_limit INTEGER NOT NULL,
	cost REAL NOT NULL,
	turns_user INTEGER NOT NULL,
	turns_assistant INTEGER NOT NULL,
	turns_tool INTEGER NOT NULL,
	response_headers TEXT NOT NULL,
	answer_tokens INTEGER NOT NULL DEFAULT 0,
	first_answer_at INTEGER NOT NULL DEFAULT 0,
	first_reasoning_at INTEGER NOT NULL DEFAULT 0,
	gen_tokens INTEGER NOT NULL DEFAULT 0,
	had_answer_content INTEGER NOT NULL DEFAULT 0,
	gen_tps REAL NOT NULL DEFAULT 0,
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	processing_ms INTEGER NOT NULL DEFAULT 0,
	prompt_preview TEXT NOT NULL DEFAULT '',
	response_preview TEXT NOT NULL DEFAULT '',
	provider_model TEXT NOT NULL DEFAULT '',
	provider_request_id TEXT NOT NULL DEFAULT '',
	provider_server TEXT NOT NULL DEFAULT '',
	queue_wait_ms INTEGER NOT NULL DEFAULT 0,
	rate_limited INTEGER NOT NULL DEFAULT 0,
	retries INTEGER NOT NULL DEFAULT 0,
	retry_after_ms INTEGER NOT NULL DEFAULT 0,
	tool_names TEXT NOT NULL DEFAULT 'null',
	req_max_tokens INTEGER NOT NULL DEFAULT 0,
	req_temperature REAL NOT NULL DEFAULT 0,
	req_top_p REAL NOT NULL DEFAULT 0,
	req_tools_count INTEGER NOT NULL DEFAULT 0,
	req_tool_choice TEXT NOT NULL DEFAULT '',
	req_n INTEGER NOT NULL DEFAULT 0,
	req_stop INTEGER NOT NULL DEFAULT 0,
	req_logprobs INTEGER NOT NULL DEFAULT 0,
	req_presence_pen REAL NOT NULL DEFAULT 0,
	req_frequency_pen REAL NOT NULL DEFAULT 0,
	req_response_format TEXT NOT NULL DEFAULT '',
	req_seed INTEGER NOT NULL DEFAULT 0,
	req_parallel_tools INTEGER NOT NULL DEFAULT 0,
	req_logit_bias INTEGER NOT NULL DEFAULT 0,
	req_top_logprobs INTEGER NOT NULL DEFAULT 0,
	req_service_tier TEXT NOT NULL DEFAULT '',
	req_thinking INTEGER NOT NULL DEFAULT 0,
	req_metadata_keys INTEGER NOT NULL DEFAULT 0,
	req_stream_opts INTEGER NOT NULL DEFAULT 0,
	attempts TEXT NOT NULL DEFAULT 'null',
	final_attempt_at INTEGER NOT NULL DEFAULT 0,
	conversation_id TEXT NOT NULL DEFAULT '',
	parent_conversation_id TEXT NOT NULL DEFAULT '',
	chars_system INTEGER NOT NULL DEFAULT 0,
	chars_user INTEGER NOT NULL DEFAULT 0,
	chars_assistant INTEGER NOT NULL DEFAULT 0,
	chars_tool INTEGER NOT NULL DEFAULT 0,
	images INTEGER NOT NULL DEFAULT 0,
	attachments INTEGER NOT NULL DEFAULT 0,
	last_turn_role TEXT NOT NULL DEFAULT '',
	client_meta TEXT NOT NULL DEFAULT '',
	req_reasoning_effort TEXT NOT NULL DEFAULT '',
	req_verbosity TEXT NOT NULL DEFAULT '',
	debug INTEGER NOT NULL DEFAULT 0,
	debug_session_id TEXT NOT NULL DEFAULT '',
	` + requestParamPresenceDDL + `
);
	CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
` + requestDebugDDL + projectionSchema()

// One persistent mutation boundary covers every writer, including a draining
// older process during restart handoff. Ordinary inserts advance the rowid
// high-water mark; rewrites/removals invalidate decoded analytical history.
// BEFORE INSERT catches REPLACE, whose implicit delete need not fire a DELETE
// trigger. Explicit rowid reuse is detected by the AFTER INSERT high-water test.
type projectionTrigger struct{ name, definition string }

func (tr projectionTrigger) sql() string { return "CREATE TRIGGER " + tr.name + " " + tr.definition }

var projectionTriggers = []projectionTrigger{
	{"requests_projection_replace", `BEFORE INSERT ON requests
		WHEN EXISTS (SELECT 1 FROM requests WHERE id = NEW.id) BEGIN
		UPDATE request_projection_state SET epoch = epoch + 1 WHERE singleton = 1; END`},
	{"requests_projection_insert", `AFTER INSERT ON requests BEGIN
		UPDATE request_projection_state SET
		epoch = epoch + (NEW.rowid <= high_rowid),
		high_rowid = MAX(high_rowid, NEW.rowid) WHERE singleton = 1; END`},
	{"requests_projection_update", `AFTER UPDATE ON requests BEGIN
		UPDATE request_projection_state SET epoch = epoch + 1,
		high_rowid = CASE WHEN OLD.rowid = high_rowid AND NEW.rowid < OLD.rowid
		THEN MAX(0, COALESCE((SELECT MAX(rowid) FROM requests), 0))
		ELSE MAX(high_rowid, NEW.rowid) END WHERE singleton = 1; END`},
	{"requests_projection_delete", `AFTER DELETE ON requests BEGIN
		UPDATE request_projection_state SET epoch = epoch + 1,
		high_rowid = CASE WHEN OLD.rowid = high_rowid
		THEN MAX(0, COALESCE((SELECT MAX(rowid) FROM requests), 0))
		ELSE high_rowid END WHERE singleton = 1; END`},
}

func projectionSchema() string {
	var ddl strings.Builder
	ddl.WriteString(`CREATE TABLE IF NOT EXISTS request_projection_state (
		singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
		epoch INTEGER NOT NULL,
		high_rowid INTEGER NOT NULL
	);
	INSERT OR IGNORE INTO request_projection_state (singleton, epoch, high_rowid)
	SELECT 1, 0, MAX(0, COALESCE(MAX(rowid), 0)) FROM requests;
	`)
	for _, tr := range projectionTriggers {
		fmt.Fprintf(&ddl, "CREATE TRIGGER IF NOT EXISTS %s %s;\n", tr.name, tr.definition)
	}
	return ddl.String()
}

// Startup reconciles owned trigger definitions transactionally. IF NOT EXISTS
// alone would retain an older binary's definitions forever after an upgrade.
// Replacing a definition invalidates all existing decoded projections at the
// same commit; running readers still fail closed on unexpected schema edits.
func ensureProjectionTriggers(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := false
	for _, tr := range projectionTriggers {
		var definition string
		err := tx.QueryRow("SELECT sql FROM sqlite_schema WHERE type = 'trigger' AND name = ? AND tbl_name = 'requests'", tr.name).Scan(&definition)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if definition == tr.sql() {
			continue
		}
		if _, err := tx.Exec("DROP TRIGGER IF EXISTS " + tr.name); err != nil {
			return err
		}
		if _, err := tx.Exec(tr.sql()); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if _, err := tx.Exec("UPDATE request_projection_state SET epoch = epoch + 1, high_rowid = MAX(0, COALESCE((SELECT MAX(rowid) FROM requests), 0)) WHERE singleton = 1"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrate brings an existing requests table up to the current schema: it adds
// any missing columns (forward-compatible, never drops history), removes
// superseded indexes and repairs data that violates a column invariant older
// binaries could write. It owns the invariant "attempts holds valid JSON" -
// the HasError purge predicate runs json_each over that column, and SQLite
// raises "malformed JSON" on any non-JSON value, denying the errors-only
// purge/count/export outright.
func migrate(db *sql.DB) error {
	// idx_requests_log(started_at, id) is the only request-log index. Remove
	// every superseded spelling once: the timestamp-only and single-column
	// provider/model predecessors, plus the six per-dimension composites this
	// schema used to maintain. Their equality predicates remain in every
	// page/count/export/purge statement: pages and exports recheck them while
	// scanning the ordered index, equality-only counts and purges scan the
	// table, so results never change; only the access path does. Creation is
	// owned by requestLogIndexDDL below, after the column pass, because some
	// legacy columns are added by ALTER TABLE. DROP IF EXISTS keeps every
	// re-run harmless and never touches idx_requests_log.
	reclaim := false
	for _, index := range supersededRequestLogIndexes {
		var exists int
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema
			WHERE type = 'index' AND tbl_name = 'requests' AND name = ?)`, index).Scan(&exists); err != nil {
			return fmt.Errorf("inspect superseded index %s: %w", index, err)
		}
		reclaim = reclaim || exists == 1
		if _, err := db.Exec("DROP INDEX IF EXISTS " + index); err != nil {
			return err
		}
	}
	if _, err := db.Exec(requestDebugDDL); err != nil {
		return fmt.Errorf("request_debug: %w", err)
	}
	cols, err := tableColumnSet(context.Background(), db, "", "requests")
	if err != nil {
		return err
	}
	needed := []struct{ name, ddl string }{
		{"error_code", "ALTER TABLE requests ADD COLUMN error_code TEXT NOT NULL DEFAULT ''"},
		{"rate_limit_remaining", "ALTER TABLE requests ADD COLUMN rate_limit_remaining INTEGER NOT NULL DEFAULT 0"},
		{"rate_limit_limit", "ALTER TABLE requests ADD COLUMN rate_limit_limit INTEGER NOT NULL DEFAULT 0"},
		{"cost", "ALTER TABLE requests ADD COLUMN cost REAL NOT NULL DEFAULT 0"},
		{"turns_user", "ALTER TABLE requests ADD COLUMN turns_user INTEGER NOT NULL DEFAULT 0"},
		{"turns_assistant", "ALTER TABLE requests ADD COLUMN turns_assistant INTEGER NOT NULL DEFAULT 0"},
		{"turns_tool", "ALTER TABLE requests ADD COLUMN turns_tool INTEGER NOT NULL DEFAULT 0"},
		{"response_headers", "ALTER TABLE requests ADD COLUMN response_headers TEXT NOT NULL DEFAULT ''"},
		{"answer_tokens", "ALTER TABLE requests ADD COLUMN answer_tokens INTEGER NOT NULL DEFAULT 0"},
		{"client", "ALTER TABLE requests ADD COLUMN client TEXT NOT NULL DEFAULT ''"},
		{"client_ip", "ALTER TABLE requests ADD COLUMN client_ip TEXT NOT NULL DEFAULT ''"},
		{"client_lang", "ALTER TABLE requests ADD COLUMN client_lang TEXT NOT NULL DEFAULT ''"},
		{"first_answer_at", "ALTER TABLE requests ADD COLUMN first_answer_at INTEGER NOT NULL DEFAULT 0"},
		{"gen_tokens", "ALTER TABLE requests ADD COLUMN gen_tokens INTEGER NOT NULL DEFAULT 0"},
		{"had_answer_content", "ALTER TABLE requests ADD COLUMN had_answer_content INTEGER NOT NULL DEFAULT 0"},
		{"gen_tps", "ALTER TABLE requests ADD COLUMN gen_tps REAL NOT NULL DEFAULT 0"},
		{"method", "ALTER TABLE requests ADD COLUMN method TEXT NOT NULL DEFAULT ''"},
		{"path", "ALTER TABLE requests ADD COLUMN path TEXT NOT NULL DEFAULT ''"},
		{"processing_ms", "ALTER TABLE requests ADD COLUMN processing_ms INTEGER NOT NULL DEFAULT 0"},
		{"prompt_preview", "ALTER TABLE requests ADD COLUMN prompt_preview TEXT NOT NULL DEFAULT ''"},
		{"response_preview", "ALTER TABLE requests ADD COLUMN response_preview TEXT NOT NULL DEFAULT ''"},
		{"provider_model", "ALTER TABLE requests ADD COLUMN provider_model TEXT NOT NULL DEFAULT ''"},
		{"provider_request_id", "ALTER TABLE requests ADD COLUMN provider_request_id TEXT NOT NULL DEFAULT ''"},
		{"provider_server", "ALTER TABLE requests ADD COLUMN provider_server TEXT NOT NULL DEFAULT ''"},
		{"queue_wait_ms", "ALTER TABLE requests ADD COLUMN queue_wait_ms INTEGER NOT NULL DEFAULT 0"},
		{"rate_limited", "ALTER TABLE requests ADD COLUMN rate_limited INTEGER NOT NULL DEFAULT 0"},
		{"retries", "ALTER TABLE requests ADD COLUMN retries INTEGER NOT NULL DEFAULT 0"},
		{"retry_after_ms", "ALTER TABLE requests ADD COLUMN retry_after_ms INTEGER NOT NULL DEFAULT 0"},
		{"tool_names", "ALTER TABLE requests ADD COLUMN tool_names TEXT NOT NULL DEFAULT 'null'"},
		{"req_max_tokens", "ALTER TABLE requests ADD COLUMN req_max_tokens INTEGER NOT NULL DEFAULT 0"},
		{"req_temperature", "ALTER TABLE requests ADD COLUMN req_temperature REAL NOT NULL DEFAULT 0"},
		{"req_top_p", "ALTER TABLE requests ADD COLUMN req_top_p REAL NOT NULL DEFAULT 0"},
		{"req_tools_count", "ALTER TABLE requests ADD COLUMN req_tools_count INTEGER NOT NULL DEFAULT 0"},
		{"req_tool_choice", "ALTER TABLE requests ADD COLUMN req_tool_choice TEXT NOT NULL DEFAULT ''"},
		{"req_n", "ALTER TABLE requests ADD COLUMN req_n INTEGER NOT NULL DEFAULT 0"},
		{"req_stop", "ALTER TABLE requests ADD COLUMN req_stop INTEGER NOT NULL DEFAULT 0"},
		{"req_logprobs", "ALTER TABLE requests ADD COLUMN req_logprobs INTEGER NOT NULL DEFAULT 0"},
		{"req_presence_pen", "ALTER TABLE requests ADD COLUMN req_presence_pen REAL NOT NULL DEFAULT 0"},
		{"req_frequency_pen", "ALTER TABLE requests ADD COLUMN req_frequency_pen REAL NOT NULL DEFAULT 0"},
		{"req_response_format", "ALTER TABLE requests ADD COLUMN req_response_format TEXT NOT NULL DEFAULT ''"},
		{"req_seed", "ALTER TABLE requests ADD COLUMN req_seed INTEGER NOT NULL DEFAULT 0"},
		{"req_parallel_tools", "ALTER TABLE requests ADD COLUMN req_parallel_tools INTEGER NOT NULL DEFAULT 0"},
		{"req_logit_bias", "ALTER TABLE requests ADD COLUMN req_logit_bias INTEGER NOT NULL DEFAULT 0"},
		{"req_top_logprobs", "ALTER TABLE requests ADD COLUMN req_top_logprobs INTEGER NOT NULL DEFAULT 0"},
		{"req_service_tier", "ALTER TABLE requests ADD COLUMN req_service_tier TEXT NOT NULL DEFAULT ''"},
		{"req_thinking", "ALTER TABLE requests ADD COLUMN req_thinking INTEGER NOT NULL DEFAULT 0"},
		{"req_metadata_keys", "ALTER TABLE requests ADD COLUMN req_metadata_keys INTEGER NOT NULL DEFAULT 0"},
		{"req_stream_opts", "ALTER TABLE requests ADD COLUMN req_stream_opts INTEGER NOT NULL DEFAULT 0"},
		{"attempts", "ALTER TABLE requests ADD COLUMN attempts TEXT NOT NULL DEFAULT 'null'"},
		{"final_attempt_at", "ALTER TABLE requests ADD COLUMN final_attempt_at INTEGER NOT NULL DEFAULT 0"},
		{"conversation_id", "ALTER TABLE requests ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''"},
		{"parent_conversation_id", "ALTER TABLE requests ADD COLUMN parent_conversation_id TEXT NOT NULL DEFAULT ''"},
		{"chars_system", "ALTER TABLE requests ADD COLUMN chars_system INTEGER NOT NULL DEFAULT 0"},
		{"chars_user", "ALTER TABLE requests ADD COLUMN chars_user INTEGER NOT NULL DEFAULT 0"},
		{"chars_assistant", "ALTER TABLE requests ADD COLUMN chars_assistant INTEGER NOT NULL DEFAULT 0"},
		{"chars_tool", "ALTER TABLE requests ADD COLUMN chars_tool INTEGER NOT NULL DEFAULT 0"},
		{"images", "ALTER TABLE requests ADD COLUMN images INTEGER NOT NULL DEFAULT 0"},
		{"attachments", "ALTER TABLE requests ADD COLUMN attachments INTEGER NOT NULL DEFAULT 0"},
		{"last_turn_role", "ALTER TABLE requests ADD COLUMN last_turn_role TEXT NOT NULL DEFAULT ''"},
		{"client_meta", "ALTER TABLE requests ADD COLUMN client_meta TEXT NOT NULL DEFAULT ''"},
		{"req_reasoning_effort", "ALTER TABLE requests ADD COLUMN req_reasoning_effort TEXT NOT NULL DEFAULT ''"},
		{"req_verbosity", "ALTER TABLE requests ADD COLUMN req_verbosity TEXT NOT NULL DEFAULT ''"},
		{"debug", "ALTER TABLE requests ADD COLUMN debug INTEGER NOT NULL DEFAULT 0"},
		{"debug_session_id", "ALTER TABLE requests ADD COLUMN debug_session_id TEXT NOT NULL DEFAULT ''"},
		{"req_param_presence", "ALTER TABLE requests ADD COLUMN " + requestParamPresenceDDL},
		{"chunks", "ALTER TABLE requests ADD COLUMN chunks INTEGER NOT NULL DEFAULT 0"},
		{"first_reasoning_at", "ALTER TABLE requests ADD COLUMN first_reasoning_at INTEGER NOT NULL DEFAULT 0"},
		{"request_bytes", "ALTER TABLE requests ADD COLUMN request_bytes INTEGER NOT NULL DEFAULT 0"},
		{"response_bytes", "ALTER TABLE requests ADD COLUMN response_bytes INTEGER NOT NULL DEFAULT 0"},
		{"upstream_gzip", "ALTER TABLE requests ADD COLUMN upstream_gzip INTEGER NOT NULL DEFAULT 0"},
		{"upstream_connect_ms", "ALTER TABLE requests ADD COLUMN upstream_connect_ms INTEGER NOT NULL DEFAULT 0"},
		{"upstream_tls_ms", "ALTER TABLE requests ADD COLUMN upstream_tls_ms INTEGER NOT NULL DEFAULT 0"},
		{"upstream_ttfb_ms", "ALTER TABLE requests ADD COLUMN upstream_ttfb_ms INTEGER NOT NULL DEFAULT 0"},
		{"upstream_conn_reused", "ALTER TABLE requests ADD COLUMN upstream_conn_reused INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range needed {
		if _, ok := cols[c.name]; !ok {
			if _, err := db.Exec(c.ddl); err != nil {
				return fmt.Errorf("add column %s: %w", c.name, err)
			}
		}
	}
	// Data repair for databases migrated before this invariant existed: the
	// original attempts ADD COLUMN defaulted to '' - not valid JSON - so
	// pre-attempts rows still hold '' and the column default in those
	// databases stays ''. 'null' is the writer's own encoding of "no
	// attempts" (json.Marshal of a nil slice), and scanRecord decodes it to
	// nil exactly like an empty string, so this is a pure re-encoding.
	// The EXISTS probe has no index to lean on - it is a full-table scan -
	// so it is one-shot via the meta table both paths already share: the
	// marker is absent on the first boot and on any backup predating it,
	// and is written after a clean probe or after the repair itself. Every
	// later boot reads one meta row instead of rescanning the table.
	var repaired string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, attemptsRepairMetaKey).Scan(&repaired); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("repair attempts marker: %w", err)
	}
	if repaired == "" {
		var brokenAttempts int
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM requests WHERE attempts = '')`).Scan(&brokenAttempts); err != nil {
			return fmt.Errorf("repair attempts: %w", err)
		}
		if brokenAttempts != 0 {
			if _, err := db.Exec(`UPDATE requests SET attempts = 'null' WHERE attempts = ''`); err != nil {
				return fmt.Errorf("repair attempts: %w", err)
			}
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, attemptsRepairMetaKey); err != nil {
			return fmt.Errorf("repair attempts marker: %w", err)
		}
	}
	// Same one-shot repair for tool_names: the original ADD COLUMN defaulted to
	// '' (not valid JSON), so a database migrated from before the column keeps
	// that spelling even though every writer path stores 'null' or an array.
	// Decode treats both as no names, but raw json_each consumers such as the
	// MCP values(dim:"tool") probe require valid JSON, so canonicalize once.
	var repairedToolNames string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, toolNamesRepairMetaKey).Scan(&repairedToolNames); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("repair tool_names marker: %w", err)
	}
	if repairedToolNames == "" {
		var brokenToolNames int
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM requests WHERE tool_names = '')`).Scan(&brokenToolNames); err != nil {
			return fmt.Errorf("repair tool_names: %w", err)
		}
		if brokenToolNames != 0 {
			if _, err := db.Exec(`UPDATE requests SET tool_names = 'null' WHERE tool_names = ''`); err != nil {
				return fmt.Errorf("repair tool_names: %w", err)
			}
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, toolNamesRepairMetaKey); err != nil {
			return fmt.Errorf("repair tool_names marker: %w", err)
		}
	}
	// Every leading equality column now exists, whether this database was just
	// created with the full schema or upgraded by the ALTER pass above. Build
	// the ordered log indexes once; IF NOT EXISTS keeps later boots cheap.
	if _, err := db.Exec(requestLogIndexDDL); err != nil {
		return fmt.Errorf("log indexes: %w", err)
	}
	if reclaim {
		reclaimIndexPages(db)
	}
	return nil
}

// reclaimIndexPages repacks the database after migrate dropped superseded
// request-log indexes. Dropping an index moves its pages to SQLite's freelist;
// without a rebuild the file keeps that space indefinitely. migrate calls this
// only when it actually found one of those indexes, so a fresh or
// already-migrated database never vacuums, and a failed reclaim is never
// retried on a later boot.
//
// VACUUM cannot run inside a transaction and needs temporary space up to twice
// the database size. A failure is deliberately non-fatal: the dropped indexes
// are already gone, every request row and aggregate is intact, and the operator
// can reclaim the freelist manually (see docs/operations.md). The connection
// comes from the same DSN pool, so WAL mode, busy_timeout, synchronous and the
// other connection pragmas remain in force.
func reclaimIndexPages(db *sql.DB) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Printf("storage: reclaim freed index pages: acquire connection: %v (request rows are intact; run VACUUM manually to reclaim disk)", err)
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		log.Printf("storage: reclaim freed index pages: VACUUM failed: %v (request rows are intact; run VACUUM manually to reclaim disk)", err)
		return
	}
	// VACUUM may renumber implicit rowids on a table whose primary key is not
	// INTEGER (requests keys on TEXT id). Reset the append-only projection
	// witness so persisted totals are not blessed against a stale rowid
	// watermark; the next preload rebuilds analytical history from rows.
	if _, err := conn.ExecContext(ctx, `UPDATE request_projection_state
		SET epoch = epoch + 1,
			high_rowid = MAX(0, COALESCE((SELECT MAX(rowid) FROM requests), 0))
		WHERE singleton = 1`); err != nil {
		log.Printf("storage: reclaim freed index pages: reset analytical projection: %v (request rows are intact; aggregates will rebuild)", err)
	}
}

// tableColumnSet reads one table's column-name set through PRAGMA table_info,
// delegating the read to backup.TableColumnSet (the shared reader; backup's
// own snapshot inspector reads through it too). schema may name an ATTACHed
// backup schema ("" reads the main database); the caller owns the allowlist
// guards on both arguments before calling, because the query is built by
// concatenation (migrate passes its own table literal; the snapshot paths
// reject unknown schemas and tables first).
func tableColumnSet(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, table string) (map[string]struct{}, error) {
	return backup.TableColumnSet(ctx, q, schema, table)
}
