// Package storage provides durable per-request storage using an embedded
// SQLite database. Records flow in asynchronously from the hot path via a
// channel, so DB writes never block the proxy.
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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// sqliteWriterConns is the SQLite single-writer pool size. Safety
// guardrail (avoids SQLITE_BUSY), not user-tunable.
const sqliteWriterConns = 1

// sqliteReadConns bounds the read-only pool. Each connection owns a SQLite
// page cache, so an unbounded pool multiplies memory under dashboard/MCP
// concurrency; reads beyond this bound queue behind the query timeout.
// Internal guardrail, not user-tunable.
const sqliteReadConns = 9

// Ad-hoc SELECTs cannot materialize thousands of independent large columns
// before the result-byte check. Internal safety guardrail, not user-tunable;
// the canonical requests schema (including SELECT *) fits below this ceiling.
const queryMaxColumns = 128

// sqliteBusyTimeout bounds how long every SQLite connection waits on a
// contended write lock before failing with SQLITE_BUSY. It closes the
// restart-handoff straggler window: a drain overrun is tolerated (the
// draining parent keeps serving straggler streams), so the parent can keep
// committing batches after its flush - exactly while the fresh child's Open
// runs its schema/migrate writes on the same file. With no busy timeout the
// second writer fails immediately: the child's Open errors into a spurious
// failAndResume abort, or the parent's insertBatch drops the batch. Five
// seconds absorbs any ms-scale straggler commit; a lock held longer still
// fails closed (child Open fails -> failAndResume; dropped batches stay
// counted in Dropped). Internal guardrail, not user-tunable.
const sqliteBusyTimeout = 5 * time.Second

// exportTimeout bounds one durable log export end to end. An export holds an
// open read transaction for the whole stream, so an abandoned or trickling
// consumer would otherwise pin the WAL snapshot indefinitely. The budget is
// internal (not an operator tunable) and deliberately equal to the
// maintenance budget (both 10 minutes): both bound one full-history storage
// operation, and an export is a delivery path, not a rebuild, so it is not
// given a longer ceiling than maintenance. The interactive query timeout is a
// separate bound for dashboard SELECTs.
const exportTimeout = 10 * time.Minute

// maintenanceTimeout bounds long, non-interactive storage materialization and
// maintenance work: the startup projection preload, the boot totals rebuild,
// an administrative purge, and a backup merge's totals scan. These operations
// legitimately read or rewrite full history, so the interactive
// storage_query_timeout must not abort them. A caller's earlier deadline still
// wins (context.WithTimeout keeps the shorter one). Internal guardrail, not a
// user-tunable config value.
const maintenanceTimeout = 10 * time.Minute

// Options holds the storage pipeline tunables. All values come from config
// (single source of truth); nothing is defaulted here.
type Options struct {
	// WriteChanCap bounds records buffered between the hot path and the
	// background writer; overflow drops records to protect the proxy.
	WriteChanCap int
	// BatchCap is the max number of records flushed per transaction.
	BatchCap int
	// FlushInterval is how often the writer flushes a partial batch.
	FlushInterval time.Duration
	// QueryTimeout bounds ad-hoc dashboard SELECT queries so a slow query
	// cannot hold a read connection indefinitely. Validate requires > 0;
	// withQueryTimeout is the single choke point (a 0 fail-closes).
	QueryTimeout time.Duration
	// QueryMaxBytes/Rows bound ad-hoc SELECT results, not analytical scans.
	// Both are required config values; zero fails closed, never defaults here.
	QueryMaxBytes int
	QueryMaxRows  int
	// WriteTrackCap bounds the in-memory set of recently written record ids.
	// Dashboard aggregates merge un-flushed ring records against stored rows;
	// the written set is the exact dedupe boundary. Sized 2x the ring
	// (history_size) so it always covers every record the ring can hold.
	WriteTrackCap int
	// DebugMaxBytes caps the durable debug-capture sidecar; new captures are
	// refused once the next write would exceed it. Zero disables the cap.
	DebugMaxBytes int64
	// BusyTimeout shortens the sqliteBusyTimeout guardrail for tests that
	// assert the contention window quickly (a 5s window costs 5s of wall
	// time per phase). Zero keeps the production constant; config has no
	// knob because the guardrail is deliberately not user-tunable.
	BusyTimeout time.Duration
	// MaintenanceTimeout shortens the maintenanceTimeout guardrail for tests
	// that assert the boot/maintenance failure path quickly (a 10m window is
	// not testable). Zero keeps the production constant; config has no knob
	// because the budget is an internal guardrail, not user-tunable.
	MaintenanceTimeout time.Duration
	// ExportTimeout shortens the exportTimeout guardrail for tests that assert
	// the bounded export failure path quickly. Zero keeps the production
	// constant; config has no knob because the budget is an internal guardrail,
	// not user-tunable.
	ExportTimeout time.Duration
}

// errNotSelect rejects any dashboard query that is not a single SELECT.
var errNotSelect = errors.New("only a single SELECT statement is allowed")

// ErrQueryLimit rejects the entire result; callers must never serve a prefix.
var ErrQueryLimit = errors.New("query result exceeds its byte, row, or column limit")

// ErrStorageDisabled is the one sentinel answering "no durable store is
// available" (a nil *Store, or no db_path): every surface that refuses an
// operation for that reason returns or wraps it, so callers classify with
// errors.Is instead of matching drifted wordings. Each surface keeps its own
// status code; only the identity and the canonical message are shared.
var ErrStorageDisabled = errors.New("durable storage is disabled")

// ErrExportTimeout marks a log export that hit its own total budget. It is
// distinct from a caller cancellation or caller deadline so the HTTP layer and
// tests can tell "the server abandoned this export" from "the client went
// away". After the sentinel, the response may already contain a partial
// artifact; see ExportWhere.
var ErrExportTimeout = errors.New("log export exceeded its time budget")

// withQueryTimeout is the single choke point that bounds a dashboard SELECT
// by QueryTimeout. Validate requires QueryTimeout > 0; a 0 fail-closes
// (deadline already expired) rather than running unbounded.
func (s *Store) withQueryTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opts.QueryTimeout)
}

// withMaintenanceTimeout is the explicit budget for non-interactive
// materialization and maintenance work. Interactive dashboard queries stay on
// withQueryTimeout; callers select this path by method, never by widening the
// interactive timeout. A shorter parent deadline is preserved.
func (s *Store) withMaintenanceTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.opts.MaintenanceTimeout
	if timeout <= 0 {
		timeout = maintenanceTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// withExportTimeout bounds one export with the internal exportTimeout guardrail
// (or the test seam value). A shorter caller deadline is preserved by
// context.WithTimeout, and the caller's own cancellation still propagates.
func (s *Store) withExportTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.opts.ExportTimeout
	if timeout <= 0 {
		timeout = exportTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

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

// Totals is the since-inception cross-request aggregate the dashboard's KPI
// surface reads. The single writer goroutine maintains it exactly: only rows
// that have been durably committed are counted, so a dashboard aggregate that
// merges totals with the un-flushed ring records (deduped via the written-id
// set) matches the durable history byte-for-byte with no flush-lag window.
// IsError semantics come from the canonical metrics.Record.IsError.
type Totals struct {
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Cost      float64 `json:"cost"`
	InputTok  int64   `json:"input_tokens"`
	OutputTok int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	Reasoning int64   `json:"reasoning_tokens"`
	TTFTSum   int64   `json:"-"`
	TTFTN     int64   `json:"-"`
	TPSSum    float64 `json:"-"`
	TPSN      int64   `json:"-"`
	// Cost-bearing denominators for the KPI's blended unit economics: the
	// count of requests that reported cost > 0 and their in+out tokens.
	// Blended $/req and $/Mtok divide by these, so an unpriced provider
	// (no cost in its response → 0) can never dilute the rate.
	CostReqs  int64   `json:"-"`
	CostInOut float64 `json:"-"` // ratios are floating point; in+out need not fit int64
	err       error
}

// Err reports invalid arithmetic without exposing partial or wrapped totals.
func (t Totals) Err() error { return t.err }

// AvgTTFT returns the mean over captured TTFT samples (ttft_ms > 0), or null
// when no sample exists - mirrors the dashboard's "- not 0" rule.
func (t Totals) AvgTTFT() *float64 {
	if t.TTFTN == 0 {
		return nil
	}
	return metrics.ScaledRatio(float64(t.TTFTSum), float64(t.TTFTN), 1)
}

// AvgTPS returns the mean over captured generation-speed samples (tps > 0).
func (t Totals) AvgTPS() *float64 {
	if t.TPSN == 0 {
		return nil
	}
	return metrics.ScaledRatio(t.TPSSum, float64(t.TPSN), 1)
}

// Add is the shared durable/ring accounting owner. Arithmetic failure stays
// attached to the aggregate until a rebuild, never masquerading as zero.
func (t *Totals) Add(r *metrics.Record) {
	if r == nil || t.err != nil {
		return
	}
	t.Requests++
	if r.IsError() {
		t.Errors++
	}
	// Contributions are admitted per record and every step's overflow is
	// checked, so the durable accumulator is a checked sum by induction and is
	// never re-validated here. t.err keeps the fail-closed latch.
	if err := metrics.AddValue(&t.Cost, r.Cost); err != nil {
		t.err = err
		return
	}
	// Only cost-reporting requests feed the blended denominators - a 0-cost
	// request (provider sent no price) adds tokens but must not drag the
	// blended rate down.
	if r.Cost > 0 {
		t.CostReqs++
		if err := metrics.AddValue(&t.CostInOut, float64(r.Usage.InputTokens)); err != nil {
			t.err = err
			return
		}
		if err := metrics.AddValue(&t.CostInOut, float64(r.Usage.OutputTokens)); err != nil {
			t.err = err
			return
		}
	}
	if err := metrics.AddCount(&t.InputTok, r.Usage.InputTokens); err != nil {
		t.err = err
		return
	}
	if err := metrics.AddCount(&t.OutputTok, r.Usage.OutputTokens); err != nil {
		t.err = err
		return
	}
	if err := metrics.AddCount(&t.CacheRead, r.Usage.CacheReadTokens); err != nil {
		t.err = err
		return
	}
	if err := metrics.AddCount(&t.Reasoning, r.Usage.ReasoningTokens); err != nil {
		t.err = err
		return
	}
	// TTFTMs == 0 means "absent", not "0 ms" - no-token requests must not
	// dilute the mean (same gate as the client's truthy r.ttft_ms).
	if r.TTFTMs > 0 {
		if err := metrics.AddCount(&t.TTFTSum, r.TTFTMs); err != nil {
			t.err = err
			return
		}
		t.TTFTN++
	}
	if tps := r.DecodeTPS; tps > 0 {
		if err := metrics.AddValue(&t.TPSSum, tps); err != nil {
			t.err = err
			return
		}
		t.TPSN++
	}
}

// Store is a durable SQLite-backed store. It is safe for concurrent use.
type Store struct {
	// path is the live database file. Its directory owns the capacity for
	// database-sized transient staging (backup/restore); see stageDir.
	path       string
	db         *sql.DB // writer pool: single connection, serialized
	rdb        *sql.DB // read pool: serves dashboard queries without starving the writer
	ch         chan queuedRecord
	opts       Options
	wg         sync.WaitGroup
	done       chan struct{}
	writerDone chan struct{} // closed only after final drain and its error publication
	writerErr  error         // read only after writerDone closes
	once       sync.Once

	// data_version is connection-local, so aggregate cache validation keeps
	// one read-only connection for its lifetime. It observes commits from the
	// writer pool and a draining parent process after a restart handoff.
	// Lazily acquired by readers; never touched by the proxy/write hot path.
	versionMu     sync.Mutex
	versionConn   *sql.Conn
	versionClosed bool

	// sendMu guards the ch send in Record against the channel close in Close,
	// so a producer can never send on a closed channel (the close and the
	// check-then-send are mutually exclusive). shut reports Close has run.
	sendMu       sync.Mutex
	shut         bool
	accepted     uint64
	purgeMu      sync.Mutex                   // admin serialization only; never acquired by Record
	pendingPurge atomic.Pointer[purgeCommand] // one command owner; publication under sendMu
	purgeWake    chan struct{}

	drops atomic.Uint64
	// debug sidecar accounting: the writer connection serializes physical
	// writes, debugMu makes the byte counter and the SQL that changes it one
	// atomic pair across request-goroutine saves and the writer's expiry.
	debugMu       sync.Mutex
	debugBytes    int64
	debugSeeded   bool
	debugMaxBytes int64

	// aggMu guards the written-id set and the totals snapshot. The writer
	// updates both under this lock AFTER a batch commits, and dashboard
	// aggregators read both under the same lock, so a ring record is either
	// "already durably counted" (skipped in the merge) or not - never both,
	// never neither.
	aggMu         sync.Mutex
	written       map[string]struct{}
	writtenOrder  []string
	writtenCap    int
	totals        Totals
	totalsBuilt   bool
	totalsWitness totalsWitness
	// accountingRevision is the exact witness for the durable KPI fold's memo
	// key. It advances under aggMu in the same critical section that publishes
	// totals or written-id changes, so before/after key reads can prove a fold
	// observed one coherent durable state without taking the accounting lock.
	// Its scope matches the in-memory totals snapshot: in-process accounting
	// only (unsupported external writers are already outside that contract).
	accountingRevision atomic.Uint64
	// totalsDegraded reports that the boot scan failed and totals count
	// only rows committed since (account() rebuilds from zero on the first
	// post-boot commit). The dashboard surfaces the flag so the subset is
	// never presented as since-inception; a later successful scan clears it.
	totalsDegraded bool
	// flushCh carries synchronous flush requests (restart handoff): the
	// writer drains the buffered channel and commits before replying, so
	// the caller knows every accepted record is durable - without sealing
	// the pipeline the way Close does.
	flushCh chan chan error
	// lastDebugExpire is writer-goroutine state for the opportunistic expired
	// sidecar sweep. debugExpirePending means a sweep (the writer tick or an
	// explicit startup call) stopped at its bounded increment and the writer's
	// near-term tick must continue it; it is atomic because the startup call
	// can publish it off the writer goroutine.
	lastDebugExpire    time.Time
	debugExpirePending atomic.Bool
}

// cantOpenContext explains SQLITE_CANTOPEN startup failures. The bare SQLite
// text ("unable to open database file (14)") names neither the database path
// nor the runtime identity, which turned first-launch permission mistakes on
// host bind mounts into guesswork. It classifies the database directory only
// on the already failing path: missing, not writable by this process, or
// writable with the failure coming from the file, its WAL/SHM sidecars or the
// filesystem. Deployment remediation (Docker creates missing bind-mount host
// directories as root) belongs to the operations documentation, not here.
func cantOpenContext(err error, path string) error {
	var serr *sqlite.Error
	if !errors.As(err, &serr) || serr.Code()&0xff != sqlite3.SQLITE_CANTOPEN {
		return err
	}
	dir := filepath.Dir(path)
	var cause string
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		cause = fmt.Sprintf("directory %q does not exist", dir)
	} else if probe, probeErr := os.CreateTemp(dir, ".millivolt-open-probe-*"); probeErr != nil {
		cause = fmt.Sprintf("directory %q is not writable by uid %d/gid %d; create it owned by that user or chown it",
			dir, os.Getuid(), os.Getgid())
	} else {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		cause = fmt.Sprintf("directory %q is writable, so the database file, its WAL/SHM sidecars or the filesystem rejected the open", dir)
	}
	return fmt.Errorf("%w: cannot open %q: %s", err, path, cause)
}

// initializeDatabase is the one owner of the startup schema sequence: the
// fresh-database schema, the owned analytical trigger definitions, and the
// forward migrations. Open runs it on the live file; the pending-snapshot
// preflight runs the identical sequence on the staged file before admission,
// so a snapshot that would fail here is quarantined before any live file is
// touched. Labels are part of Open's observable error contract.
func initializeDatabase(db *sql.DB, path string) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("schema: %w", cantOpenContext(err, path))
	}
	if err := ensureProjectionTriggers(db); err != nil {
		return fmt.Errorf("analytical projection triggers: %w", err)
	}
	if err := migrate(db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Schema/migrate alone does not prove the writer can write this database.
	// Run the exact writer INSERT (same column list, conflict clause and value
	// list) inside a transaction and roll it back. An extra required column or
	// an id without a unique key fails here, before a live store starts its
	// writer goroutine (or a pending snapshot is admitted) and every batch
	// fails. Shared by Open and the pending-snapshot preflight so live boot
	// and restore admission cannot disagree about a writable schema.
	if err := probeWriterInsert(db); err != nil {
		return fmt.Errorf("writer insert probe: %w", err)
	}
	return nil
}

// Open opens (or creates) the SQLite database at path and starts the
// background writer. Call Close to shut down cleanly. opts carries the
// pipeline tunables (channel/batch sizes, flush cadence, query timeout).
func Open(path string, opts Options) (*Store, error) {
	admission, err := admitPendingSnapshot(path)
	if err != nil {
		return nil, fmt.Errorf("pending snapshot: %w", err)
	}
	// fail restores the pre-admission database before Open returns an error
	// after an admission. The rollback error is joined instead of masking the
	// construction failure that triggered it.
	fail := func(err error) (*Store, error) {
		if rerr := admission.rollback(); rerr != nil {
			return nil, errors.Join(err, fmt.Errorf("pending snapshot: rollback: %w", rerr))
		}
		return nil, err
	}
	// Pragmas ride the DSN so every pooled connection gets them: modernc
	// applies each _pragma at connection open (busy_timeout sorted ahead of
	// the rest), so a replacement connection can never silently lose one -
	// a one-shot db.Exec reaches only whichever connection happens to serve
	// it, which was never guaranteed for synchronous (a per-connection
	// setting; a replacement writer would fall back to the FULL default).
	// journal_mode WAL is persistent in the database header once the first
	// writer connection sets it. The schema exec below is the eager fail
	// point: a rejected pragma fails Open here, never a later request.
	// opts.BusyTimeout overrides the guardrail constant when nonzero
	// (test seam; production Options leave it zero).
	busyTimeout := opts.BusyTimeout
	if busyTimeout <= 0 {
		busyTimeout = sqliteBusyTimeout
	}
	busy := fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds())
	// journal_size_limit bounds the WAL file after checkpoints (the default
	// keeps the high-water mark for the process lifetime). wal_autocheckpoint
	// stays at SQLite's 1000-page default: the batch writer commits every
	// flush interval, and a larger threshold trades WAL bytes for fewer
	// checkpoint passes; it is measured by BenchmarkStorageWriter.
	params := url.Values{}
	params.Add("_pragma", busy)
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Add("_pragma", "journal_size_limit(67108864)")
	// Take the write lock at BEGIN for every explicit transaction. A deferred
	// BEGIN would establish a read snapshot before insertBatch compares its
	// totals witness, so a restart-handoff writer committing in that window
	// would turn the upgrade into SQLITE_BUSY_SNAPSHOT instead of the busy
	// wait the writer contract promises.
	params.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", backup.SQLiteURI(path, params))
	if err != nil {
		return fail(fmt.Errorf("open sqlite: %w", err))
	}
	if err := initializeDatabase(db, path); err != nil {
		db.Close()
		return fail(err)
	}
	db.SetMaxOpenConns(sqliteWriterConns)
	// Planner statistics are a read-path guardrail, not request data. They let
	// SQLite cost the ordered log index scan against a table scan instead of
	// relying on default guesses. Refresh only when absent or materially stale
	// so ordinary boots pay no write.
	ensureRequestStatistics(db)

	// Read pool: with WAL, readers don't block the single writer. Dashboard
	// queries run here so a slow SELECT can't starve insertBatch. The pool is
	// opened read-only as a second layer of defense behind validateQuery - the
	// "file:" URI scheme is required for modernc.org/sqlite to honor mode=ro
	// (a bare "path?mode=ro" is silently opened read-write). Only the busy
	// timeout rides this DSN: a mode=ro connection cannot change
	// journal_mode, and synchronous is write-path state.
	readParams := url.Values{"mode": {"ro"}}
	readParams.Add("_pragma", busy)
	rdb, err := sql.Open("sqlite", backup.SQLiteURI(path, readParams))
	if err != nil {
		db.Close()
		return fail(fmt.Errorf("open read pool: %w", err))
	}
	// Bound the pool: database/sql defaults to unlimited connections, and each
	// SQLite connection carries its own page cache (about 2 MiB by default) in
	// anonymous mmap outside the Go heap limit. The bound queues excess
	// dashboard/MCP reads behind StorageQueryTimeout instead of multiplying
	// caches without limit; the writer and the retained data_version
	// connection are separate pools.
	rdb.SetMaxOpenConns(sqliteReadConns)
	rdb.SetMaxIdleConns(sqliteReadConns)

	s := &Store{
		path:          path,
		db:            db,
		rdb:           rdb,
		ch:            make(chan queuedRecord, opts.WriteChanCap),
		opts:          opts,
		done:          make(chan struct{}),
		writerDone:    make(chan struct{}),
		written:       make(map[string]struct{}),
		writtenCap:    opts.WriteTrackCap,
		flushCh:       make(chan chan error, 1),
		purgeWake:     make(chan struct{}, 1),
		debugMaxBytes: int64(opts.DebugMaxBytes),
	}
	if s.debugMaxBytes > 0 {
		// Seed the sidecar byte counter once; the cap must reflect captures
		// written by earlier processes. A read failure leaves the counter
		// unseeded and SaveDebugCapture retries the seed before enforcing.
		if err := s.seedDebugBytes(context.Background()); err != nil {
			log.Printf("storage: seed debug byte counter: %v", err)
		}
	}
	// Since-inception totals are a durable meta row maintained in the same
	// transaction as every mutation. A current document is O(1) at boot; an
	// absent/stale one rebuilds once from the rows (bounded by the maintenance
	// budget, not the interactive query timeout). A failure logs and leaves
	// totals degraded - never silently pretend they're right.
	tctx, cancel := s.withMaintenanceTimeout(context.Background())
	if persisted, witness, ok, err := loadTotals(tctx, s.rdb); err == nil && ok {
		s.installTotals(persisted, witness.epoch, witness.highRowID)
	} else {
		if err != nil {
			log.Printf("storage: persisted totals unreadable (%v); rebuilding", err)
		}
		if err := s.computeTotals(tctx); err != nil {
			log.Printf("storage: initial totals scan failed (dashboard aggregates degraded): %v", err)
			s.aggMu.Lock()
			s.totalsDegraded = true
			s.aggMu.Unlock()
		}
	}
	cancel()
	s.wg.Add(1)
	go s.writer()
	// The store is fully constructed and serving: the admitted snapshot is now
	// permanent, so remove its rollback sibling. A cleanup failure before the
	// sibling's main file is gone rolls the admission back and fails boot
	// instead of leaving a database that the next boot would silently revert.
	if err := admission.commit(); err != nil {
		closeErr := s.Close()
		return fail(errors.Join(fmt.Errorf("pending snapshot: commit: %w", err), closeErr))
	}
	// Reclaim database-sized staging entries a previous process left behind
	// when it died between creating a private file/directory and its defer.
	// This runs after admission so the sweep cannot touch the pending slot it
	// just resolved, and it never blocks or fails boot.
	if removed := s.sweepOrphanedStaging(time.Now()); removed > 0 {
		log.Printf("storage: reclaimed %d orphaned staging entries in %s", removed, s.stageDir())
	}
	return s, nil
}

// requestStatisticsTimeout bounds the one-time planner-statistics build so a
// stuck disk cannot hold boot indefinitely. It is an internal startup
// guardrail, not an operator tunable.
const requestStatisticsTimeout = 2 * time.Minute

// requestStatisticsDrift is how far the analyzed row count may lag the live
// count before a refresh. A small floor avoids re-analyzing on every append.
const requestStatisticsDrift = 10_000

// ensureRequestStatistics builds sqlite_stat1/sqlite_stat4 for requests when
// no statistics exist or the analyzed row count drifted materially. ANALYZE
// writes only SQLite's stat tables; request rows, triggers and totals are
// untouched. A failure is logged and leaves the database usable, with the
// planner's existing cost guesses.
func ensureRequestStatistics(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), requestStatisticsTimeout)
	defer cancel()
	stale, err := requestStatisticsStale(ctx, db)
	if err != nil {
		log.Printf("storage: inspect planner statistics: %v", err)
		return
	}
	if !stale {
		return
	}
	if _, err := db.ExecContext(ctx, "ANALYZE requests"); err != nil {
		log.Printf("storage: refresh planner statistics: %v", err)
	}
}

func requestStatisticsStale(ctx context.Context, db *sql.DB) (bool, error) {
	var stat string
	err := db.QueryRowContext(ctx,
		`SELECT stat FROM sqlite_stat1 WHERE tbl = 'requests' LIMIT 1`).Scan(&stat)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no such table") {
			return true, nil
		}
		return false, err
	}
	fields := strings.Fields(stat)
	if len(fields) == 0 {
		return true, nil
	}
	analyzed, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return true, nil
	}
	var live int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&live); err != nil {
		return false, err
	}
	if live == 0 {
		return false, nil
	}
	drift := live - analyzed
	if drift < 0 {
		drift = -drift
	}
	threshold := live / 10
	if threshold < requestStatisticsDrift {
		threshold = requestStatisticsDrift
	}
	return drift > threshold, nil
}

// Record implements metrics.Recorder. It never blocks: if the channel is
// full, the record is dropped (metrics are best-effort, not lossless).
// Records arriving after Close are dropped too: a send on the closed channel
// panics, which we recover and count as a drop rather than crash the proxy.
// The shared retention boundary runs here as well: Store.Record is a valid
// Recorder without a ring, and nothing may persist unbounded client text.
func (s *Store) Record(r *metrics.Record) {
	s.record(r, nil)
}

func (s *Store) record(r *metrics.Record, buffer *metrics.Buffer) {
	if r == nil {
		return
	}
	// Recover is a last-resort belt: the sendMu/shut guard below already makes
	// send-on-closed-channel impossible, so a panic here would be a real bug.
	defer func() {
		if recover() != nil {
			s.incrDrops()
		}
	}()
	// Bound exactly once at the shared publication fence. The ring path's
	// Buffer.Record applies the retention owner before enqueueing this same
	// record and must stay under sendMu: Clear captures the accepted count and
	// the ring removal boundary in one critical section, so ring publication
	// and the enqueue fence cannot straddle a purge. A direct Store.Record has
	// no ring and applies retention here.
	// Hold sendMu so Close cannot close ch between the shut check and the send
	// (the select never blocks: the channel is buffered and default drops).
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if buffer != nil {
		buffer.Record(r)
	} else {
		metrics.RetainRecord(r)
	}
	if s.shut {
		s.incrDrops()
		return
	}
	select {
	case s.ch <- queuedRecord{record: r, seq: s.accepted + 1}:
		s.accepted++
	case <-s.done:
		s.incrDrops()
	default:
		// Drop on overflow to protect the hot path. This is intentional:
		// the ring buffer already holds the most recent records in memory.
		s.incrDrops()
	}
}

// rowQuerier is the one-row read surface shared by sidecar byte accounting on
// a raw pool, a transaction or a held connection.
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// debugByteTotalHook is a test seam (production nil). It can force the
// compute-only sidecar read to fail so merge's commit-blocking accounting path
// stays testable without a fault-injection driver.
var debugByteTotalHook func() error

// debugByteTotal is the compute-only sidecar read on the caller's
// transaction/connection. It never publishes the counter: a transaction's
// uncommitted state may still roll back, so only its committer may adjust the
// counter afterwards.
func debugByteTotal(ctx context.Context, q rowQuerier) (int64, error) {
	if debugByteTotalHook != nil {
		if err := debugByteTotalHook(); err != nil {
			return 0, err
		}
	}
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug`).Scan(&n)
	return n, err
}

// installDebugBytes publishes a committed sidecar byte total. Callers must
// establish that the backing transaction (if any) committed: publishing an
// in-transaction total before commit lets a rollback silently bypass the cap.
func (s *Store) installDebugBytes(n int64) {
	s.debugMu.Lock()
	s.debugBytes, s.debugSeeded = n, true
	s.debugMu.Unlock()
}

// debugAfterCommitHook is a test seam (production nil). It runs after a
// maintenance transaction commits and before its sidecar delta is published,
// widening the exact window a concurrent capture must not lose.
var debugAfterCommitHook func()

// commitDebugDelta commits a maintenance transaction and applies its net
// sidecar byte delta while holding debugMu. Holding the lock across commit
// keeps a concurrent SaveDebugCapture (which needs the same writer connection)
// from incrementing the counter between the committed state and its
// publication, so the absolute publish bug becomes an exact delta applied to
// the current value. When the counter is unseeded the delta is deliberately
// dropped: the next save reseeds from durable rows and enforces the cap.
func (s *Store) commitDebugDelta(tx *sql.Tx, delta int64) error {
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	if err := tx.Commit(); err != nil {
		// A COMMIT error leaves the outcome uncertain. Never keep enforcing
		// the old absolute counter: the next save reseeds from durable rows.
		s.debugSeeded = false
		return err
	}
	if debugAfterCommitHook != nil {
		debugAfterCommitHook()
	}
	if s.debugSeeded {
		s.debugBytes = max(0, s.debugBytes+delta)
	}
	return nil
}

// seedDebugBytes initializes the sidecar byte counter from durable rows. The
// writer connection is free at Open; a failure leaves it unseeded so
// SaveDebugCapture retries before enforcing the cap.
func (s *Store) seedDebugBytes(ctx context.Context) error {
	n, err := debugByteTotal(ctx, s.db)
	if err != nil {
		return err
	}
	s.installDebugBytes(n)
	return nil
}

func (s *Store) incrDrops() {
	// Atomic: the overload path must not serialize producers on a mutex
	// exactly when records are already being lost.
	s.drops.Add(1)
}

// Dropped returns the number of records dropped due to a full write channel.
func (s *Store) Dropped() uint64 { return s.drops.Load() }

func (s *Store) writer() {
	defer func() { close(s.writerDone); s.wg.Done() }()
	batch := make([]*metrics.Record, 0, s.opts.BatchCap)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.insertBatch(batch)
		if err != nil {
			if s.writerErr == nil {
				s.writerErr = err
			}
			log.Printf("storage: insert batch: %v", err)
			// Count flush failures as drops so durable-write loss is visible
			// alongside channel-overflow drops (metrics are best-effort, but
			// never silently lost).
			for range batch {
				s.incrDrops()
			}
		}
		clear(batch)
		batch = batch[:0]
		return err
	}
	var consumed uint64
	appendRecord := func(r queuedRecord) error {
		consumed = r.seq
		batch = append(batch, r.record)
		if len(batch) >= cap(batch) {
			return flush()
		}
		return nil
	}
	// A command is published at an exact accepted-record boundary. Inspect it
	// before consuming any later record, including synchronous flush/shutdown
	// drains. Only the writer waits for SQL; producers merely publish the fence.
	// A nil check must not acquire sendMu: contending request producers can
	// otherwise starve this consumer while its bounded channel overflows.
	// Clear publishes under sendMu before later enqueues; receiving such an
	// enqueue and then loading this pointer observes that command. A command
	// published after this load still includes the already-received record in
	// its accepted boundary. Thus lock-free inspection cannot cross the fence.
	servicePurge := func(held *queuedRecord) (bool, error) {
		cmd := s.pendingPurge.Load()
		if cmd == nil {
			return false, nil
		}
		var firstErr error
		usedHeld := held != nil && held.seq <= cmd.accepted
		if usedHeld {
			firstErr = appendRecord(*held)
		}
		for consumed < cmd.accepted {
			r, ok := <-s.ch
			if !ok {
				firstErr = errors.New("storage: closed before purge boundary")
				break
			}
			if err := appendRecord(r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		writeErr := firstErr
		var result PurgeResult
		if firstErr == nil {
			result, firstErr = s.purgeTransaction(cmd)
		}
		// Clear holds purgeMu until this result is received, so another command
		// cannot replace this one before the writer has retired it. Retire with
		// compare-and-swap anyway: clearing an exact command can never discard a
		// newer published command.
		s.pendingPurge.CompareAndSwap(cmd, nil)
		cmd.result <- purgeOutcome{result, firstErr}
		return usedHeld, writeErr
	}
	accept := func(r queuedRecord) error {
		used, err := servicePurge(&r)
		if used {
			return err
		}
		return errors.Join(err, appendRecord(r))
	}
	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()
	// The continuation cadence is independent of the configured flush interval
	// so a large expired sidecar set still drains promptly when that interval
	// is long.
	debugTicker := time.NewTicker(debugExpireRetryInterval)
	defer debugTicker.Stop()
	for {
		select {
		case r, ok := <-s.ch:
			if !ok {
				// Channel closed and fully drained; final flush.
				servicePurge(nil)
				flush()
				return
			}
			accept(r)
		case <-s.purgeWake:
			servicePurge(nil)
		case ret := <-s.flushCh:
			// Synchronous flush (restart handoff): absorb everything already
			// buffered, commit it, then reply. The writer stays alive - unlike
			// Close, the pipeline keeps accepting records afterwards.
			_, firstErr := servicePurge(nil)
		drain:
			for {
				select {
				case r, ok := <-s.ch:
					if !ok {
						break drain
					}
					err := accept(r)
					if err != nil && firstErr == nil {
						firstErr = err
					}
				default:
					break drain
				}
			}
			if _, err := servicePurge(nil); err != nil && firstErr == nil {
				firstErr = err
			}
			if err := flush(); err != nil && firstErr == nil {
				firstErr = err
			}
			ret <- firstErr
		case <-ticker.C:
			flush()
			s.expireDebugCapturesIfDue()
		case <-debugTicker.C:
			s.continueDebugExpire()
		case <-s.done:
			// Shutdown: Close closes s.ch right after s.done, so range drains
			// every buffered record - including any a producer raced in after
			// the done check - before the final flush.
			for r := range s.ch {
				accept(r)
			}
			servicePurge(nil)
			flush()
			return
		}
	}
}

// expireDebugCapturesIfDue runs the expired-sidecar sweep from the writer
// goroutine at a bounded cadence. Expired rows are already invisible to reads;
// without the sweep their bytes stay on disk until the next restart. A sweep
// stopped at its per-tick bound keeps the pending flag set so the next tick
// continues immediately instead of waiting a full interval.
func (s *Store) expireDebugCapturesIfDue() {
	now := time.Now()
	if !s.debugExpirePending.Load() && !s.lastDebugExpire.IsZero() && now.Sub(s.lastDebugExpire) < debugExpireInterval {
		return
	}
	s.sweepDebugCaptures(now)
}

// continueDebugExpire drains the next bounded increment when the previous
// sweep stopped at its per-tick limit. The writer's dedicated ticker calls it
// even when the configured flush cadence is long.
func (s *Store) continueDebugExpire() {
	if !s.debugExpirePending.Load() {
		return
	}
	s.sweepDebugCaptures(time.Now())
}

// sweepDebugCaptures runs exactly one bounded sweep on the writer goroutine and
// records whether another increment remains.
func (s *Store) sweepDebugCaptures(now time.Time) {
	s.lastDebugExpire = now
	ctx, cancel := s.withQueryTimeout(context.Background())
	defer cancel()
	_, more, err := s.expireDebugCaptures(ctx, now.UnixMilli(), debugExpireBatchesPerSweep)
	s.debugExpirePending.Store(err == nil && more)
	if err != nil && ctx.Err() == nil {
		log.Printf("storage: expire debug captures: %v", err)
	}
}

// markWrittenLocked records id as durably written, evicting the oldest
// tracked id once the cap is reached so the written set stays bounded. The
// caller must hold s.aggMu; duplicate guards stay with each caller.
func (s *Store) markWrittenLocked(id string) {
	if s.writtenCap > 0 && len(s.writtenOrder) >= s.writtenCap {
		delete(s.written, s.writtenOrder[0])
		s.writtenOrder = s.writtenOrder[1:]
	}
	s.written[id] = struct{}{}
	s.writtenOrder = append(s.writtenOrder, id)
}

// account folds a committed batch into the written-id set and totals. When
// next is non-nil it is the exact aggregate persisted in the batch's own
// transaction; installing it (instead of re-adding) keeps memory and disk
// identical. Called by the single writer goroutine, so it never races another
// account().
func (s *Store) account(records []*metrics.Record, next *Totals, witness totalsWitness, degraded bool) {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	if next != nil {
		// A maintenance install (merge, purge, totals rebuild) can acquire the
		// writer connection after this batch commits but before account runs.
		// Such an install already counted this batch from durable rows, so a
		// newer witness wins: never overwrite it with the batch's older base.
		if !s.totalsBuilt || !totalsWitnessNewer(s.totalsWitness, witness) {
			s.totals = *next
			s.totalsBuilt = true
			s.totalsWitness = witness
			// The degraded flag describes a rebuild that never saw older rows.
			// Post-boot accounting never clears it; only a later boot's
			// successful full scan (or an explicit maintenance rebuild) does. A
			// failed in-transaction rebuild sets it so the subset is never
			// presented as since-inception.
			s.totalsDegraded = s.totalsDegraded || degraded
		}
	} else {
		if !s.totalsBuilt {
			// Degraded mode: the initial rebuild failed. Count new rows from
			// zero so live aggregates at least track post-start traffic.
			s.totals = Totals{}
			s.totalsBuilt = true
		}
		for _, r := range records {
			s.totals.Add(r)
		}
	}
	for _, r := range records {
		if _, ok := s.written[r.ID]; !ok {
			s.markWrittenLocked(r.ID)
		}
	}
	// Every successful account call follows a durable commit boundary. Publish
	// a new revision even when a duplicate record leaves the folded values
	// unchanged: a reader may have captured the old key before this commit.
	s.accountingRevision.Add(1)
}

// totalsBase returns the in-memory aggregate and the projection witness it
// was read against. The caller holds the sole writer connection (Begin has
// returned), so no merge/purge can publish between this read and the commit;
// insertBatch still compares the witness inside its transaction because an
// external restart-handoff writer can commit between boot and this batch.
func (s *Store) totalsBase() (Totals, bool, bool, totalsWitness) {
	s.aggMu.Lock()
	next := s.totals
	built := s.totalsBuilt
	degraded := s.totalsDegraded
	witness := s.totalsWitness
	s.aggMu.Unlock()
	if !built {
		next = Totals{}
	}
	return next, built, degraded, witness
}

// AccountingRevision returns the durable accounting witness used to validate
// exact KPI fold memos. It advances under aggMu whenever totals or the
// written-id set changes; reading it never takes that lock. A fold whose
// before/after reads are equal observed one coherent durable state.
func (s *Store) AccountingRevision() uint64 { return s.accountingRevision.Load() }

// TotalsAndUnwritten captures totals and filters the caller-owned ring slice
// under one accounting lock. A racing commit cannot remove a row from both
// sides of the merge. Only the bounded ring is probed; the written set is not
// copied, and record folding happens after the lock is released.
func (s *Store) TotalsAndUnwritten(ring []*metrics.Record) (Totals, []*metrics.Record) {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	out := ring[:0]
	for _, r := range ring {
		if r == nil {
			continue
		}
		if _, written := s.written[r.ID]; !written {
			out = append(out, r)
		}
	}
	return s.totals, out
}

// FilterUnwritten takes a set of record ids and returns only those not in the
// written set - the exact dedupe boundary for ring/stored merges. Takes one
// aggMu hold instead of copying the full written map, so the caller pays
// O(ring) probes instead of O(written) allocation.
func (s *Store) FilterUnwritten(ids []string) []string {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	out := ids[:0] // reuse the caller's slice to avoid allocation
	for _, id := range ids {
		if _, ok := s.written[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Totals returns the since-inception aggregate (durably committed rows). For
// ring-merge consumers prefer TotalsAndUnwritten (atomic pair).
func (s *Store) Totals() Totals {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	return s.totals
}

// MarkWritten primes the written-id set with ids that were persisted BEFORE
// this process started (the startup ring backfill). Without it a fresh
// process would treat every backfilled record as "not yet written" and the
// dashboard aggregates would double-count the whole ring against the store.
func (s *Store) MarkWritten(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	changed := false
	for _, id := range ids {
		if _, ok := s.written[id]; ok {
			continue
		}
		s.markWrittenLocked(id)
		changed = true
	}
	if changed {
		s.accountingRevision.Add(1)
	}
}

// computeTotals rebuilds the aggregate from the durable rows (startup +
// post-merge). One full scan; runs synchronously at Open or while the caller
// holds the sole writer connection.
func (s *Store) computeTotals(ctx context.Context) error {
	return s.computeTotalsWith(ctx, nil)
}

// computeTotalsWith is computeTotals with a test seam between the consistent
// read snapshot and the writer-side epoch check. Production passes nil.
func (s *Store) computeTotalsWith(ctx context.Context, afterRead func() error) error {
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		t, epoch, highRowID, err := s.readTotalsSnapshot(ctx)
		if err != nil {
			return err
		}
		if afterRead != nil {
			if err := afterRead(); err != nil {
				return err
			}
		}
		ok, err := s.persistTotalsIfCurrent(ctx, t, epoch, highRowID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("totals snapshot changed while rebuilding")
}

// readTotalsSnapshot reads the rows and their mutation witness from one
// consistent read transaction. The epoch and high rowid are the same witness
// the writer-side recheck compares against.
func (s *Store) readTotalsSnapshot(ctx context.Context) (Totals, int64, int64, error) {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Totals{}, 0, 0, err
	}
	defer tx.Rollback()
	t, err := readTotals(ctx, tx)
	if err != nil {
		return Totals{}, 0, 0, err
	}
	epoch, err := projectionEpoch(ctx, tx)
	if err != nil {
		return Totals{}, 0, 0, err
	}
	highRowID, err := projectionHighRowID(ctx, tx)
	if err != nil {
		return Totals{}, 0, 0, err
	}
	return t, epoch, highRowID, nil
}

// persistTotalsIfCurrent commits the document only when both mutation
// witnesses still match the read snapshot: the epoch covers rewrites,
// removals and out-of-order rowids, and the high rowid covers ordinary
// appends that leave the epoch unchanged. Holding the sole writer connection
// through installTotals prevents an in-process commit from landing between the
// check and publication; the next boot rebuilds if an unsupported external
// writer commits after the check.
func (s *Store) persistTotalsIfCurrent(ctx context.Context, t Totals, epoch, highRowID int64) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, err := projectionEpoch(ctx, tx)
	if err != nil {
		return false, err
	}
	currentHigh, err := projectionHighRowID(ctx, tx)
	if err != nil {
		return false, err
	}
	if current != epoch || currentHigh != highRowID {
		return false, nil
	}
	if err := saveTotals(ctx, tx, t, false, current, currentHigh); err != nil {
		return false, fmt.Errorf("persist totals: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.installTotals(t, current, currentHigh)
	return true, nil
}

func (s *Store) installTotals(t Totals, epoch, highRowID int64) {
	s.aggMu.Lock()
	s.totals = t
	s.totalsBuilt = true
	s.totalsWitness = totalsWitness{epoch: epoch, highRowID: highRowID}
	s.totalsDegraded = false
	s.accountingRevision.Add(1)
	s.aggMu.Unlock()
}

// TotalsDegraded reports whether the boot-time totals scan failed, leaving
// the aggregate a truthful subset (rows committed since the failed scan)
// instead of since-inception totals.
func (s *Store) TotalsDegraded() bool {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	return s.totalsDegraded
}

// totalsMetaKey is the durable since-inception aggregate. It is written in the
// same transaction as every insert, full purge, filtered purge and merge, so a
// reboot loads one row instead of scanning all history. Version gates the
// document: a semantics change bumps it and forces one rebuilding scan.
//
// v3: TPS means the single first-token-to-last-token rate (gen_tps). The old
// document's tps_sum mixed overall_tps and gen_tps, so it is rebuilt.
const totalsMetaKey = "totals"

const totalsMetaVersion = 3

// totalsWitness is the projection state the in-memory totals were read or
// computed against. A batch may only extend a base whose witness is still
// current; otherwise it rebuilds from durable rows instead of blessing an
// undercount with newer witnesses. Protected by aggMu.
type totalsWitness struct {
	epoch     int64
	highRowID int64
}

// totalsWitnessNewer reports whether a describes a later projection mutation
// than b. The epoch advances on rewrites/removals; within one epoch the high
// rowid advances on appends.
func totalsWitnessNewer(a, b totalsWitness) bool {
	if a.epoch != b.epoch {
		return a.epoch > b.epoch
	}
	return a.highRowID > b.highRowID
}

// totalsDoc is the persisted shape. Totals' JSON tags are the dashboard API
// contract; this type owns the storage contract separately.
type totalsDoc struct {
	V int `json:"v"`
	// Epoch is the projection mutation epoch observed after the same
	// transaction's request triggers. Rewrites, removals and out-of-order
	// rowids advance it, so a stale document is detected at boot and rebuilt
	// instead of trusted over authoritative SQLite rows.
	Epoch int64 `json:"epoch"`
	// HighRowID is the projection high-water mark observed in the same
	// transaction. An ordinary external append keeps the epoch unchanged but
	// always advances this mark, so it catches the one mutation the epoch
	// deliberately leaves out of the fast path.
	HighRowID int64   `json:"high_rowid"`
	Invalid   bool    `json:"invalid,omitempty"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Cost      float64 `json:"cost"`
	InputTok  int64   `json:"input_tokens"`
	OutputTok int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	Reasoning int64   `json:"reasoning_tokens"`
	TTFTSum   int64   `json:"ttft_sum"`
	TTFTN     int64   `json:"ttft_n"`
	TPSSum    float64 `json:"tps_sum"`
	TPSN      int64   `json:"tps_n"`
	CostReqs  int64   `json:"cost_reqs"`
	CostInOut float64 `json:"cost_in_out"`
}

func totalsDocument(t Totals, invalid bool, epoch, highRowID int64) totalsDoc {
	return totalsDoc{
		V: totalsMetaVersion, Epoch: epoch, HighRowID: highRowID, Invalid: invalid || t.err != nil,
		Requests: t.Requests, Errors: t.Errors, Cost: t.Cost,
		InputTok: t.InputTok, OutputTok: t.OutputTok,
		CacheRead: t.CacheRead, Reasoning: t.Reasoning,
		TTFTSum: t.TTFTSum, TTFTN: t.TTFTN,
		TPSSum: t.TPSSum, TPSN: t.TPSN,
		CostReqs: t.CostReqs, CostInOut: t.CostInOut,
	}
}

func (d totalsDoc) totals() Totals {
	return Totals{
		Requests: d.Requests, Errors: d.Errors, Cost: d.Cost,
		InputTok: d.InputTok, OutputTok: d.OutputTok,
		CacheRead: d.CacheRead, Reasoning: d.Reasoning,
		TTFTSum: d.TTFTSum, TTFTN: d.TTFTN,
		TPSSum: d.TPSSum, TPSN: d.TPSN,
		CostReqs: d.CostReqs, CostInOut: d.CostInOut,
	}
}

// saveTotals writes the aggregate and both mutation witnesses on the caller's
// transaction/connection. It must share the mutation's transaction: a totals
// row ahead of or behind the request rows would turn a crash into silently
// wrong since-inception KPIs, and a witness read outside that transaction
// could bless an aggregate that never saw its rows.
func saveTotals(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, t Totals, invalid bool, epoch, highRowID int64) error {
	b, err := json.Marshal(totalsDocument(t, invalid, epoch, highRowID))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, totalsMetaKey, string(b))
	return err
}

// loadTotals returns the persisted aggregate and the projection witness it was
// written against. found is false for an absent, malformed, invalid or
// older-version document, in which case the caller rebuilds from the durable
// rows and rewrites the document.
func loadTotals(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (t Totals, witness totalsWitness, found bool, err error) {
	var raw string
	var epoch, highRowID int64
	// One statement reads the document and both mutation witnesses from the
	// same SQLite snapshot, so a concurrent commit cannot land between them.
	err = db.QueryRowContext(ctx, `SELECT m.value, s.epoch, s.high_rowid
		FROM meta AS m CROSS JOIN request_projection_state AS s
		WHERE m.key = ? AND s.singleton = 1`, totalsMetaKey).Scan(&raw, &epoch, &highRowID)
	if errors.Is(err, sql.ErrNoRows) {
		return Totals{}, totalsWitness{}, false, nil
	}
	if err != nil {
		return Totals{}, totalsWitness{}, false, err
	}
	var d totalsDoc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		log.Printf("storage: persisted totals malformed (%v); rebuilding", err)
		return Totals{}, totalsWitness{}, false, nil
	}
	if d.V != totalsMetaVersion || d.Invalid {
		return Totals{}, totalsWitness{}, false, nil
	}
	if epoch != d.Epoch || highRowID != d.HighRowID {
		// Authoritative rows changed outside this store's transaction (the
		// projection triggers fired); the cached aggregate is no longer exact.
		return Totals{}, totalsWitness{}, false, nil
	}
	return d.totals(), totalsWitness{epoch: epoch, highRowID: highRowID}, true, nil
}

// projectionEpoch reads the current mutation witness on the caller's
// transaction/connection. Triggers keep it current within the transaction.
func projectionEpoch(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var epoch int64
	err := db.QueryRowContext(ctx, `SELECT epoch FROM request_projection_state WHERE singleton = 1`).Scan(&epoch)
	return epoch, err
}

// projectionHighRowID reads the append witness next to the epoch on the
// caller's transaction/connection. Ordinary inserts advance it without
// touching the epoch, so a persisted totals document must record both.
func projectionHighRowID(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var highRowID int64
	err := db.QueryRowContext(ctx, `SELECT high_rowid FROM request_projection_state WHERE singleton = 1`).Scan(&highRowID)
	return highRowID, err
}

func readTotals(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (Totals, error) {
	rows, err := db.QueryContext(ctx, `SELECT status_code, error_type, attempts,
		input_tokens, output_tokens, cache_read_tokens, reasoning_tokens,
		cost, ttft_ms, gen_tps
		FROM requests`)
	if err != nil {
		return Totals{}, err
	}
	defer rows.Close()
	var t Totals
	for rows.Next() {
		var status, ttft int64
		var errorType string
		var attemptsJSON []byte
		var in, out, cacheR, reason int64
		var cost, gen float64
		if err := rows.Scan(&status, &errorType, &attemptsJSON,
			&in, &out, &cacheR, &reason, &cost, &ttft, &gen); err != nil {
			return Totals{}, err
		}
		atts, err := DecodeAttemptsColumn(attemptsJSON)
		if err != nil {
			log.Printf("storage: totals: decode attempts: %v", err)
		}
		r := metrics.Record{StatusCode: int(status), ErrorType: errorType, Attempts: atts,
			TTFTMs: ttft, DecodeTPS: gen,
		}
		r.Usage.InputTokens = in
		r.Usage.OutputTokens = out
		r.Usage.CacheReadTokens = cacheR
		r.Usage.ReasoningTokens = reason
		r.Cost = cost
		t.Add(&r)
	}
	if err := rows.Err(); err != nil {
		return Totals{}, err
	}
	return t, nil
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

// Flush drains every record buffered in the write channel and commits it,
// then returns - without closing the pipeline (records sent afterwards are
// written by the writer as usual). This is the restart-handoff primitive:
// the draining process makes all finalized records durable BEFORE the fresh
// process opens the database, so the child's totals scan and ring backfill
// see the complete history while the old process keeps a working store for
// a failed-handoff resume. Concurrent Close waits for the actual final drain,
// including its error; a sealed input channel alone does not prove durability.
func (s *Store) Flush() error {
	ret := make(chan error, 1)
	select {
	case s.flushCh <- ret:
	case <-s.done:
		<-s.writerDone
		return s.writerErr
	}
	select {
	case err := <-ret:
		return err
	case <-s.done:
		<-s.writerDone
		return s.writerErr
	}
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

func (s *Store) Close() error {
	var err error
	s.once.Do(func() {
		// Mark shut and close under sendMu so a concurrent Record's
		// check-then-send is mutually exclusive with the channel close (no
		// send-on-closed-channel race).
		s.sendMu.Lock()
		s.shut = true
		close(s.done) // signal shutdown
		close(s.ch)   // seal the channel so the writer drains it deterministically
		s.sendMu.Unlock()
		s.wg.Wait() // wait for the final flush
		err = errors.Join(s.writerErr, s.db.Close())
		s.versionMu.Lock()
		s.versionClosed = true
		if s.versionConn != nil {
			if verr := s.versionConn.Close(); err == nil {
				err = verr
			}
		}
		s.versionMu.Unlock()
		if s.rdb != nil {
			// The read pool holds no write state (mode=ro), so a close failure
			// is benign - but surface it when nothing else failed.
			if rerr := s.rdb.Close(); err == nil {
				err = rerr
			}
		}
	})
	return err
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

	resultBytes := 2 // JSON array brackets, including the empty-array case
	rowCount := 0
	for rows.Next() {
		if rowCount == s.opts.QueryMaxRows {
			return ErrQueryLimit
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = values[i]
		}
		encoded, e := json.Marshal(row)
		if e != nil {
			return e
		}
		separator := 0
		if rowCount > 0 {
			separator = 1
		}
		if len(encoded) > s.opts.QueryMaxBytes-resultBytes-separator {
			return ErrQueryLimit
		}
		resultBytes += len(encoded) + separator
		rowCount++
		if jsonOut != nil {
			if separator == 1 {
				*jsonOut = append(*jsonOut, ',')
			}
			*jsonOut = append(*jsonOut, encoded...)
		}
		if keepRow != nil {
			keepRow(row)
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
