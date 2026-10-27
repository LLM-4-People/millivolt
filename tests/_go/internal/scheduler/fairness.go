package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// groupQueued reads one group's live admission queue depth. The key must
// already exist; these tests create it before calling.
func groupQueued(s *Scheduler, key string) int {
	g := s.groupFor(key, 0)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queued
}

// TestFairProviderDrainGrantsOnePerKey pins cross-key fairness on a provider
// release: two queued keys and two freed provider slots must grant one waiter
// to each key before either key receives a second. The old drain emptied the
// first attributed key's queue before visiting the next key, so both freed
// slots went to the same key.
func TestFairProviderDrainGrantsOnePerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "fair.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 2}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		h1, err := s.AcquireWith(ctx, provider+"|holder-1", 0, hooks)
		if err != nil {
			t.Fatal(err)
		}
		h2, err := s.AcquireWith(ctx, provider+"|holder-2", 0, hooks)
		if err != nil {
			h1(0)
			t.Fatal(err)
		}

		type grant struct {
			key string
			rel Release
		}
		grants := make(chan grant, 4)
		var wg sync.WaitGroup
		for _, key := range []string{provider + "|a", provider + "|b"} {
			for i := 0; i < 2; i++ {
				key := key
				wg.Go(func() {
					rel, err := s.AcquireWith(ctx, key, 0, hooks)
					if err != nil {
						t.Errorf("acquire %s: %v", key, err)
						return
					}
					grants <- grant{key: key, rel: rel}
				})
			}
		}
		synctest.Wait()
		if got := s.Stats().Queued; got != 4 {
			t.Fatalf("queued = %d, want 4 behind the full provider cap", got)
		}

		h1(0)
		synctest.Wait()
		if len(grants) != 1 {
			t.Fatalf("first release granted %d waiters, want 1", len(grants))
		}
		first := <-grants

		h2(0)
		synctest.Wait()
		if len(grants) != 1 {
			t.Fatalf("second release granted %d waiters, want 1", len(grants))
		}
		second := <-grants
		if first.key == second.key {
			t.Fatalf("one key took both freed provider slots: %s then %s", first.key, second.key)
		}
		if a, b := groupQueued(s, provider+"|a"), groupQueued(s, provider+"|b"); a != 1 || b != 1 {
			t.Fatalf("queues after two fair grants = a:%d b:%d, want 1 each", a, b)
		}

		first.rel(0)
		second.rel(0)
		synctest.Wait()
		if len(grants) != 2 {
			t.Fatalf("releases granted %d remaining waiters, want 2", len(grants))
		}
		for i := 0; i < 2; i++ {
			(<-grants).rel(0)
		}
		wg.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("fair drain leaked occupancy: %+v", st)
		}
	})
}

// TestQueuedSnapshotRotatesProviderGroups pins the rotating cursor: each fair
// drain starts at the cursor, and advancing after a served group makes
// successive drains start at successive queued keys instead of the head group
// every time. Real waiters are parked behind a global hold, so the only effect
// observed is the snapshot order.
func TestQueuedSnapshotRotatesProviderGroups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "rotate.example"
		s.RestoreHolds([]Hold{{All: true}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		keys := []string{provider + "|a", provider + "|b", provider + "|c"}
		var wg sync.WaitGroup
		for _, key := range keys {
			key := key
			wg.Go(func() {
				if _, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider}); err != context.Canceled {
					t.Errorf("held acquire %s = %v", key, err)
				}
			})
		}
		synctest.Wait()
		if got := s.Stats().Queued; got != len(keys) {
			t.Fatalf("queued = %d, want %d", got, len(keys))
		}

		s.mu.Lock()
		idx := s.providerGroups[provider]
		s.mu.Unlock()
		if idx == nil {
			t.Fatal("provider index missing")
		}
		var starts []string
		seen := make(map[string]bool, len(keys))
		for i := 0; i < 6; i++ {
			snap := idx.queuedSnapshot()
			if len(snap) != len(keys) {
				t.Fatalf("snapshot %d has %d groups, want %d", i, len(snap), len(keys))
			}
			starts = append(starts, snap[0].key)
			if i < len(keys) {
				seen[snap[0].key] = true
			}
			// Simulate a pass that served only the snapshot's first group.
			idx.advanceCursor(snap, 0)
		}
		for _, key := range keys {
			if !seen[key] {
				t.Fatalf("rotation never started at %s: %v", key, starts)
			}
		}
		for i := 1; i < len(starts); i++ {
			if starts[i] == starts[i-1] {
				t.Fatalf("rotation repeated starting group %s: %v", starts[i], starts)
			}
		}

		cancel()
		synctest.Wait()
		wg.Wait()
		s.RestoreHolds(nil)
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("cancelled held waiters leaked occupancy: %+v", st)
		}
	})
}

// TestProviderFastPathQueuesBehindAnotherKey pins the provider-wide queue
// witness on the fast path. Key A's waiter is queued behind A's own per-key
// cap while the provider has a free slot; a brand-new key B must enqueue and
// be admitted by the fair drain, not jump the provider queue. OnWait is the
// observable: a fast-path grant never publishes it.
func TestProviderFastPathQueuesBehindAnotherKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "fastpath.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 2}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		holderA, err := s.AcquireWith(ctx, provider+"|a", 1, hooks)
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancelWait := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() {
			release, err := s.AcquireWith(waitCtx, provider+"|a", 1, hooks)
			if err == context.Canceled {
				return
			}
			if err != nil {
				t.Errorf("capped waiter: %v", err)
				return
			}
			release(0)
		})
		synctest.Wait()
		if got := s.Stats().Queued; got != 1 {
			t.Fatalf("queued = %d, want A's waiter parked behind its key cap", got)
		}

		var waited atomic.Int32
		releaseB, err := s.AcquireWith(ctx, provider+"|b", 0, WaiterHooks{
			Provider: provider,
			OnWait:   func() { waited.Add(1) },
		})
		if err != nil {
			cancelWait()
			holderA(0)
			t.Fatal(err)
		}
		if waited.Load() != 1 {
			releaseB(0)
			cancelWait()
			holderA(0)
			t.Fatalf("new key granted on the fast path while another key was queued")
		}
		if got := s.Stats().Queued; got != 1 {
			releaseB(0)
			cancelWait()
			holderA(0)
			t.Fatalf("queued = %d, want only A's waiter still parked", got)
		}

		releaseB(0)
		cancelWait()
		synctest.Wait()
		holderA(0)
		wg.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("fast-path regression test leaked occupancy: %+v", st)
		}
	})
}

// TestFairDrainNoStarvation pins bounded service across keys: with N keys each
// holding two queued waiters behind N provider slots, releasing N holders must
// grant exactly one waiter to every key. The old per-key-to-exhaustion drain
// spent every freed slot on the first keys and starved the rest.
func TestFairDrainNoStarvation(t *testing.T) {
	const keys = 8
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "starvation.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: keys}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		holders := make([]Release, keys)
		for i := range holders {
			release, err := s.AcquireWith(ctx, provider+"|key-"+string(rune('a'+i)), 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			holders[i] = release
		}
		var wg sync.WaitGroup
		grants := make(chan struct {
			key string
			rel Release
		}, 2*keys)
		for i := 0; i < keys; i++ {
			for j := 0; j < 2; j++ {
				key := provider + "|key-" + string(rune('a'+i))
				wg.Go(func() {
					release, err := s.AcquireWith(ctx, key, 0, hooks)
					if err != nil {
						t.Errorf("acquire %s: %v", key, err)
						return
					}
					grants <- struct {
						key string
						rel Release
					}{key: key, rel: release}
				})
			}
		}
		synctest.Wait()
		if got := s.Stats().Queued; got != 2*keys {
			t.Fatalf("queued = %d, want %d", got, 2*keys)
		}

		for i := range holders {
			holders[i](0)
			synctest.Wait()
			if got := len(grants); got != i+1 {
				t.Fatalf("after %d releases %d waiters were granted, want %d", i+1, got, i+1)
			}
		}
		served := make(map[string]bool, keys)
		firstWave := make([]Release, 0, keys)
		for i := 0; i < keys; i++ {
			g := <-grants
			served[g.key] = true
			firstWave = append(firstWave, g.rel)
		}
		if len(served) != keys {
			t.Fatalf("%d releases served only %d distinct keys: %v", keys, len(served), served)
		}

		for _, release := range firstWave {
			release(0)
		}
		synctest.Wait()
		if got := len(grants); got != keys {
			t.Fatalf("second wave granted %d waiters, want %d", got, keys)
		}
		for i := 0; i < keys; i++ {
			(<-grants).rel(0)
		}
		wg.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("no-starvation drain leaked occupancy: %+v", st)
		}
		if got := providerQueuedGroups(s, provider); got != 0 {
			t.Fatalf("provider queued groups = %d, want 0", got)
		}
	})
}

// TestFairDrainCancellationDoesNotLeak pins tombstone handling inside a fair
// pass: the cancelled head of key A is skipped without consuming the pass's
// one-grant budget, both live keys receive exactly one freed slot, and no
// phantom slot or queue count survives.
func TestFairDrainCancellationDoesNotLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "cancel.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 2}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		h1, err := s.AcquireWith(ctx, provider+"|h1", 0, hooks)
		if err != nil {
			t.Fatal(err)
		}
		h2, err := s.AcquireWith(ctx, provider+"|h2", 0, hooks)
		if err != nil {
			h1(0)
			t.Fatal(err)
		}

		cancelCtx, cancelHead := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := s.AcquireWith(cancelCtx, provider+"|a", 0, hooks); err != context.Canceled {
				t.Errorf("cancelled head = %v", err)
			}
		})
		synctest.Wait()
		if got := s.Stats().Queued; got != 1 {
			t.Fatalf("queued = %d, want the cancellable head", got)
		}
		type grant struct {
			key string
			rel Release
		}
		grants := make(chan grant, 2)
		wg.Go(func() {
			release, err := s.AcquireWith(ctx, provider+"|a", 0, hooks)
			if err != nil {
				t.Errorf("A follower: %v", err)
				return
			}
			grants <- grant{key: provider + "|a", rel: release}
		})
		wg.Go(func() {
			release, err := s.AcquireWith(ctx, provider+"|b", 0, hooks)
			if err != nil {
				t.Errorf("B waiter: %v", err)
				return
			}
			grants <- grant{key: provider + "|b", rel: release}
		})
		synctest.Wait()
		if got := s.Stats().Queued; got != 3 {
			t.Fatalf("queued = %d, want 3 live/cancelled waiters", got)
		}

		cancelHead()
		synctest.Wait()
		if got := s.Stats().Queued; got != 2 {
			t.Fatalf("queued = %d after cancellation, want 2", got)
		}

		h1(0)
		h2(0)
		synctest.Wait()
		if len(grants) != 2 {
			t.Fatalf("grants = %d, want both keys served once", len(grants))
		}
		served := make(map[string]bool, 2)
		for i := 0; i < 2; i++ {
			g := <-grants
			served[g.key] = true
			g.rel(0)
		}
		if len(served) != 2 {
			t.Fatalf("keys served = %v, want key A and key B", served)
		}
		wg.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("cancelled fair drain leaked occupancy: %+v", st)
		}
		if got := providerQueuedGroups(s, provider); got != 0 {
			t.Fatalf("provider queued groups = %d, want 0", got)
		}
		for _, key := range []string{provider + "|a", provider + "|b"} {
			if got := groupQueued(s, key); got != 0 {
				t.Fatalf("group %s queued = %d, want 0", key, got)
			}
		}
	})
}
