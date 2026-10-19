package web

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// kpiMemoStore opens a store with a long automatic flush interval so each test
// owns the exact durable commit boundary (Flush/Clear/merge/MarkWritten).
func kpiMemoStore(t *testing.T) *storage.Store {
	t.Helper()
	d := config.Default()
	s, err := storage.Open(filepath.Join(t.TempDir(), "kpi-memo.db"), storage.Options{
		WriteChanCap:  4096, // hold every test record so the concurrent case never drops
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Hour,
		QueryTimeout:  time.Second,
		WriteTrackCap: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// checkKPI asserts the exact KPI count and whether this call had to refold
// (a fresh single-slot entry) or reused the memoized totals.
func checkKPI(t *testing.T, api *AggAPI, want int64, fresh bool) {
	t.Helper()
	before := api.kpiMemo.entry
	p := api.kpi()
	if p.err != nil {
		t.Fatalf("kpi err = %v, want nil", p.err)
	}
	if p.Requests != want {
		t.Fatalf("kpi requests = %d, want %d", p.Requests, want)
	}
	changed := api.kpiMemo.entry != before
	if fresh && !changed {
		t.Fatalf("kpi memo was not invalidated (requests=%d)", want)
	}
	if !fresh && changed {
		t.Fatalf("kpi memo was unexpectedly invalidated (requests=%d)", want)
	}
}

// Every owner that can change the KPI fold input must invalidate the single
// slot. Ring changes travel through Buffer's finalized revision; durable totals
// and written-id changes travel through Store's accounting revision.
func TestKPIMemoInvalidatesEveryPath(t *testing.T) {
	now := time.Now()

	t.Run("store_record_and_flush", func(t *testing.T) {
		s := kpiMemoStore(t)
		buf := metrics.NewBuffer(8)
		api := NewAggAPI(buf, s, time.Second)
		checkKPI(t, api, 0, true)
		checkKPI(t, api, 0, false)

		finalBefore, pendingBefore := buf.Revisions()
		s.Record(mkRec("durable", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01))
		// The record is only enqueued: no durable accounting has changed, so
		// the exact memo must still hold.
		checkKPI(t, api, 0, false)
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		finalAfter, pendingAfter := buf.Revisions()
		if finalAfter != finalBefore || pendingAfter != pendingBefore {
			t.Fatalf("direct store commit moved ring revisions: (%d,%d) -> (%d,%d)",
				finalBefore, pendingBefore, finalAfter, pendingAfter)
		}
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
	})

	t.Run("flush_dedupes_ring_without_reusing_old_totals", func(t *testing.T) {
		s := kpiMemoStore(t)
		buf := metrics.NewBuffer(8)
		api := NewAggAPI(buf, s, time.Second)
		checkKPI(t, api, 0, true)

		rec := mkRec("live-flush", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01)
		buf.Record(rec)
		finalBefore, _ := buf.Revisions()
		checkKPI(t, api, 1, true) // ring-only contribution
		checkKPI(t, api, 1, false)

		s.Record(rec) // enqueued, not durable yet: the ring still supplies it
		checkKPI(t, api, 1, false)
		revBefore := s.AccountingRevision()
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		if rev := s.AccountingRevision(); rev <= revBefore {
			t.Fatalf("flush did not advance accounting revision: %d -> %d", revBefore, rev)
		}
		// Same KPI value, different dedupe state: a hit here would hide the
		// durable row if the ring copy later evicted.
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
		finalAfter, _ := buf.Revisions()
		if finalAfter != finalBefore {
			t.Fatalf("flush changed ring revision: %d -> %d", finalBefore, finalAfter)
		}
	})

	t.Run("purge", func(t *testing.T) {
		s := kpiMemoStore(t)
		a := mkRec("purge-a", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01)
		a.Provider = "p"
		b := mkRec("purge-b", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01)
		b.Provider = "q"
		s.Record(a)
		s.Record(b)
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		buf := metrics.NewBuffer(8)
		api := NewAggAPI(buf, s, time.Second)
		finalBefore, pendingBefore := buf.Revisions()
		checkKPI(t, api, 2, true)
		checkKPI(t, api, 2, false)

		// A filtered purge republishes durable totals through installTotals.
		if _, err := s.Clear(t.Context(), &storage.PurgeFilter{Provider: "p"}, nil); err != nil {
			t.Fatal(err)
		}
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
		if _, err := s.Clear(t.Context(), nil, nil); err != nil {
			t.Fatal(err)
		}
		checkKPI(t, api, 0, true)
		finalAfter, pendingAfter := buf.Revisions()
		if finalAfter != finalBefore || pendingAfter != pendingBefore {
			t.Fatalf("store-only purge moved ring revisions: (%d,%d) -> (%d,%d)",
				finalBefore, pendingBefore, finalAfter, pendingAfter)
		}
	})

	t.Run("merge", func(t *testing.T) {
		src := kpiMemoStore(t)
		src.Record(mkRec("merged", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01))
		if err := src.Flush(); err != nil {
			t.Fatal(err)
		}
		data, err := src.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		s := kpiMemoStore(t)
		buf := metrics.NewBuffer(8)
		api := NewAggAPI(buf, s, time.Second)
		finalBefore, _ := buf.Revisions()
		checkKPI(t, api, 0, true)
		inserted, _, err := s.MergeSnapshot(t.Context(), data)
		if err != nil {
			t.Fatal(err)
		}
		if inserted != 1 {
			t.Fatalf("merge inserted %d, want 1", inserted)
		}
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
		finalAfter, _ := buf.Revisions()
		if finalAfter != finalBefore {
			t.Fatalf("merge moved ring revision: %d -> %d", finalBefore, finalAfter)
		}
	})

	t.Run("backfill", func(t *testing.T) {
		s := kpiMemoStore(t)
		buf := metrics.NewBuffer(8)
		api := NewAggAPI(buf, s, time.Second)
		checkKPI(t, api, 0, true)
		buf.Backfill([]*metrics.Record{mkRec("backfilled", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01)})
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
	})

	t.Run("reset", func(t *testing.T) {
		s := kpiMemoStore(t)
		s.Record(mkRec("reset-durable", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01))
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		buf := metrics.NewBuffer(8)
		buf.Record(mkRec("reset-live", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01))
		api := NewAggAPI(buf, s, time.Second)
		checkKPI(t, api, 2, true)
		checkKPI(t, api, 2, false)
		buf.Reset()
		checkKPI(t, api, 1, true) // the durable row survives the ring reset
		checkKPI(t, api, 1, false)
	})

	t.Run("mark_written", func(t *testing.T) {
		s := kpiMemoStore(t)
		s.Record(mkRec("written-durable", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01))
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		buf := metrics.NewBuffer(8)
		extra := mkRec("written-prime", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01)
		buf.Record(extra)
		api := NewAggAPI(buf, s, time.Second)
		checkKPI(t, api, 2, true)
		checkKPI(t, api, 2, false)
		// Priming the written set changes the ring/stored dedupe boundary even
		// though no totals or ring revision moves.
		s.MarkWritten([]string{extra.ID})
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
	})

	t.Run("ring_only", func(t *testing.T) {
		buf := metrics.NewBuffer(8)
		api := NewAggAPI(buf, nil, time.Second)
		checkKPI(t, api, 0, true)
		buf.Record(mkRec("ring-only", now, 200, "", nil, 10, 100, 10, 5, 0, 0.01))
		checkKPI(t, api, 1, true)
		checkKPI(t, api, 1, false)
		buf.RemoveWhere(func(r *metrics.Record) bool { return r.ID == "ring-only" })
		checkKPI(t, api, 0, true)
	})
}

// A memo hit retains only the folded totals: the live in-flight gauge must be
// filled from current counters on every serve, exactly like a fresh fold.
func TestKPIMemoHitKeepsLiveInFlight(t *testing.T) {
	buf := metrics.NewBuffer(8)
	api := NewAggAPI(buf, nil, time.Second)
	checkKPI(t, api, 0, true)
	checkKPI(t, api, 0, false)
	buf.PublishLive("begin", &metrics.Record{ID: "in-flight"})
	if p := api.kpi(); p.InFlight != 1 {
		t.Fatalf("memoized kpi in_flight = %d, want the live gauge 1", p.InFlight)
	}
	buf.Record(&metrics.Record{ID: "in-flight", StatusCode: 200, Start: time.Now()})
	if p := api.kpi(); p.InFlight != 0 || p.Requests != 1 {
		t.Fatalf("post-completion kpi = in_flight %d requests %d, want 0/1", p.InFlight, p.Requests)
	}
}

// kpiPayloadEqual compares the observable totals by value: the derived
// pointers are allocated per call, so pointer identity is not meaningful.
func kpiPayloadEqual(a, b kpiPayload) bool {
	eq := func(x, y *float64) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return *x == *y
	}
	if a.err != nil || b.err != nil {
		return false
	}
	return a.Requests == b.Requests && a.Errors == b.Errors && a.InFlight == b.InFlight &&
		a.Cost == b.Cost && eq(a.CostPerReq, b.CostPerReq) && eq(a.CostPerMTok, b.CostPerMTok) &&
		a.InputTok == b.InputTok && a.OutputTok == b.OutputTok && a.CacheRead == b.CacheRead &&
		a.Reasoning == b.Reasoning && eq(a.AvgTTFT, b.AvgTTFT) && eq(a.AvgTPS, b.AvgTPS)
}

// Concurrent cold callers for one exact key must share one fold. The test
// owner holds the leader in flight while the other callers arrive; without the
// fold owner each caller would enter its own compute.
func TestKPIMemoFoldCoalescesSameKeyColdFolds(t *testing.T) {
	buf := metrics.NewBuffer(8)
	for i := range 3 {
		buf.Record(mkRec(fmt.Sprintf("coalesce-%d", i), time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.01))
	}
	api := NewAggAPI(buf, nil, time.Second)

	const callers = 8
	started := make(chan struct{})
	release := make(chan struct{})
	var hold sync.Once
	var hookCalls atomic.Int32
	api.kpiMemo.testFold = func(kpiMemoKey) {
		hookCalls.Add(1)
		hold.Do(func() { close(started) })
		<-release
	}

	key := api.kpiMemoKey()
	start := make(chan struct{})
	results := make(chan kpiPayload, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			results <- api.kpi()
		}()
	}
	close(start)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cold fold did not start")
	}
	// The leader is held inside the fold, so no other caller may have entered
	// it: every same-key caller is waiting on the fold owner. The observed key
	// is still the cold key the leader was asked to fold.
	if got := api.kpiMemoKey(); got != key {
		t.Fatalf("fold key = %+v, want %+v", got, key)
	}
	if got := api.kpiMemo.folds; got != 1 {
		t.Fatalf("concurrent folds = %d, want 1", got)
	}
	close(release)
	wg.Wait()
	close(results)

	if got := api.kpiMemo.folds; got != 1 {
		t.Fatalf("cold folds = %d, want 1", got)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Fatalf("fold hook calls = %d, want 1", got)
	}
	var want kpiPayload
	first := true
	for got := range results {
		if got.Requests != 3 || got.InputTok != 30 || got.OutputTok != 15 {
			t.Fatalf("kpi totals = requests %d in %d out %d, want 3/30/15",
				got.Requests, got.InputTok, got.OutputTok)
		}
		if first {
			want, first = got, false
			continue
		}
		if !kpiPayloadEqual(got, want) {
			t.Fatalf("concurrent callers received different KPI payloads: %+v vs %+v", got, want)
		}
	}
}

// A waiter that arrives while the leader holds the fold must not return until
// the leader publishes. kpi() has no cancellation parameter, so there is no
// opt-out path to assert: the waiter simply blocks on the fold owner, then
// reuses the leader's memoized totals.
func TestKPIMemoWaiterBlocksUntilLeaderPublishes(t *testing.T) {
	buf := metrics.NewBuffer(8)
	buf.Record(mkRec("waiter", time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.01))
	api := NewAggAPI(buf, nil, time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var hold sync.Once
	api.kpiMemo.testFold = func(kpiMemoKey) {
		hold.Do(func() { close(started) })
		<-release
	}

	leaderDone := make(chan kpiPayload, 1)
	go func() { leaderDone <- api.kpi() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not enter the fold")
	}

	waiterStarted := make(chan struct{})
	waiterDone := make(chan kpiPayload, 1)
	go func() {
		close(waiterStarted)
		waiterDone <- api.kpi()
	}()
	<-waiterStarted
	select {
	case got := <-waiterDone:
		t.Fatalf("waiter returned before the fold was released: requests=%d", got.Requests)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	leader := <-leaderDone
	waiter := <-waiterDone
	if leader.Requests != 1 || waiter.Requests != 1 {
		t.Fatalf("requests leader=%d waiter=%d, want 1/1", leader.Requests, waiter.Requests)
	}
	if !kpiPayloadEqual(leader, waiter) {
		t.Fatalf("waiter did not receive the leader result: %+v vs %+v", waiter, leader)
	}
	if got := api.kpiMemo.folds; got != 1 {
		t.Fatalf("cold folds = %d, want 1", got)
	}
}

// Different exact keys are not coalesced or reused: each performs its own
// fold even though the fold owner serializes them.
func TestKPIMemoDifferentKeysFoldOnceEach(t *testing.T) {
	buf := metrics.NewBuffer(8)
	api := NewAggAPI(buf, nil, time.Second)
	if got := api.kpi(); got.Requests != 0 || api.kpiMemo.folds != 1 {
		t.Fatalf("initial kpi = requests %d folds %d, want 0/1", got.Requests, api.kpiMemo.folds)
	}
	firstKey := api.kpiMemoKey()
	buf.Record(mkRec("second-key", time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.01))
	secondKey := api.kpiMemoKey()
	if firstKey == secondKey {
		t.Fatal("ring commit did not change the memo key")
	}
	got := api.kpi()
	if got.Requests != 1 {
		t.Fatalf("second-key requests = %d, want 1", got.Requests)
	}
	if api.kpiMemo.folds != 2 {
		t.Fatalf("different-key folds = %d, want 2", api.kpiMemo.folds)
	}
	if _, ok := api.kpiMemo.get(firstKey); ok {
		t.Fatal("the old key was reused for the new totals")
	}
	if totals, ok := api.kpiMemo.get(secondKey); !ok || totals.Requests != 1 {
		t.Fatalf("new-key memo = %d/%v, want requests 1", totals.Requests, ok)
	}
	if got := api.kpi(); got.Requests != 1 {
		t.Fatalf("memo-hit requests = %d, want 1", got.Requests)
	}
	if api.kpiMemo.folds != 2 {
		t.Fatalf("memo hit folded again: folds = %d, want 2", api.kpiMemo.folds)
	}
}

// A commit landing mid-fold must prevent publication under the pre-commit key,
// and every concurrent caller must still converge on the committed totals.
func TestKPIMemoMidFoldCommitIsNotPublishedUnderOldKey(t *testing.T) {
	buf := metrics.NewBuffer(8)
	buf.Record(mkRec("before-fold", time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.01))
	api := NewAggAPI(buf, nil, time.Second)
	oldKey := api.kpiMemoKey()

	started := make(chan struct{})
	release := make(chan struct{})
	var hold sync.Once
	api.kpiMemo.testFold = func(kpiMemoKey) {
		hold.Do(func() { close(started) })
		<-release
	}

	const callers = 8
	start := make(chan struct{})
	results := make(chan kpiPayload, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			results <- api.kpi()
		}()
	}
	close(start)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cold fold did not start")
	}

	// Commit while the leader is held before its snapshot.
	buf.Record(mkRec("during-fold", time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.01))
	newKey := api.kpiMemoKey()
	if newKey == oldKey {
		t.Fatal("mid-fold commit did not change the memo key")
	}
	close(release)
	wg.Wait()
	close(results)

	if _, ok := api.kpiMemo.get(oldKey); ok {
		t.Fatal("stale KPI totals were published under the pre-commit key")
	}
	for got := range results {
		if got.Requests != 2 {
			t.Fatalf("mid-fold caller requests = %d, want 2", got.Requests)
		}
	}
	// A caller at the settled key computes and publishes the final totals.
	if got := api.kpi(); got.Requests != 2 {
		t.Fatalf("settled kpi requests = %d, want 2", got.Requests)
	}
	if totals, ok := api.kpiMemo.get(api.kpiMemoKey()); !ok || totals.Requests != 2 {
		t.Fatalf("settled memo = %d/%v, want requests 2", totals.Requests, ok)
	}
}

// Commits racing the fold must never publish stale totals under a fresh key.
// The final settled poll always converges on the exact durable+ring count.
func TestKPIMemoConcurrentCommitsConverge(t *testing.T) {
	s := kpiMemoStore(t)
	buf := metrics.NewBuffer(512)
	api := NewAggAPI(buf, s, time.Second)
	checkKPI(t, api, 0, true)

	const writers = 8
	const perWriter = 32
	var wg sync.WaitGroup
	done := make(chan struct{})
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				rec := mkRec(fmt.Sprintf("race-%d-%d", w, i), time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.01)
				buf.Record(rec)
				s.Record(rec)
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-done:
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			checkKPI(t, api, writers*perWriter, true)
			checkKPI(t, api, writers*perWriter, false)
			return
		default:
			_ = api.kpi()
		}
	}
}
