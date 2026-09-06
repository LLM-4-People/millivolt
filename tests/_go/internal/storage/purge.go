package storage

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"modernc.org/sqlite"
)

var purgeTestFunction atomic.Uint64

// Holding the producer fence must not stop an idle-purge writer from draining
// records that have already crossed that fence.
func TestWriterDrainsWithoutProducerLock(t *testing.T) {
	opts := testOpts
	opts.BatchCap = 1
	s := openPurgeTestStore(t, opts)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waits := s.db.Stats().WaitCount
	s.Record(&metrics.Record{ID: "first", Start: time.Now()})
	waitPurgeCondition(t, func() bool { return s.db.Stats().WaitCount > waits })
	s.Record(&metrics.Record{ID: "second", Start: time.Now()})
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for {
		n, err := s.CountWhere(ctx, PurgeFilter{})
		if err != nil {
			t.Fatalf("writer could not drain previously accepted records while producer lock held: %v", err)
		}
		if n == 2 {
			break
		}
		runtime.Gosched()
	}
}

func TestPendingPurgeFenceAcrossWriterDrains(t *testing.T) {
	for _, mode := range []string{"normal", "flush", "close"} {
		t.Run(mode, func(t *testing.T) {
			opts := testOpts
			opts.BatchCap = 1
			opts.WriteChanCap = 8
			path := filepath.Join(t.TempDir(), "drain.db")
			s, err := Open(path, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			b := metrics.NewBuffer(8)
			recorder := s.Recorder(b)
			conn, err := s.db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waits := s.db.Stats().WaitCount
			recorder.Record(&metrics.Record{ID: "old-writing", Start: time.Now()})
			waitPurgeCondition(t, func() bool { return s.db.Stats().WaitCount > waits })
			recorder.Record(&metrics.Record{ID: "old-queued", Start: time.Now()})
			purged := make(chan purgeOutcome, 1)
			go func() { got, err := s.Clear(t.Context(), nil, b); purged <- purgeOutcome{got, err} }()
			waitPurgeCondition(t, func() bool { return s.pendingPurge.Load() != nil })
			recorder.Record(&metrics.Record{ID: "new", Start: time.Now()})
			finished := make(chan error, 1)
			switch mode {
			case "flush":
				go func() { finished <- s.Flush() }()
			case "close":
				go func() { finished <- s.Close() }()
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			out := <-purged
			if out.err != nil || out.result.Deleted != 2 {
				t.Fatalf("purge=%+v err=%v", out.result, out.err)
			}
			if mode != "normal" {
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
			} else if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			if rows := b.Snapshot(); len(rows) != 1 || rows[0].ID != "new" {
				t.Fatalf("ring fence lost: %+v", rows)
			}
			if s.Totals().Requests != 1 {
				t.Fatalf("durable totals=%+v", s.Totals())
			}
			// An independent read-only connection verifies actual persisted IDs
			// even in the Close case, after both Store pools have been closed.
			durable, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			var n int
			var ids string
			if err := durable.QueryRowContext(t.Context(), "SELECT count(*), coalesce(group_concat(id), '') FROM requests").Scan(&n, &ids); err != nil || n != 1 || ids != "new" {
				t.Fatalf("durable fence count=%d IDs=%q error=%v", n, ids, err)
			}
		})
	}
}

func TestOverlappingPurgesKeepSeparateAcceptedBoundaries(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	name := fmt.Sprintf("purge_overlap_gate_%d", purgeTestFunction.Add(1))
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		enteredOnce.Do(func() { close(entered); <-release })
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	s := openPurgeTestStore(t, testOpts)
	b := metrics.NewBuffer(8)
	recorder := s.Recorder(b)
	write := func(id string) { recorder.Record(&metrics.Record{ID: id, Start: time.Now()}) }
	write("old")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER purge_overlap_gate BEFORE DELETE ON requests BEGIN SELECT " + name + "(); END"); err != nil {
		t.Fatal(err)
	}
	first := make(chan purgeOutcome, 1)
	go func() { got, err := s.Clear(t.Context(), nil, b); first <- purgeOutcome{got, err} }()
	select {
	case <-entered:
	case out := <-first:
		t.Fatalf("first purge returned before gate: %+v", out)
	case <-time.After(5 * time.Second):
		t.Fatal("first purge did not reach gate")
	}
	write("between")
	second := make(chan purgeOutcome, 1)
	starting := make(chan struct{})
	go func() { close(starting); got, err := s.Clear(t.Context(), nil, b); second <- purgeOutcome{got, err} }()
	<-starting
	unblock()
	for _, out := range []purgeOutcome{<-first, <-second} {
		if out.err != nil || out.result.Deleted != 1 {
			t.Fatalf("overlapping purge result=%+v error=%v", out.result, out.err)
		}
	}
	write("after")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadRecent(t.Context(), 8)
	if err != nil || len(rows) != 1 || rows[0].ID != "after" {
		t.Fatalf("durable fence rows=%+v error=%v", rows, err)
	}
	if ring := b.Snapshot(); len(ring) != 1 || ring[0].ID != "after" {
		t.Fatalf("ring fence rows=%+v", ring)
	}
	if s.pendingPurge.Load() != nil {
		t.Fatal("completed command retained")
	}
}

func waitPurgeCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for purge boundary")
		}
		runtime.Gosched()
	}
}

func openPurgeTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "purge.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestClearFencePreservesNewCompletionsAndDebugCaptures(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprint(filtered), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			name := fmt.Sprintf("purge_test_gate_%d", purgeTestFunction.Add(1))
			if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
				enteredOnce.Do(func() { close(entered) })
				<-release
				return int64(0), nil
			}); err != nil {
				t.Fatal(err)
			}
			opts := testOpts
			opts.FlushInterval = time.Hour
			s := openPurgeTestStore(t, opts)
			b := metrics.NewBuffer(4)
			rec := s.Recorder(b)
			for i := range 6 {
				provider := "target"
				if i%2 != 0 {
					provider = "keep"
				}
				rec.Record(&metrics.Record{ID: fmt.Sprintf("old-%d", i), Provider: provider, Start: time.Now(), StatusCode: 200})
			}
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"old-0", "new"} {
				if err := s.SaveDebugCapture(t.Context(), id, "session", 0, 0, []byte(`{"capture":true}`)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("CREATE TRIGGER purge_test_gate BEFORE DELETE ON requests BEGIN SELECT " + name + "(); END"); err != nil {
				t.Fatal(err)
			}
			var filter *PurgeFilter
			if filtered {
				filter = &PurgeFilter{Provider: "target"}
			}
			result := make(chan purgeOutcome, 1)
			go func() { got, err := s.Clear(t.Context(), filter, b); result <- purgeOutcome{got, err} }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not reach gate")
			}
			// SQL is deliberately blocked. A new completion must remain O(1),
			// land after the fence, and retain its previously saved capture.
			finished := make(chan struct{})
			go func() {
				rec.Record(&metrics.Record{ID: "new", Provider: "target", Start: time.Now(), StatusCode: 200})
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("Record blocked behind DELETE")
			}
			unblock()
			out := <-result
			if out.err != nil {
				t.Fatal(out.err)
			}
			wantDeleted := int64(6)
			if filtered {
				wantDeleted = 3
			}
			if out.result.Deleted != wantDeleted {
				t.Fatalf("deleted=%d want %d", out.result.Deleted, wantDeleted)
			}
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			rows, err := s.LoadRecent(t.Context(), 20)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if filtered {
				want = 4
			}
			if len(rows) != want || s.Totals().Requests != int64(want) {
				t.Fatalf("durable=%d totals=%+v", len(rows), s.Totals())
			}
			for _, row := range b.Snapshot() {
				if row.ID != "new" && (!filtered || row.Provider == "target") {
					t.Fatalf("old purged row survived: %s", row.ID)
				}
			}
			if !filtered && (b.Len() != 1 || b.Counters().TotalReq != 1) {
				t.Fatalf("full purge lost newer counter/row: %+v len=%d", b.Counters(), b.Len())
			}
			if raw, err := s.LoadDebugCapture(t.Context(), "old-0"); err != nil || len(raw) != 0 {
				t.Fatalf("old capture survived: %s %v", raw, err)
			}
			if raw, err := s.LoadDebugCapture(t.Context(), "new"); err != nil || len(raw) == 0 {
				t.Fatalf("in-flight capture deleted: %s %v", raw, err)
			}
		})
	}
}

func TestClearFenceDrainsQueuedWritesAndDroppedRingRows(t *testing.T) {
	opts := testOpts
	opts.WriteChanCap = 2
	opts.BatchCap = 1
	opts.FlushInterval = time.Hour
	s := openPurgeTestStore(t, opts)
	b := metrics.NewBuffer(8)
	rec := s.Recorder(b)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	beforeWait := s.db.Stats().WaitCount
	write := func(id string) { rec.Record(&metrics.Record{ID: id, Start: time.Now(), StatusCode: 200}) }
	write("old-writing")
	waitPurgeCondition(t, func() bool { return s.db.Stats().WaitCount > beforeWait })
	write("old-queued-1")
	write("old-queued-2")
	write("old-dropped")
	if s.Dropped() != 1 {
		t.Fatalf("expected one overflow, got %d", s.Dropped())
	}
	result := make(chan purgeOutcome, 1)
	go func() { got, err := s.Clear(t.Context(), nil, b); result <- purgeOutcome{got, err} }()
	waitPurgeCondition(t, func() bool { return s.pendingPurge.Load() != nil })
	write("new-dropped")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-result
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result.Deleted != 3 || out.result.BufferRemoved != 4 {
		t.Fatalf("purge boundary=%+v", out.result)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountWhere(t.Context(), PurgeFilter{}); err != nil || n != 0 {
		t.Fatalf("queued write resurrected: %d %v", n, err)
	}
	if rows := b.Snapshot(); len(rows) != 1 || rows[0].ID != "new-dropped" || b.Counters().TotalReq != 1 {
		t.Fatalf("new ring-only completion lost: %+v %+v", rows, b.Counters())
	}
}

func TestClearFailedDeleteRollsBackSidecarsAndRing(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	b := metrics.NewBuffer(4)
	s.Recorder(b).Record(&metrics.Record{ID: "old", Start: time.Now(), StatusCode: 500})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDebugCapture(t.Context(), "old", "session", 0, 0, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER deny_delete BEFORE DELETE ON requests BEGIN SELECT RAISE(ABORT, 'denied'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clear(t.Context(), nil, b); err == nil {
		t.Fatal("failed DELETE reported success")
	}
	if b.Len() != 1 || b.Counters().TotalReq != 1 || s.Totals().Requests != 1 {
		t.Fatal("failed DELETE changed ring/counters/totals")
	}
	if raw, err := s.LoadDebugCapture(t.Context(), "old"); err != nil || len(raw) == 0 {
		t.Fatalf("sidecar not rolled back: %s %v", raw, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Clear(ctx, nil, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled clear: %v", err)
	}
}

func BenchmarkRecorderPublication(b *testing.B) {
	for _, coupled := range []bool{false, true} {
		b.Run(fmt.Sprint(coupled), func(b *testing.B) {
			// No writer: benchmark the bounded overflow path shared by actual
			// overload and isolate publication from asynchronous SQLite work.
			s := &Store{ch: make(chan queuedRecord, 1), done: make(chan struct{})}
			ring := metrics.NewBuffer(10_000)
			var recorder metrics.Recorder = metrics.MultiRecorder{ring, s}
			if coupled {
				recorder = s.Recorder(ring)
			}
			r := &metrics.Record{ID: "benchmark", StatusCode: 200}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				recorder.Record(r)
			}
		})
	}
}

func TestFlushWaitsForConcurrentCloseDrain(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			opts := testOpts
			opts.BatchCap = 1
			s := openPurgeTestStore(t, opts)
			if fail {
				if _, err := s.db.Exec(`CREATE TRIGGER deny_insert BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'denied'); END`); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := s.db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waits := s.db.Stats().WaitCount
			s.Record(&metrics.Record{ID: "queued", Start: time.Now()})
			waitPurgeCondition(t, func() bool { return s.db.Stats().WaitCount > waits })
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			<-s.done
			flushed := make(chan error, 1)
			go func() { flushed <- s.Flush() }()
			select {
			case err := <-flushed:
				t.Fatalf("Flush returned before blocked final drain: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-flushed; (err != nil) != fail {
				t.Fatalf("Flush error=%v, fail=%v", err, fail)
			}
			if err := <-closed; (err != nil) != fail {
				t.Fatalf("Close error=%v, fail=%v", err, fail)
			}
		})
	}
}

func TestClearCancellationDuringDeleteRollsBack(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	name := fmt.Sprintf("purge_test_gate_%d", purgeTestFunction.Add(1))
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(entered)
		<-release
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	s := openPurgeTestStore(t, testOpts)
	b := metrics.NewBuffer(4)
	s.Recorder(b).Record(&metrics.Record{ID: "old", Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDebugCapture(t.Context(), "old", "session", 0, 0, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER purge_test_gate BEFORE DELETE ON requests BEGIN SELECT " + name + "(); END"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := s.Clear(ctx, nil, b); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not reach gate")
	}
	cancel()
	unblock()
	if err := <-result; err == nil {
		t.Fatal("canceled delete reported success")
	}
	if n, err := s.CountWhere(t.Context(), PurgeFilter{}); err != nil || n != 1 || b.Len() != 1 || s.Totals().Requests != 1 {
		t.Fatalf("canceled delete changed state: %d %v", n, err)
	}
	if raw, err := s.LoadDebugCapture(t.Context(), "old"); err != nil || len(raw) == 0 {
		t.Fatalf("canceled delete lost capture: %s %v", raw, err)
	}
}

var (
	exportReadFailure    = errors.New("injected export row read failure")
	exportReadDriverOnce sync.Once
)

// Inject a deterministic database/sql Rows error, independently of SQLite's
// asynchronous cancellation watcher (a complete result may legitimately beat it).
type exportReadErrorDriver struct{}
type exportReadErrorConn struct{}
type exportReadErrorRows struct{}

func (exportReadErrorDriver) Open(string) (driver.Conn, error) { return exportReadErrorConn{}, nil }
func (exportReadErrorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (exportReadErrorConn) Close() error { return nil }
func (exportReadErrorConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions unsupported")
}
func (exportReadErrorConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return exportReadErrorRows{}, nil
}
func (exportReadErrorRows) Columns() []string         { return []string{"id"} }
func (exportReadErrorRows) Close() error              { return nil }
func (exportReadErrorRows) Next([]driver.Value) error { return exportReadFailure }

func TestExportFailureNeverClosesIncompleteJSONArray(t *testing.T) {
	const driverName = "millivolt-export-read-error"
	exportReadDriverOnce.Do(func() { sql.Register(driverName, exportReadErrorDriver{}) })
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{rdb: db}
	var w bytes.Buffer
	if _, err := s.ExportWhere(t.Context(), PurgeFilter{}, &w); !errors.Is(err, exportReadFailure) {
		t.Fatalf("row read failure was not propagated: %v", err)
	}
	if json.Valid(w.Bytes()) {
		t.Fatal("incomplete export masquerades as a complete JSON array")
	}
	if got := w.String(); got != "[\n" {
		t.Fatalf("unexpected partial export %q", got)
	}
}
