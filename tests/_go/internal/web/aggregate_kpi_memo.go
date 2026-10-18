package web

import (
	"fmt"
	"path/filepath"
	"sync"
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
