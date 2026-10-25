// Package storage provides durable per-request storage using an embedded
// SQLite database. Records flow in asynchronously from the hot path via a
// channel, so DB writes never block the proxy.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"modernc.org/sqlite"
	"modernc.org/sqlite/lib"
)

// sqliteWriterConns is the SQLite single-writer pool size. Safety
// guardrail (avoids SQLITE_BUSY), not user-tunable.
const sqliteWriterConns = 1

// sqliteReadConns bounds the read-only pool. Each connection owns a SQLite
// page cache, so an unbounded pool multiplies memory under dashboard/MCP
// concurrency; reads beyond this bound queue behind the query timeout.
// Internal guardrail, not user-tunable.
const sqliteReadConns = 9

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

// ErrStorageDisabled is the one sentinel answering "no durable store is
// available" (a nil *Store, or no db_path): every surface that refuses an
// operation for that reason returns or wraps it, so callers classify with
// errors.Is instead of matching drifted wordings. Each surface keeps its own
// status code; only the identity and the canonical message are shared.
var ErrStorageDisabled = errors.New("durable storage is disabled")

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
