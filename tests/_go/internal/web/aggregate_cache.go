package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func cacheChartRequests(p chartPayload) int64 {
	var n int64
	for _, b := range p.Buckets {
		n += b.Req
	}
	return n
}

func cacheRecord(id string, at time.Time) *metrics.Record {
	r := mkRec(id, at, 200, "", nil, 10, 100, 10, 5, 0, 0.01)
	r.Provider, r.Model = "old.example", "MODEL-A"
	return r
}

func TestAggregateMemoReusesIdenticalResults(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	fs := []scopeFilter{{dim: "provider", id: "old.example"}, {dim: "status", id: "2xx"}}
	reversed := []scopeFilter{fs[1], fs[0]}
	p, err := api.chart(t.Context(), 0, fs, "", now.UnixMilli())
	if err != nil || cacheChartRequests(p) != 1 {
		t.Fatalf("chart = %+v, %v", p, err)
	}
	chartEntry := api.chartMemo.entry
	p2, err := api.chart(t.Context(), 0, reversed, "", now.Add(time.Second).UnixMilli())
	if err != nil || api.chartMemo.entry != chartEntry {
		t.Fatalf("identical chart refolded: %v", err)
	}
	if p2.NowMs == p.NowMs || chartEntry.value.payload.NowMs != p.NowMs || !reflect.DeepEqual(p.Buckets, p2.Buckets) {
		t.Fatal("chart hit must update only its copied clock metadata")
	}
	e, err := api.explorer(t.Context(), "provider", fs, "")
	if err != nil || e.Total != 1 {
		t.Fatalf("explorer = %+v, %v", e, err)
	}
	explorerEntry := api.explorerMemo.entry
	e2, err := api.explorer(t.Context(), "provider", reversed, "")
	if err != nil || api.explorerMemo.entry != explorerEntry || !reflect.DeepEqual(e, e2) {
		t.Fatalf("identical explorer refolded: %v", err)
	}
	if _, err := api.chart(t.Context(), 60, fs, "", now.UnixMilli()); err != nil || api.chartMemo.entry == chartEntry {
		t.Fatalf("different window reused old entry: %v", err)
	}
	if _, err := api.explorer(t.Context(), "model", fs, ""); err != nil || api.explorerMemo.entry == explorerEntry {
		t.Fatalf("different dimension reused old entry: %v", err)
	}
}

func TestAggregateMemoInvalidatesBufferAndPending(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	buf := metrics.NewBuffer(1)
	buf.Record(cacheRecord("a", now))
	api := NewAggAPI(buf, nil, time.Second)
	refresh := func(wantFinal, wantAll int64) {
		t.Helper()
		p, err := api.chart(t.Context(), 60, nil, "", now.Add(time.Minute).UnixMilli())
		if err != nil || cacheChartRequests(p) != wantFinal {
			t.Fatalf("chart requests = %d, %v; want %d", cacheChartRequests(p), err, wantFinal)
		}
		e, err := api.explorer(t.Context(), "status", nil, "")
		if err != nil || e.Total != wantAll {
			t.Fatalf("explorer requests = %d, %v; want %d", e.Total, err, wantAll)
		}
	}
	refresh(1, 1)
	chartEntry, explorerEntry := api.chartMemo.entry, api.explorerMemo.entry
	for _, phase := range []string{"begin", "update"} {
		r := cacheRecord("pending", now)
		r.Stream, r.Paused = true, phase == "update"
		buf.PublishLive(phase, r)
		refresh(1, 2)
		if api.chartMemo.entry != chartEntry || api.explorerMemo.entry == explorerEntry {
			t.Fatalf("%s must invalidate explorer but not finalized chart", phase)
		}
		explorerEntry = api.explorerMemo.entry
	}
	buf.Record(cacheRecord("pending", now)) // completion retires pending atomically
	refresh(1, 1)
	if api.chartMemo.entry == chartEntry || api.explorerMemo.entry == explorerEntry {
		t.Fatal("completion must invalidate both finalized and pending views")
	}
	chartEntry = api.chartMemo.entry
	buf.Record(cacheRecord("b", now)) // replacement keeps count, changes revision
	refresh(1, 1)
	if api.chartMemo.entry == chartEntry {
		t.Fatal("ring eviction reused old chart")
	}
	buf.RemoveWhere(func(*metrics.Record) bool { return true })
	refresh(0, 0)
	buf.Backfill([]*metrics.Record{cacheRecord("c", now)})
	refresh(1, 1)
	buf.Reset()
	refresh(0, 0)
}

func TestAggregateMemoInvalidatesDatabaseMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	d := config.Default()
	s, err := storage.Open(path, storage.Options{WriteChanCap: d.StorageWriteChanCap,
		BatchCap: d.StorageBatchCap, FlushInterval: d.StorageFlushInterval, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.Record(cacheRecord("stored", now.Add(-time.Minute)))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(8), s, time.Second)
	refresh := func(n int64, provider string) {
		t.Helper()
		p, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli())
		if err != nil || cacheChartRequests(p) != n {
			t.Fatalf("chart = %d, %v; want %d", cacheChartRequests(p), err, n)
		}
		e, err := api.explorer(t.Context(), "provider", nil, "")
		if err != nil || e.Total != n || (n > 0 && e.Groups[0].Name != provider) {
			t.Fatalf("explorer = %+v, %v", e, err)
		}
	}
	refresh(1, "old.example")
	first := api.chartMemo.entry
	if _, err := s.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"}); err != nil {
		t.Fatal(err)
	}
	refresh(1, "new.example")
	if api.chartMemo.entry == first {
		t.Fatal("provider rename retained chart cache")
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec("UPDATE requests SET cost = ? WHERE id = ?", 0.75, "stored"); err != nil {
		t.Fatal(err)
	}
	refresh(1, "new.example")
	if cost := api.explorerMemo.entry.value.Groups[0].Cost; cost != 0.75 {
		t.Fatalf("external writer invisible to cache: cost %v, want 0.75", cost)
	}
	if _, err := s.Clear(t.Context(), &storage.PurgeFilter{Provider: "new.example"}, nil); err != nil {
		t.Fatal(err)
	}
	refresh(0, "")
	s.Record(cacheRecord("new", now.Add(-time.Minute)))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	refresh(1, "old.example")
	if _, err := s.Clear(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	refresh(0, "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli()); err == nil {
		t.Fatal("closed database reused cached chart")
	}
	if _, err := api.explorer(t.Context(), "provider", nil, ""); err == nil {
		t.Fatal("closed database reused cached explorer")
	}
}

func TestAggregateMemoClockGeometry(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	for _, win := range []int{0, 60} {
		p, err := api.chart(t.Context(), win, nil, "", now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		entry := api.chartMemo.entry
		after := now.Add(time.Duration(p.BucketMs) * time.Millisecond)
		p2, err := api.chart(t.Context(), win, nil, "", after.UnixMilli())
		if err != nil || api.chartMemo.entry == entry || cacheChartRequests(p2) != 1 {
			t.Fatalf("window %d missed clock boundary invalidation: %v", win, err)
		}
	}
	buf.Reset()
	if _, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	p, err := api.chart(t.Context(), 0, nil, "", now.Add(3*time.Hour).UnixMilli())
	if err != nil || len(p.Buckets) != 1 || p.FromMs < now.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("empty All range kept an obsolete minimum: %+v, %v", p, err)
	}
	buf.Record(cacheRecord("future", now.Add(time.Hour)))
	entry := api.chartMemo.entry
	if _, err := api.chart(t.Context(), 0, nil, "", now.UnixMilli()); err != nil || api.chartMemo.entry != entry {
		t.Fatalf("future-clamped All window must not become a cache entry: %v", err)
	}
}

func TestAggregateMemoSnapshotRulesAndMidScanMutation(t *testing.T) {
	for _, surface := range []string{"chart", "explorer"} {
		t.Run(surface, func(t *testing.T) {
			now := time.Now()
			buf := metrics.NewBuffer(8)
			buf.Record(cacheRecord("first", now.Add(-time.Minute)))
			api := NewAggAPI(buf, nil, time.Second)
			calls := 0
			api.ModelCanon = func() config.ModelCanon {
				calls++
				if calls == 2 {
					buf.Record(cacheRecord("racing", now.Add(-time.Minute)))
				}
				return config.ModelCanon{}
			}
			if surface == "chart" {
				p, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli())
				if err != nil || cacheChartRequests(p) != 1 || api.chartMemo.entry != nil {
					t.Fatalf("racing chart was memoized: %+v, %v", p, err)
				}
			} else {
				p, err := api.explorer(t.Context(), "model", nil, "")
				if err != nil || p.Total != 1 || api.explorerMemo.entry != nil {
					t.Fatalf("racing explorer was memoized: %+v, %v", p, err)
				}
			}
		})
	}
	now := time.Now()
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now))
	buf.PublishLive("begin", cacheRecord("pending", now))
	api := NewAggAPI(buf, nil, time.Second)
	calls := 0
	api.ModelCanon = func() config.ModelCanon {
		calls++
		if calls == 1 {
			return config.ModelCanon{}
		}
		return config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	}
	e, err := api.explorer(t.Context(), "model", nil, "")
	if err != nil || len(e.Groups) != 1 || e.Groups[0].Name != "MODEL-A" || e.Total != 2 || api.explorerMemo.entry != nil {
		t.Fatalf("one fold mixed canonicalization snapshots: %+v, %v", e, err)
	}
	e, err = api.explorer(t.Context(), "model", nil, "")
	if err != nil || len(e.Groups) != 1 || e.Groups[0].Name != "model-a" || api.explorerMemo.entry == nil {
		t.Fatalf("new canonicalization was not applied: %+v, %v", e, err)
	}
}

func TestAggregateMemoCancellationBoundAndConcurrentReaders(t *testing.T) {
	now := time.Now()
	buf := metrics.NewBuffer(64)
	for i := range 48 {
		r := cacheRecord(fmt.Sprint(i), now.Add(-time.Minute))
		r.Provider = fmt.Sprintf("group-%d.example", i)
		buf.Record(r)
	}
	api := NewAggAPI(buf, nil, time.Second)
	e, err := api.explorer(t.Context(), "provider", nil, "")
	if err != nil || len(e.Groups) != xpNodeCap || cap(e.Groups) != xpNodeCap {
		t.Fatalf("cached groups retain discarded history: len/cap=%d/%d, %v", len(e.Groups), cap(e.Groups), err)
	}
	if _, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := api.explorer(ctx, "provider", nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled explorer request = %v", err)
	}
	if _, err := api.chart(ctx, 60, nil, "", now.UnixMilli()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled chart request = %v", err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for range 8 {
				p, err := api.chart(t.Context(), 60, nil, "", now.Add(time.Duration(i)*time.Millisecond).UnixMilli())
				if err != nil || cacheChartRequests(p) != 48 {
					t.Errorf("concurrent chart = %d, %v", cacheChartRequests(p), err)
				}
				e, err := api.explorer(t.Context(), "provider", nil, "")
				if err != nil || e.Total != 48 {
					t.Errorf("concurrent explorer = %d, %v", e.Total, err)
				}
			}
		})
	}
	wg.Wait()
}

// flightProbe sequences the in-flight coalescing tests without timing
// assumptions: the first cold fold blocks until release, and the first
// same-key waiter announces itself through joined.
type flightProbe struct {
	cold    atomic.Int64
	waits   atomic.Int64
	started chan struct{}
	release chan struct{}
	joined  chan struct{}
}

func newFlightProbe() *flightProbe {
	return &flightProbe{started: make(chan struct{}), release: make(chan struct{}), joined: make(chan struct{})}
}

func (p *flightProbe) hooks() *aggregateTestHooks {
	var joinedOnce sync.Once
	return &aggregateTestHooks{
		cold: func() {
			if p.cold.Add(1) == 1 {
				close(p.started)
				<-p.release
			}
		},
		wait: func() {
			p.waits.Add(1)
			joinedOnce.Do(func() { close(p.joined) })
		},
	}
}

func awaitFlightSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitFlightJoin(t *testing.T, probe *flightProbe, release func()) {
	t.Helper()
	select {
	case <-probe.joined:
	case <-time.After(5 * time.Second):
		release()
		t.Fatalf("same-key caller did not join the in-flight fold (cold calls = %d)", probe.cold.Load())
	}
}

func assertFlightComplete[T any](t *testing.T, m *aggregateMemo[T]) {
	t.Helper()
	m.mu.Lock()
	published, inFlight := m.entry != nil, m.flight != nil
	m.mu.Unlock()
	if !published || inFlight {
		t.Fatalf("memo published/flight = %v/%v, want true/false", published, inFlight)
	}
}

func TestAggregateFlightCoalescesChart(t *testing.T) {
	for _, winMin := range []int{0, 60} {
		t.Run(fmt.Sprintf("window_%d", winMin), func(t *testing.T) {
			now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
			buf := metrics.NewBuffer(8)
			buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
			api := NewAggAPI(buf, nil, time.Second)
			probe := newFlightProbe()
			api.testHooks = probe.hooks()

			var wg sync.WaitGroup
			payloads := make([]chartPayload, 2)
			errs := make([]error, 2)
			wg.Go(func() { payloads[0], errs[0] = api.chart(t.Context(), winMin, nil, "", now.UnixMilli()) })
			awaitFlightSignal(t, probe.started, "leader chart fold")
			wg.Go(func() { payloads[1], errs[1] = api.chart(t.Context(), winMin, nil, "", now.UnixMilli()) })
			awaitFlightJoin(t, probe, func() { close(probe.release); wg.Wait() })
			close(probe.release)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("chart %d: %v", i, err)
				}
			}
			if got := probe.cold.Load(); got != 1 {
				t.Fatalf("cold folds = %d, want 1", got)
			}
			if !reflect.DeepEqual(payloads[0], payloads[1]) {
				t.Fatalf("coalesced charts differ:\n%+v\n%+v", payloads[0], payloads[1])
			}
			if got := cacheChartRequests(payloads[1]); got != 1 {
				t.Fatalf("chart requests = %d, want 1", got)
			}
			assertFlightComplete(t, &api.chartMemo)
		})
	}
}

func TestAggregateFlightCoalescesExplorer(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	probe := newFlightProbe()
	api.testHooks = probe.hooks()

	var wg sync.WaitGroup
	payloads := make([]explorerPayload, 2)
	errs := make([]error, 2)
	wg.Go(func() { payloads[0], errs[0] = api.explorer(t.Context(), "provider", nil, "") })
	awaitFlightSignal(t, probe.started, "leader explorer fold")
	wg.Go(func() { payloads[1], errs[1] = api.explorer(t.Context(), "provider", nil, "") })
	awaitFlightJoin(t, probe, func() { close(probe.release); wg.Wait() })
	close(probe.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("explorer %d: %v", i, err)
		}
	}
	if got := probe.cold.Load(); got != 1 {
		t.Fatalf("cold folds = %d, want 1", got)
	}
	if payloads[0].Total != 1 || !reflect.DeepEqual(payloads[0], payloads[1]) {
		t.Fatalf("coalesced explorers differ:\n%+v\n%+v", payloads[0], payloads[1])
	}
	assertFlightComplete(t, &api.explorerMemo)
}

func TestAggregateFlightDifferentKeyDoesNotWait(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	probe := newFlightProbe()
	api.testHooks = probe.hooks()

	var wg sync.WaitGroup
	leaderErr := make(chan error, 1)
	wg.Go(func() {
		_, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli())
		leaderErr <- err
	})
	awaitFlightSignal(t, probe.started, "leader chart fold")

	// The one flight slot belongs to the 60-minute key. A different window
	// must compute now, not wait behind it.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	p, err := api.chart(ctx, 30, nil, "", now.UnixMilli())
	if err != nil {
		t.Fatalf("different-key chart waited behind the flight: %v", err)
	}
	if got := probe.cold.Load(); got != 2 {
		t.Fatalf("cold folds = %d, want 2", got)
	}
	from, step, count := chartWindowEdges(now.UnixMilli(), now.Add(-30*time.Minute).UnixMilli())
	if p.FromMs != from || p.BucketMs != step || len(p.Buckets) != count {
		t.Fatalf("different-key geometry = %d/%d/%d, want %d/%d/%d",
			p.FromMs, p.BucketMs, len(p.Buckets), from, step, count)
	}

	close(probe.release)
	wg.Wait()
	if err := <-leaderErr; err != nil {
		t.Fatalf("leader: %v", err)
	}
}

func TestAggregateFlightCanceledFollowerKeepsLeader(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	probe := newFlightProbe()
	api.testHooks = probe.hooks()

	var wg sync.WaitGroup
	leaderErr := make(chan error, 1)
	wg.Go(func() {
		_, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli())
		leaderErr <- err
	})
	awaitFlightSignal(t, probe.started, "leader chart fold")

	followerCtx, cancelFollower := context.WithCancel(t.Context())
	followerDone := make(chan error, 1)
	wg.Go(func() {
		_, err := api.chart(followerCtx, 60, nil, "", now.UnixMilli())
		followerDone <- err
	})
	awaitFlightJoin(t, probe, func() { close(probe.release); wg.Wait() })
	cancelFollower()

	select {
	case err := <-followerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled follower = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		close(probe.release)
		wg.Wait()
		t.Fatal("canceled follower did not return")
	}

	// The follower's cancellation must leave the leader and its slot intact.
	api.chartMemo.mu.Lock()
	flight, entry := api.chartMemo.flight, api.chartMemo.entry
	api.chartMemo.mu.Unlock()
	if flight == nil || entry != nil {
		t.Fatalf("follower cancellation disturbed the leader: in-flight=%v published=%v", flight != nil, entry != nil)
	}
	if got := probe.cold.Load(); got != 1 {
		t.Fatalf("cold folds = %d, want 1", got)
	}

	close(probe.release)
	wg.Wait()
	if err := <-leaderErr; err != nil {
		t.Fatalf("leader: %v", err)
	}
	assertFlightComplete(t, &api.chartMemo)
}

func TestAggregateFlightLeaderCanceledThenFollowerLeads(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	probe := newFlightProbe()
	api.testHooks = probe.hooks()

	var wg sync.WaitGroup
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leaderErr := make(chan error, 1)
	wg.Go(func() {
		_, err := api.chart(leaderCtx, 60, nil, "", now.UnixMilli())
		leaderErr <- err
	})
	awaitFlightSignal(t, probe.started, "leader chart fold")

	followerPayload := make(chan chartPayload, 1)
	followerErr := make(chan error, 1)
	wg.Go(func() {
		p, err := api.chart(t.Context(), 60, nil, "", now.UnixMilli())
		followerPayload <- p
		followerErr <- err
	})
	awaitFlightJoin(t, probe, func() { close(probe.release); wg.Wait() })

	cancelLeader()
	close(probe.release)
	wg.Wait()

	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader = %v, want context.Canceled", err)
	}
	if err := <-followerErr; err != nil {
		t.Fatalf("follower inherited the leader failure: %v", err)
	}
	p := <-followerPayload
	if got := cacheChartRequests(p); got != 1 {
		t.Fatalf("follower requests = %d, want 1", got)
	}
	if got := probe.cold.Load(); got != 2 {
		t.Fatalf("cold folds = %d, want 2 (canceled leader then follower)", got)
	}
	assertFlightComplete(t, &api.chartMemo)
}

func TestAggregateFlightChartGeometryRecomputes(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)
	probe := newFlightProbe()
	api.testHooks = probe.hooks()

	var wg sync.WaitGroup
	payloads := make([]chartPayload, 2)
	errs := make([]error, 2)
	wg.Go(func() { payloads[0], errs[0] = api.chart(t.Context(), 60, nil, "", now.UnixMilli()) })
	awaitFlightSignal(t, probe.started, "leader chart fold")

	// The same key carries no clock, so the follower joins the flight and then
	// finds a published payload with the leader's geometry.
	followerNow := now.Add(2 * time.Minute)
	wg.Go(func() { payloads[1], errs[1] = api.chart(t.Context(), 60, nil, "", followerNow.UnixMilli()) })
	awaitFlightJoin(t, probe, func() { close(probe.release); wg.Wait() })
	close(probe.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("chart %d: %v", i, err)
		}
	}
	if got := probe.cold.Load(); got != 2 {
		t.Fatalf("cold folds = %d, want 2 (mismatched geometry recomputes)", got)
	}
	leaderFrom, leaderStep, leaderCount := chartWindowEdges(now.UnixMilli(), now.Add(-60*time.Minute).UnixMilli())
	followerFrom, followerStep, followerCount := chartWindowEdges(followerNow.UnixMilli(), followerNow.Add(-60*time.Minute).UnixMilli())
	if followerFrom == leaderFrom {
		t.Fatal("test clock did not change the bucket geometry")
	}
	for i, want := range []struct {
		from  int64
		step  int64
		count int
	}{{leaderFrom, leaderStep, leaderCount}, {followerFrom, followerStep, followerCount}} {
		p := payloads[i]
		if p.FromMs != want.from || p.BucketMs != want.step || len(p.Buckets) != want.count {
			t.Fatalf("chart %d geometry = %d/%d/%d, want %d/%d/%d",
				i, p.FromMs, p.BucketMs, len(p.Buckets), want.from, want.step, want.count)
		}
		if cacheChartRequests(p) != 1 {
			t.Fatalf("chart %d requests = %d, want 1", i, cacheChartRequests(p))
		}
	}
	if api.chartMemo.entry.value.payload.FromMs != followerFrom {
		t.Fatalf("published chart kept stale geometry: %d, want %d", api.chartMemo.entry.value.payload.FromMs, followerFrom)
	}
}

func TestAggregateFlightConcurrentCold(t *testing.T) {
	const callers = 8
	now := time.Date(2030, 1, 2, 12, 0, 30, 0, time.UTC)
	buf := metrics.NewBuffer(8)
	buf.Record(cacheRecord("a", now.Add(-10*time.Minute)))
	api := NewAggAPI(buf, nil, time.Second)

	var cold, waits atomic.Int64
	allJoined := make(chan struct{})
	api.testHooks = &aggregateTestHooks{
		cold: func() {
			if cold.Add(1) == 1 {
				select {
				case <-allJoined:
				case <-time.After(5 * time.Second):
				}
			}
		},
		wait: func() {
			if waits.Add(1) == callers-1 {
				close(allJoined)
			}
		},
	}

	start := make(chan struct{})
	payloads := make([]chartPayload, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			payloads[i], errs[i] = api.chart(t.Context(), 60, nil, "", now.UnixMilli())
		})
	}
	close(start)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("chart %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(payloads[i], payloads[0]) {
			t.Fatalf("concurrent chart %d differs:\n%+v\n%+v", i, payloads[i], payloads[0])
		}
	}
	if got := cold.Load(); got != 1 {
		t.Fatalf("cold folds = %d, want 1", got)
	}
	if got := waits.Load(); got != callers-1 {
		t.Fatalf("waits = %d, want %d", got, callers-1)
	}
	t.Logf("callers=%d folds=%d waits=%d", callers, cold.Load(), waits.Load())
	assertFlightComplete(t, &api.chartMemo)
}
