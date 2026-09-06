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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// sqliteWriterConns is the SQLite single-writer pool size. Safety
// guardrail (avoids SQLITE_BUSY), not user-tunable.
const sqliteWriterConns = 1

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
}

// errNotSelect rejects any dashboard query that is not a single SELECT.
var errNotSelect = errors.New("only a single SELECT statement is allowed")

// ErrQueryLimit rejects the entire result; callers must never serve a prefix.
var ErrQueryLimit = errors.New("query result exceeds its byte, row, or column limit")

// withQueryTimeout is the single choke point that bounds a dashboard SELECT
// by QueryTimeout. Validate requires QueryTimeout > 0; a 0 fail-closes
// (deadline already expired) rather than running unbounded.
func (s *Store) withQueryTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opts.QueryTimeout)
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

var schema = `
CREATE TABLE IF NOT EXISTS requests (
	id TEXT PRIMARY KEY,
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
	gen_tokens INTEGER NOT NULL DEFAULT 0,
	had_answer_content INTEGER NOT NULL DEFAULT 0,
	gen_tps REAL NOT NULL DEFAULT 0,
	overall_tps REAL NOT NULL DEFAULT 0,
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
	tool_names TEXT NOT NULL DEFAULT '',
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
	CREATE INDEX IF NOT EXISTS idx_requests_log ON requests (started_at, id);
CREATE INDEX IF NOT EXISTS idx_requests_provider ON requests (provider);
CREATE INDEX IF NOT EXISTS idx_requests_model ON requests (model);
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
	Answer    int64   `json:"answer_tokens"`
	ToolCalls int64   `json:"tool_calls"`
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

// AvgTPS returns the mean over captured throughput samples (tps > 0).
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
	t.Cost, t.err = metrics.SumValues(t.Cost, r.Cost)
	if t.err != nil {
		return
	}
	// Only cost-reporting requests feed the blended denominators - a 0-cost
	// request (provider sent no price) adds tokens but must not drag the
	// blended rate down.
	if r.Cost > 0 {
		t.CostReqs++
		t.CostInOut, t.err = metrics.SumValues(t.CostInOut, float64(r.Usage.InputTokens), float64(r.Usage.OutputTokens))
		if t.err != nil {
			return
		}
	}
	for _, term := range [...]struct {
		dst   *int64
		value int64
	}{
		{&t.InputTok, r.Usage.InputTokens}, {&t.OutputTok, r.Usage.OutputTokens},
		{&t.CacheRead, r.Usage.CacheReadTokens}, {&t.Reasoning, r.Usage.ReasoningTokens},
		{&t.Answer, r.AnswerTokens}, {&t.ToolCalls, int64(r.ToolCalls)},
	} {
		*term.dst, t.err = metrics.SumCounts(*term.dst, term.value)
		if t.err != nil {
			return
		}
	}
	// TTFTMs == 0 means "absent", not "0 ms" - no-token requests must not
	// dilute the mean (same gate as the client's truthy r.ttft_ms).
	if r.TTFTMs > 0 {
		t.TTFTSum, t.err = metrics.SumCounts(t.TTFTSum, r.TTFTMs)
		if t.err != nil {
			return
		}
		t.TTFTN++
	}
	if tps := r.OverallTPS; tps > 0 {
		t.TPSSum, t.err = metrics.SumValues(t.TPSSum, tps)
		t.TPSN++
	} else if r.DecodeTPS > 0 {
		t.TPSSum, t.err = metrics.SumValues(t.TPSSum, r.DecodeTPS)
		t.TPSN++
	}
}

// Store is a durable SQLite-backed store. It is safe for concurrent use.
type Store struct {
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

	mu    sync.Mutex
	drops uint64

	// aggMu guards the written-id set and the totals snapshot. The writer
	// updates both under this lock AFTER a batch commits, and dashboard
	// aggregators read both under the same lock, so a ring record is either
	// "already durably counted" (skipped in the merge) or not - never both,
	// never neither.
	aggMu        sync.Mutex
	written      map[string]struct{}
	writtenOrder []string
	writtenCap   int
	totals       Totals
	totalsBuilt  bool
	// flushCh carries synchronous flush requests (restart handoff): the
	// writer drains the buffered channel and commits before replying, so
	// the caller knows every accepted record is durable - without sealing
	// the pipeline the way Close does.
	flushCh chan chan error
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

// Open opens (or creates) the SQLite database at path and starts the
// background writer. Call Close to shut down cleanly. opts carries the
// pipeline tunables (channel/batch sizes, flush cadence, query timeout).
func Open(path string, opts Options) (*Store, error) {
	// Pragmas ride the DSN so every pooled connection gets them: modernc
	// applies each _pragma at connection open (busy_timeout sorted ahead of
	// the rest), so a replacement connection can never silently lose one -
	// a one-shot db.Exec reaches only whichever connection happens to serve
	// it, which was never guaranteed for synchronous (a per-connection
	// setting; a replacement writer would fall back to the FULL default).
	// journal_mode WAL is persistent in the database header once the first
	// writer connection sets it. The schema exec below is the eager fail
	// point: a rejected pragma fails Open here, never a later request.
	busy := fmt.Sprintf("_pragma=busy_timeout(%d)", sqliteBusyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", path+"?"+busy+"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", cantOpenContext(err, path))
	}
	if err := ensureProjectionTriggers(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("analytical projection triggers: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	db.SetMaxOpenConns(sqliteWriterConns)

	// Read pool: with WAL, readers don't block the single writer. Dashboard
	// queries run here so a slow SELECT can't starve insertBatch. The pool is
	// opened read-only as a second layer of defense behind validateQuery - the
	// "file:" URI scheme is required for modernc.org/sqlite to honor mode=ro
	// (a bare "path?mode=ro" is silently opened read-write). Only the busy
	// timeout rides this DSN: a mode=ro connection cannot change
	// journal_mode, and synchronous is write-path state.
	rdb, err := sql.Open("sqlite", "file:"+path+"?mode=ro&"+busy)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}

	s := &Store{
		db:         db,
		rdb:        rdb,
		ch:         make(chan queuedRecord, opts.WriteChanCap),
		opts:       opts,
		done:       make(chan struct{}),
		writerDone: make(chan struct{}),
		written:    make(map[string]struct{}),
		writtenCap: opts.WriteTrackCap,
		flushCh:    make(chan chan error, 1),
		purgeWake:  make(chan struct{}, 1),
	}
	// Build the since-inception totals once at Open (bounded by the query
	// timeout so a wedged read pool can't hang startup). A failure logs and
	// leaves totals in degraded mode - never silently pretend they're right.
	tctx, cancel := s.withQueryTimeout(context.Background())
	if err := s.computeTotals(tctx); err != nil {
		log.Printf("storage: initial totals scan failed (dashboard aggregates degraded): %v", err)
	}
	cancel()
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// Record implements metrics.Recorder. It never blocks: if the channel is
// full, the record is dropped (metrics are best-effort, not lossless).
// Records arriving after Close are dropped too: a send on the closed channel
// panics, which we recover and count as a drop rather than crash the proxy.
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
	// Hold sendMu so Close cannot close ch between the shut check and the send
	// (the select never blocks: the channel is buffered and default drops).
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if buffer != nil {
		buffer.Record(r)
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

func (s *Store) incrDrops() {
	s.mu.Lock()
	s.drops++
	s.mu.Unlock()
}

// Dropped returns the number of records dropped due to a full write channel.
func (s *Store) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drops
}

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
		// cannot replace this one before the writer has retired it.
		s.pendingPurge.Store(nil)
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

// account folds a committed batch into the written-id set and totals. Called
// by the single writer goroutine, so it never races another account().
func (s *Store) account(records []*metrics.Record) {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	if !s.totalsBuilt {
		// Degraded mode: the initial full-scan totals failed. Count new rows
		// from zero so live aggregates at least track post-start traffic.
		s.totals = Totals{}
		s.totalsBuilt = true
	}
	for _, r := range records {
		s.totals.Add(r)
		if _, ok := s.written[r.ID]; !ok {
			if s.writtenCap > 0 && len(s.writtenOrder) >= s.writtenCap {
				delete(s.written, s.writtenOrder[0])
				s.writtenOrder = s.writtenOrder[1:]
			}
			s.written[r.ID] = struct{}{}
			s.writtenOrder = append(s.writtenOrder, r.ID)
		}
	}
}

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

// IsWritten reports whether a record id is among the recently committed rows -
// the exact dedupe boundary for ring/stored merges.
func (s *Store) IsWritten(id string) bool {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	_, ok := s.written[id]
	return ok
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
	for _, id := range ids {
		if _, ok := s.written[id]; ok {
			continue
		}
		if s.writtenCap > 0 && len(s.writtenOrder) >= s.writtenCap {
			delete(s.written, s.writtenOrder[0])
			s.writtenOrder = s.writtenOrder[1:]
		}
		s.written[id] = struct{}{}
		s.writtenOrder = append(s.writtenOrder, id)
	}
}

// computeTotals rebuilds the aggregate from the durable rows (startup +
// post-purge). One full scan; runs synchronously at Open.
func (s *Store) computeTotals(ctx context.Context) error {
	t, err := readTotals(ctx, s.rdb)
	if err != nil {
		return err
	}
	s.installTotals(t)
	return nil
}

func (s *Store) installTotals(t Totals) {
	s.aggMu.Lock()
	s.totals = t
	s.totalsBuilt = true
	s.aggMu.Unlock()
}

func readTotals(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (Totals, error) {
	rows, err := db.QueryContext(ctx, `SELECT started_at, status_code, error_type, attempts,
		input_tokens, output_tokens, cache_read_tokens, reasoning_tokens, answer_tokens,
		tool_calls, cost, ttft_ms, duration_ms, overall_tps, gen_tps
		FROM requests`)
	if err != nil {
		return Totals{}, err
	}
	defer rows.Close()
	var t Totals
	for rows.Next() {
		var startedAt, status, ttft, dur int64
		var errorType string
		var attemptsJSON []byte
		var in, out, cacheR, reason, answer, tools int64
		var cost, overall, gen float64
		if err := rows.Scan(&startedAt, &status, &errorType, &attemptsJSON,
			&in, &out, &cacheR, &reason, &answer, &tools, &cost, &ttft, &dur, &overall, &gen); err != nil {
			return Totals{}, err
		}
		var atts []metrics.RetryAttempt
		if n := len(attemptsJSON); n > 0 && string(attemptsJSON) != "null" {
			if err := json.Unmarshal(attemptsJSON, &atts); err != nil {
				log.Printf("storage: totals: decode attempts: %v", err)
			}
		}
		r := metrics.Record{StatusCode: int(status), ErrorType: errorType, Attempts: atts,
			TTFTMs: ttft, OverallTPS: overall, DecodeTPS: gen,
		}
		r.Usage.InputTokens = in
		r.Usage.OutputTokens = out
		r.Usage.CacheReadTokens = cacheR
		r.Usage.ReasoningTokens = reason
		r.AnswerTokens = answer
		r.ToolCalls = int(tools)
		r.Cost = cost
		t.Add(&r)
	}
	if err := rows.Err(); err != nil {
		return Totals{}, err
	}
	return t, nil
}

// migrate brings an existing requests table up to the current schema: it adds
// any missing columns (forward-compatible, never drops history) and repairs
// data that violates a column invariant older binaries could write. It owns
// the invariant "attempts holds valid JSON" - the HasError purge predicate
// runs json_each over that column, and SQLite raises "malformed JSON" on any
// non-JSON value, denying the errors-only purge/count/export outright.
func migrate(db *sql.DB) error {
	// The compound cursor index replaces its timestamp-only predecessor once;
	// creating the canonical index is owned by schema above.
	if _, err := db.Exec("DROP INDEX IF EXISTS idx_requests_started_at"); err != nil {
		return err
	}
	if _, err := db.Exec(requestDebugDDL); err != nil {
		return fmt.Errorf("request_debug: %w", err)
	}
	cols, err := columnSet(db, "requests")
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
		{"overall_tps", "ALTER TABLE requests ADD COLUMN overall_tps REAL NOT NULL DEFAULT 0"},
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
		{"tool_names", "ALTER TABLE requests ADD COLUMN tool_names TEXT NOT NULL DEFAULT ''"},
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
	return nil
}

func columnSet(db *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
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
		started_at, duration_ms, ttft_ms, first_token_at, last_token_at,
		finish_reason, error_type, error_msg, error_code, tool_calls,
		input_tokens, output_tokens, total_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, client_disconnected,
		rate_limit_remaining, rate_limit_limit, cost,
		turns_user, turns_assistant, turns_tool, response_headers,
		answer_tokens, first_answer_at, gen_tokens, had_answer_content, gen_tps, overall_tps,
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

// scanRecord scans one row (in the canonical recordColumns order) into a
// metrics.Record, restoring the exact dashboard JSON shape (pointer request
// params including explicit zero/false, derived timestamps, parsed JSON columns).
func scanRecord(rows *sql.Rows) (*metrics.Record, error) {
	var r metrics.Record
	var startMs, durMs, ttftMs, firstMs, lastMs, firstAnswerMs int64
	var respHdr, toolNames, attemptsJSON string
	var finalAttemptMs int64
	var rateLimited bool
	var hadAnswer int
	var reqMaxTokens, reqN, reqTopLogprobs int
	var reqTemp, reqTopP, reqPresencePen, reqFrequencyPen float64
	var reqSeed int64
	var reqParallelTools bool
	var clientMetaJSON string
	var debugFlag int
	var reqPresence int64
	err := rows.Scan(
		&r.ID, &r.Provider, &r.Model, &r.KeyHash, &r.UserAgent, &r.Client,
		&r.ClientIP, &r.ClientLang, &r.Stream,
		&r.StatusCode, &startMs, &durMs, &ttftMs, &firstMs, &lastMs,
		&r.FinishReason, &r.ErrorType, &r.ErrorMsg, &r.ErrorCode, &r.ToolCalls,
		&r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.TotalTokens,
		&r.Usage.CacheReadTokens, &r.Usage.CacheWrite, &r.Usage.ReasoningTokens,
		&r.ClientDisconnected, &r.RateLimitRemaining, &r.RateLimitLimit,
		&r.Cost, &r.TurnsUser, &r.TurnsAssistant, &r.TurnsTool, &respHdr,
		&r.AnswerTokens, &firstAnswerMs, &r.GenTokens, &hadAnswer, &r.DecodeTPS, &r.OverallTPS,
		&r.Method, &r.Path, &r.ProcessingMs, &r.PromptPreview, &r.ResponsePreview,
		&r.ProviderModel, &r.ProviderRequestID, &r.ProviderServer,
		&r.QueueWaitMs, &rateLimited, &r.Retries, &r.RetryAfterMs, &toolNames,
		&reqMaxTokens, &reqTemp, &reqTopP, &r.ReqToolsCount, &r.ReqToolChoice,
		&reqN, &r.ReqStop, &r.ReqLogprobs, &reqPresencePen, &reqFrequencyPen,
		&r.ReqResponseFormat, &reqSeed, &reqParallelTools, &r.ReqLogitBias,
		&reqTopLogprobs, &r.ReqServiceTier, &r.ReqThinking, &r.ReqMetadataKeys,
		&r.ReqStreamOpts, &attemptsJSON, &finalAttemptMs, &r.ConversationID, &r.ParentConversationID,
		&r.CharsSystem, &r.CharsUser, &r.CharsAssistant, &r.CharsTool,
		&r.Images, &r.Attachments, &r.LastTurnRole,
		&clientMetaJSON, &r.ReqReasoningEffort, &r.ReqVerbosity,
		&debugFlag, &r.DebugSessionID, &reqPresence,
	)
	if err != nil {
		return nil, err
	}
	r.Start = time.UnixMilli(startMs)
	r.End = r.Start.Add(time.Duration(durMs) * time.Millisecond)
	r.DurationMs = durMs // restore the stored duration; End is derived from it
	r.TTFTMs = ttftMs
	r.HadAnswerContent = hadAnswer != 0
	if firstMs > 0 {
		r.FirstTokenAt = time.UnixMilli(firstMs)
	}
	if lastMs > 0 {
		r.LastTokenAt = time.UnixMilli(lastMs)
	}
	if firstAnswerMs > 0 {
		r.FirstAnswerAt = time.UnixMilli(firstAnswerMs)
	}
	if finalAttemptMs > 0 {
		r.FinalAttemptAt = time.UnixMilli(finalAttemptMs)
	}
	if respHdr != "" {
		if err := json.Unmarshal([]byte(respHdr), &r.ResponseHeaders); err != nil {
			log.Printf("storage: parse response headers for %s: %v", r.ID, err)
		}
	}
	if toolNames != "" {
		if err := json.Unmarshal([]byte(toolNames), &r.ToolNames); err != nil {
			log.Printf("storage: parse tool names for %s: %v", r.ID, err)
		}
	}
	if attemptsJSON != "" {
		if err := json.Unmarshal([]byte(attemptsJSON), &r.Attempts); err != nil {
			log.Printf("storage: parse attempts for %s: %v", r.ID, err)
		}
	}
	r.RateLimited = rateLimited
	r.Debug = debugFlag != 0
	r.ReqMaxTokens, r.ReqTemperature, r.ReqTopP = &reqMaxTokens, &reqTemp, &reqTopP
	r.ReqN, r.ReqPresencePen, r.ReqFrequencyPen = &reqN, &reqPresencePen, &reqFrequencyPen
	r.ReqSeed, r.ReqParallelTools, r.ReqTopLogprobs = &reqSeed, &reqParallelTools, &reqTopLogprobs
	restoreRequestParams(&r, reqPresence)
	if clientMetaJSON != "" && clientMetaJSON != "{}" {
		if err := json.Unmarshal([]byte(clientMetaJSON), &r.ClientMeta); err != nil {
			log.Printf("storage: parse client_meta for %s: %v", r.ID, err)
		}
	}
	return &r, nil
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

// QueryBefore returns a newest-first page below the exclusive cursor. A nil
// cursor starts at durable newest, independently of the arrival-ordered ring.
func (s *Store) QueryBefore(ctx context.Context, before *LogCursor, limit int) ([]*metrics.Record, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	q := `SELECT ` + recordColumns + ` FROM requests`
	var args []any
	if before != nil {
		q += ` WHERE (started_at, id) < (?, ?)`
		args = append(args, before.StartMs, before.ID)
	}
	q += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*metrics.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+recordColumns+` FROM requests ORDER BY started_at DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*metrics.Record
	for rows.Next() {
		r, err := scanRecord(rows)
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

// ExportWhere streams every stored row matching the filter (an empty filter
// = all rows - export is read-only, so "everything" is safe) as a compact
// JSON array, one record per line, oldest-first, directly to the writer -
// memory stays bounded no matter how much history exists. Returns the number
// of records written.
func (s *Store) ExportWhere(ctx context.Context, f PurgeFilter, w io.Writer) (int64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	q := `SELECT ` + recordColumns + ` FROM requests`
	where, args := f.build() // the same parameterized predicate as purge/count
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY started_at ASC, id ASC`
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return 0, err
	}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return 0, err
		}
		b, err := s.marshalExport(ctx, r)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			if _, err := io.WriteString(w, ",\n"); err != nil {
				return 0, err
			}
		}
		if _, err := w.Write(b); err != nil {
			return 0, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	if _, err := io.WriteString(w, "\n]\n"); err != nil {
		return 0, err
	}
	return n, nil
}

// marshalExport encodes one record for log download. Debug captures ride
// as a sibling "debug_capture" object so the ring JSON stays small.
func (s *Store) marshalExport(ctx context.Context, r *metrics.Record) ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	if !r.Debug {
		return json.Marshal(r)
	}
	type exportRec struct {
		*metrics.Record
		DebugCapture json.RawMessage `json:"debug_capture,omitempty"`
	}
	out := exportRec{Record: r}
	if raw, err := s.LoadDebugCapture(ctx, r.ID); err != nil {
		return nil, fmt.Errorf("load debug capture: %w", err)
	} else if len(raw) > 0 {
		out.DebugCapture = raw
	}
	return json.Marshal(out)
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

// Purge explicitly clears all durable history through the writer boundary.
func (s *Store) Purge(ctx context.Context) error {
	_, err := s.Clear(ctx, nil, nil)
	return err
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
// the same records from the live ring buffer that PurgeWhere deletes from the
// database, so the two never disagree.
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
		//   - 429 (rate limit) and 499 (local client closed the connection -
		//     its own cancellation) are flow events, never errors on their own;
		//   - any other error status (>= 400) or a structured error_type counts;
		//   - an absorbed retry attempt that hit a genuine 5xx counts even when
		//     the final outcome recovered (the upstream still failed).
		// The attempts column is a JSON array; json_each scans it for a 5xx.
		where = append(where, `(
			(status_code IN (429, 499) AND EXISTS (
				SELECT 1 FROM json_each(requests.attempts)
				WHERE json_extract(value, '$.status_code') >= 500
			))
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

// PurgeWhere deletes only the rows matching the filter, returning the count
// deleted. An empty filter is rejected (callers must use Purge for a full wipe)
// so a filtered purge can never accidentally become a delete-everything.
func (s *Store) PurgeWhere(ctx context.Context, f PurgeFilter) (int64, error) {
	result, err := s.Clear(ctx, &f, nil)
	return result.Deleted, err
}

// RenameProviders merges provider labels in stored history: every row whose
// provider matches an alias key is rewritten to its canonical label. Driven by
// config (provider_aliases) - a label split by a naming-scheme change is
// repaired generically, never with provider-specific code. Serialization with
// the writer goroutine comes from the single-connection write pool (the same
// shape PurgeWhere relies on); totals are label-independent, so no rebuild is
// needed. Malformed pairs are skipped (config Validate already rejects them -
// this is the last-chance deny), and each UPDATE is parameterized.
func (s *Store) RenameProviders(ctx context.Context, aliases map[string]string) (int64, error) {
	if s == nil || len(aliases) == 0 {
		return 0, nil
	}
	var total int64
	for from, to := range aliases {
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if from == "" || to == "" || from == to {
			continue
		}
		res, err := s.db.ExecContext(ctx, "UPDATE requests SET provider = ? WHERE provider = ?", to, from)
		if err != nil {
			return total, fmt.Errorf("rename provider %q: %w", from, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
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
// deltas. The epoch, high-water mark, and rows share one read transaction, so
// another process committing during a scan cannot make its cursor outrun its
// rows. Rewrites/removals/schema changes require a replacement snapshot.
// columns is code-owned (the same list consumed by scanContrib), never input
// from an HTTP request. No prompts, responses, or full records are retained.
func (s *Store) StreamProjection(ctx context.Context, columns string, cursor ProjectionCursor, fn func(*sql.Rows) error) (ProjectionCursor, bool, error) {
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
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

// StreamRows runs a SELECT (column list chosen by the caller - the dashboard
// aggregator) on the read pool and feeds each row to fn. Rows are scanned in
// caller-defined column order; fn must consume before the next call.
// orderByStart selects the existing timestamp index's oldest-first order,
// allowing a caller to stop after its first matching row without a full scan.
// Bounded by withQueryTimeout like LoadRecent/CountWhere (a 0 fail-closes);
// callers that already pass queryCtx nest safely.
func (s *Store) StreamRows(ctx context.Context, columns, where string, args []any, orderByStart bool, fn func(*sql.Rows) error) error {
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	q := "SELECT " + columns + " FROM requests"
	if where != "" {
		q += " WHERE " + where
	}
	if orderByStart {
		q += " ORDER BY started_at ASC"
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DataVersion identifies committed database content for aggregate memoization.
// SQLite versions are comparable only on the same connection, so never query
// this through the pool or replace a failed connection and reuse its version.
// Errors disable cache reuse; ordinary history queries can still proceed.
func (s *Store) DataVersion(ctx context.Context) (int64, error) {
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
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
	where, args := f.build()
	q := "SELECT COUNT(*) FROM requests"
	if where != "" {
		q += " WHERE " + where
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	var n int64
	err := s.rdb.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
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

// Query owns ad-hoc SELECT execution time and result resource bounds. The
// SELECT-only HTTP restriction stays in HandleQuery. SQLite working memory
// (sorting/aggregate intermediates) is not a strict per-query heap budget.
func (s *Store) Query(ctx context.Context, query string, args ...any) (out []map[string]any, err error) {
	defer func() {
		if err != nil {
			out = nil
			if queryEngineLimit(err) {
				err = fmt.Errorf("%w: %v", ErrQueryLimit, err)
			}
		}
	}()
	if s.opts.QueryMaxBytes < 2 || s.opts.QueryMaxRows < 1 {
		return nil, ErrQueryLimit
	}
	ctx, cancel := s.withQueryTimeout(ctx)
	defer cancel()
	conn, err := s.rdb.Conn(ctx)
	if err != nil {
		return nil, err
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
			return nil, e
		}
		previous = append(previous, limit{bound.id, old})
	}
	// modernc steps the first row inside QueryContext; limits must already
	// be installed here, not merely before Rows.Next/Scan.
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out = make([]map[string]any, 0)
	resultBytes := 2 // JSON array brackets, including the empty-array case
	for rows.Next() {
		if len(out) == s.opts.QueryMaxRows {
			return nil, ErrQueryLimit
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = values[i]
		}
		encoded, e := json.Marshal(m)
		if e != nil {
			return nil, e
		}
		separator := 0
		if len(out) > 0 {
			separator = 1
		}
		if len(encoded) > s.opts.QueryMaxBytes-resultBytes-separator {
			return nil, ErrQueryLimit
		}
		resultBytes += len(encoded) + separator
		out = append(out, m)
	}
	return out, rows.Err()
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
	// so a column literally named "attach" isn't a false positive.
	up := strings.ToUpper(t)
	for _, kw := range []string{"ATTACH", "DETACH", "PRAGMA", "LOAD_EXTENSION"} {
		if containsWord(up, kw) {
			return errNotSelect
		}
	}
	return nil
}

// containsWord reports whether s contains the keyword kw surrounded by
// non-letter/non-digit boundaries (so "attachment" doesn't match "ATTACH").
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
		w.WriteHeader(status)
		fmt.Fprintf(w, "{\"error\":%s}\n", jsonString(message))
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		fail(http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s == nil {
		fail(http.StatusServiceUnavailable, "durable storage is disabled")
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		fail(http.StatusBadRequest, "missing q")
		return
	}
	if err := validateQuery(q); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.Query(r.Context(), q)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrQueryLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		fail(status, err.Error())
		return
	}
	data, err := json.Marshal(rows)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
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

func jsonString(s string) string {
	// Marshal of a plain string cannot fail.
	b, _ := json.Marshal(s)
	return string(b)
}
