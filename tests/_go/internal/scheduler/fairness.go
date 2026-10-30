package scheduler

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// groupQueued reads one group's live admission queue depth. The key must
// already exist; these tests create it before calling.
func groupQueued(s *Scheduler, key string) int {
	g := s.groupFor(key, 0)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queue.live
}

// providerGateWaiters reads a provider's fair-waiting count under the scheduler
// lock (the index map itself is only mutated under s.mu).
func providerGateWaiters(s *Scheduler, provider string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx := s.providerGroups[provider]; idx != nil {
		return idx.fairCount.Load()
	}
	return 0
}

// groupGateWaiting reports one group's membership in its provider's
// fair-waiting set.
func groupGateWaiting(s *Scheduler, key string) bool {
	g := s.groupFor(key, 0)
	return g.gateWaiting.Load()
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

// TestFairSnapshotRotatesProviderGroups pins the rotating cursor over the
// fair-waiting set: each drain starts after the group served last, so
// successive drains begin at successive gate-blocked keys instead of the head
// group every time. The single provider slot is held, so every waiter is
// refused by the shared gate and stays marked for the whole observation.
func TestFairSnapshotRotatesProviderGroups(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "rotate.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		holder, err := s.AcquireWith(ctx, provider+"|holder", 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatal(err)
		}
		keys := []string{provider + "|a", provider + "|b", provider + "|c"}
		var wg sync.WaitGroup
		for _, key := range keys {
			key := key
			wg.Go(func() {
				if _, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider}); err != context.Canceled {
					t.Errorf("blocked acquire %s = %v", key, err)
				}
			})
			// Queue one key at a time so the fair slice order is the launch
			// order, making the rotation assertion deterministic.
			synctest.Wait()
		}
		if got := s.Stats().Queued; got != len(keys) {
			t.Fatalf("queued = %d, want %d", got, len(keys))
		}
		if got := providerGateWaiters(s, provider); got != int64(len(keys)) {
			t.Fatalf("fair-waiting groups = %d, want %d", got, len(keys))
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
			snap := idx.fairSnapshot()
			if len(snap) != len(keys) {
				t.Fatalf("snapshot %d has %d groups, want %d", i, len(snap), len(keys))
			}
			starts = append(starts, snap[0].key)
			if i < len(keys) {
				seen[snap[0].key] = true
			}
			// Simulate a pass that served only the snapshot's first group.
			idx.advanceFair(snap[0])
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
		holder(0)
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("cancelled fair waiters leaked occupancy: %+v", st)
		}
		if got := providerGateWaiters(s, provider); got != 0 {
			t.Fatalf("fair-waiting groups = %d after cancellation, want 0", got)
		}
	})
}

// TestProviderFastPathQueuesBehindAnotherKey pins the shared-gate half of the
// fast-path witness: a peer refused by the provider's token bucket suppresses a
// new key's fast path even though that key's own (zero-token) admission would
// fit the refilled bucket. Key A parks one waiter on the 100-token window; the
// provider still has free concurrency slots, and a minute of refill gives key B
// enough fill to pass a zero-estimate admission. B must still enqueue and take
// its fair turn, never jump A's provider-gate wait. A peer parked only on a
// per-key predicate is covered by TestProviderFastPathIgnoresPerKeyBlockedPeer,
// where the fast path must stay open.
func TestProviderFastPathQueuesBehindAnotherKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "fastpath.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
			Concurrency: 10, Tokens: 100, TokWindow: time.Hour,
		}})
		ctx := context.Background()

		holderA, err := s.AcquireWith(ctx, provider+"|a", 0, WaiterHooks{Provider: provider, EstTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancelWait := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() {
			release, err := s.AcquireWith(waitCtx, provider+"|a", 0, WaiterHooks{Provider: provider, EstTokens: 100})
			if err == context.Canceled {
				return
			}
			if err != nil {
				t.Errorf("token waiter: %v", err)
				return
			}
			release(100)
		})
		synctest.Wait()
		if got := s.Stats().Queued; got != 1 {
			t.Fatalf("queued = %d, want A's waiter parked on the token window", got)
		}
		if got := providerGateWaiters(s, provider); got != 1 {
			t.Fatalf("fair-waiting groups = %d, want the token-refused peer", got)
		}
		// Enough refill for a zero-estimate request, nowhere near the 100-token
		// estimate A's waiter reserved; A stays refused by the shared gate.
		time.Sleep(time.Minute)
		synctest.Wait()

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
			t.Fatalf("new key granted on the fast path while another key waited on the shared gate")
		}
		if st := s.Stats(); st.InFlight != 2 || st.Queued != 1 {
			releaseB(0)
			cancelWait()
			holderA(0)
			t.Fatalf("occupancy = %+v, want B in flight and only A's waiter queued", st)
		}
		if got := groupQueued(s, provider+"|b"); got != 0 {
			releaseB(0)
			cancelWait()
			holderA(0)
			t.Fatalf("key B queued = %d, want the fair drain to grant it", got)
		}

		releaseB(0)
		cancelWait()
		synctest.Wait()
		holderA(100)
		wg.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("fast-path regression test leaked occupancy: %+v", st)
		}
		if got := providerGateWaiters(s, provider); got != 0 {
			t.Fatalf("fair-waiting groups = %d after cleanup, want 0", got)
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

// TestProviderFastPathIgnoresPerKeyBlockedPeer is the regression for the
// over-broad witness: a peer parked on its own pacing window, an operator hold
// or a per-key cap waits on a predicate the shared provider gate cannot
// relieve, so a new key with a free provider slot must be admitted on the fast
// path. OnWait is the client-visible observable: committing it makes the proxy
// send SSE keepalives to a request that never had to queue.
func TestProviderFastPathIgnoresPerKeyBlockedPeer(t *testing.T) {
	const provider = "precise.example"
	cases := []struct {
		name    string
		capA    int
		setup   func(t *testing.T, s *Scheduler, key string)
		cleanup func(s *Scheduler)
	}{
		{
			name: "pacing",
			setup: func(t *testing.T, s *Scheduler, key string) {
				t.Helper()
				if !s.Trip(key, time.Hour) {
					t.Fatal("trip did not claim the send token")
				}
			},
			cleanup: func(*Scheduler) {},
		},
		{
			name: "hold",
			setup: func(t *testing.T, s *Scheduler, _ string) {
				t.Helper()
				if err := s.AddHold(Hold{ID: "peer", Clients: []string{"client-a"}}); err != nil {
					t.Fatal(err)
				}
			},
			cleanup: func(s *Scheduler) { s.RemoveHold("peer") },
		},
		{
			name:    "key-cap",
			capA:    1,
			setup:   func(*testing.T, *Scheduler, string) {},
			cleanup: func(*Scheduler) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := New(Options{})
				s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 10}})
				ctx := context.Background()
				keyA, keyB := provider+"|a", provider+"|b"
				holderA, err := s.AcquireWith(ctx, keyA, 0, WaiterHooks{Provider: provider})
				if err != nil {
					t.Fatal(err)
				}
				tc.setup(t, s, keyA)

				waitCtx, cancelWait := context.WithCancel(ctx)
				var wg sync.WaitGroup
				wg.Go(func() {
					release, err := s.AcquireWith(waitCtx, keyA, tc.capA, WaiterHooks{Provider: provider, Client: "client-a"})
					if err == context.Canceled {
						return
					}
					if err != nil {
						t.Errorf("%s peer acquire: %v", tc.name, err)
						return
					}
					release(0)
				})
				synctest.Wait()
				if got := s.Stats().Queued; got != 1 {
					t.Fatalf("queued = %d, want the %s peer parked", got, tc.name)
				}
				if got := providerGateWaiters(s, provider); got != 0 {
					t.Fatalf("%s peer entered the fair-waiting set: %d", tc.name, got)
				}

				var waited atomic.Int32
				releaseB, err := s.AcquireWith(ctx, keyB, 0, WaiterHooks{
					Provider: provider,
					Client:   "client-b",
					OnWait:   func() { waited.Add(1) },
				})
				if err != nil {
					cancelWait()
					holderA(0)
					t.Fatal(err)
				}
				if waited.Load() != 0 {
					releaseB(0)
					cancelWait()
					holderA(0)
					t.Fatalf("new key waited behind a peer parked on %s", tc.name)
				}
				if st := s.Stats(); st.InFlight != 2 || st.Queued != 1 {
					releaseB(0)
					cancelWait()
					holderA(0)
					t.Fatalf("occupancy = %+v, want the holder and B in flight with only the peer queued", st)
				}
				if got := groupQueued(s, keyB); got != 0 {
					releaseB(0)
					cancelWait()
					holderA(0)
					t.Fatalf("new key queued = %d, want a fast-path grant", got)
				}

				releaseB(0)
				cancelWait()
				synctest.Wait()
				holderA(0)
				wg.Wait()
				tc.cleanup(s)
				if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
					t.Fatalf("%s regression test leaked occupancy: %+v", tc.name, st)
				}
				if got := providerGateWaiters(s, provider); got != 0 {
					t.Fatalf("%s left %d fair-waiting groups, want 0", tc.name, got)
				}
			})
		})
	}
}

// TestFairDrainClearsPerKeyBlockedMark pins the mixed-queue rule: a group that
// was gate-waiting but becomes per-key blocked withdraws its mark during the
// drain, is skipped by the remaining fair passes, and does not consume a grant
// turn that another key can use for the freed provider capacity.
func TestFairDrainClearsPerKeyBlockedMark(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "mixed.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 3}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		holders := make([]Release, 0, 3)
		for i := 0; i < 3; i++ {
			release, err := s.AcquireWith(ctx, provider+"|holder-"+strconv.Itoa(i), 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			holders = append(holders, release)
		}

		waitCtxA, cancelA := context.WithCancel(ctx)
		grantsA := make(chan Release, 2)
		grantsB := make(chan Release, 2)
		var wg sync.WaitGroup
		// A failing assertion must release every waiter before synctest tears
		// the bubble down; otherwise the first failure is buried under a
		// deadlock panic. Release is idempotent, so the normal-path cleanup
		// below can repeat it.
		defer func() {
			cancelA()
			for _, release := range holders {
				release(0)
			}
			wg.Wait()
		}()
		launch := func(key string, acqCtx context.Context, out chan<- Release) {
			wg.Go(func() {
				release, err := s.AcquireWith(acqCtx, key, 0, hooks)
				if err != nil {
					if acqCtx.Err() == nil {
						t.Errorf("acquire %s: %v", key, err)
					}
					return
				}
				out <- release
			})
		}
		launch(provider+"|a", waitCtxA, grantsA)
		launch(provider+"|a", waitCtxA, grantsA)
		launch(provider+"|b", ctx, grantsB)
		launch(provider+"|b", ctx, grantsB)
		synctest.Wait()
		if got := s.Stats().Queued; got != 4 {
			t.Fatalf("queued = %d, want 4 behind the full provider cap", got)
		}
		if got := providerGateWaiters(s, provider); got != 2 {
			t.Fatalf("fair-waiting groups = %d, want keys A and B", got)
		}

		// A was gate-refused; trip its pacing window so it becomes per-key
		// blocked while still marked.
		if !s.Trip(provider+"|a", time.Hour) {
			t.Fatal("trip did not claim the send token")
		}
		if groupGateWaiting(s, provider+"|a") {
			t.Fatal("trip left the paced key in the fair-waiting set")
		}
		// Two more provider slots open at once: only B can use them.
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 5}})
		synctest.Wait()
		if got := len(grantsB); got != 2 {
			t.Fatalf("key B grants = %d, want both freed slots", got)
		}
		if got := groupQueued(s, provider+"|b"); got != 0 {
			t.Fatalf("key B queued = %d after the fair drain, want 0", got)
		}
		if got := groupQueued(s, provider+"|a"); got != 2 {
			t.Fatalf("key A queued = %d, want both waiters parked on pacing", got)
		}
		if groupGateWaiting(s, provider+"|a") {
			t.Fatal("paced key kept its fair-waiting mark after the drain")
		}
		if got := providerGateWaiters(s, provider); got != 0 {
			t.Fatalf("fair-waiting groups = %d after the drain, want 0", got)
		}

		cancelA()
		synctest.Wait()
		wg.Wait()
		for i := 0; i < 2; i++ {
			(<-grantsB)(0)
		}
		for _, release := range holders {
			release(0)
		}
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("mixed-queue drain leaked occupancy: %+v", st)
		}
	})
}

// TestGateWaitingMarkClearsOnCancellationAndGrant pins both normal exits from
// the fair set: canceling a gate-refused waiter and granting it through a
// release must each drop the provider count to zero, so a later key always sees
// an open fast path.
func TestGateWaitingMarkClearsOnCancellationAndGrant(t *testing.T) {
	const provider = "clearmark.example"

	t.Run("cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
			ctx := context.Background()
			holder, err := s.AcquireWith(ctx, provider+"|holder", 0, WaiterHooks{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			waitCtx, cancelWait := context.WithCancel(ctx)
			var wg sync.WaitGroup
			wg.Go(func() {
				if _, err := s.AcquireWith(waitCtx, provider+"|a", 0, WaiterHooks{Provider: provider}); err != context.Canceled {
					t.Errorf("cancelled gate waiter = %v", err)
				}
			})
			synctest.Wait()
			if got := providerGateWaiters(s, provider); got != 1 {
				t.Fatalf("fair-waiting groups = %d, want the gate-refused waiter", got)
			}
			cancelWait()
			synctest.Wait()
			wg.Wait()
			if got := providerGateWaiters(s, provider); got != 0 {
				t.Fatalf("fair-waiting groups = %d after cancellation, want 0", got)
			}
			if groupGateWaiting(s, provider+"|a") {
				t.Fatal("cancelled group kept its fair-waiting mark")
			}
			holder(0)
			if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
				t.Fatalf("cancelled gate waiter leaked occupancy: %+v", st)
			}
		})
	})

	t.Run("grant", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
			ctx := context.Background()
			holder, err := s.AcquireWith(ctx, provider+"|holder", 0, WaiterHooks{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			granted := make(chan Release, 1)
			var wg sync.WaitGroup
			wg.Go(func() {
				release, err := s.AcquireWith(ctx, provider+"|a", 0, WaiterHooks{Provider: provider})
				if err != nil {
					t.Errorf("gate waiter: %v", err)
					return
				}
				granted <- release
			})
			synctest.Wait()
			if got := providerGateWaiters(s, provider); got != 1 {
				t.Fatalf("fair-waiting groups = %d, want the gate-refused waiter", got)
			}
			holder(0)
			synctest.Wait()
			release := <-granted
			if got := providerGateWaiters(s, provider); got != 0 {
				t.Fatalf("fair-waiting groups = %d after the grant, want 0", got)
			}
			if groupGateWaiting(s, provider+"|a") {
				t.Fatal("granted group kept its fair-waiting mark")
			}
			release(0)
			wg.Wait()
			if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
				t.Fatalf("granted gate waiter leaked occupancy: %+v", st)
			}
		})
	})
}

// TestReleaseSkipsPerKeyBlockedPeer pins the release cost the over-broad
// witness caused: a peer parked on its own pacing window must not be locked by
// an unrelated key's release. The peer's group mutex is held while key B
// releases its slot; a drain that walks every queued group blocks there, while
// the fair-waiting set returns immediately because the peer is not in it.
func TestReleaseSkipsPerKeyBlockedPeer(t *testing.T) {
	s := New(Options{})
	provider := "skip.example"
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 3}})
	ctx := context.Background()

	holderA, err := s.AcquireWith(ctx, provider+"|a", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Trip(provider+"|a", time.Hour) {
		t.Fatal("trip did not claim the send token")
	}
	waitCtx, cancelWait := context.WithCancel(ctx)
	peerDone := make(chan struct{})
	go func() {
		release, err := s.AcquireWith(waitCtx, provider+"|a", 0, WaiterHooks{Provider: provider})
		if err == nil {
			release(0)
		}
		close(peerDone)
	}()
	waitFor(t, func() bool {
		return s.Stats().Queued == 1 && providerGateWaiters(s, provider) == 0
	}, "the paced peer to be queued outside the fair set")

	holderB, err := s.AcquireWith(ctx, provider+"|b", 0, WaiterHooks{Provider: provider})
	if err != nil {
		cancelWait()
		t.Fatal(err)
	}

	peer := s.groupFor(provider+"|a", 0)
	peer.mu.Lock()
	released := make(chan struct{})
	go func() {
		holderB(0)
		close(released)
	}()
	select {
	case <-released:
		peer.mu.Unlock()
	case <-time.After(time.Second):
		peer.mu.Unlock()
		select {
		case <-released:
		case <-time.After(2 * time.Second):
			t.Fatal("release did not finish after the paced peer's lock was released")
		}
		t.Fatal("release walked a peer parked only on its per-key pacing window")
	}
	cancelWait()
	<-peerDone
	holderA(0)
	if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
		t.Fatalf("release-skip test leaked occupancy: %+v", st)
	}
	if got := providerGateWaiters(s, provider); got != 0 {
		t.Fatalf("fair-waiting groups = %d after cleanup, want 0", got)
	}
}

// TestFairDrainStopsWhenProviderConcurrencyFull pins the saturated-cap stop: a
// release that frees the provider's only slot grants the next fair waiter and
// must stop the drain instead of walking every other fair-waiting key. Key B's
// group mutex stays held while the release runs, so a drain that visits B in
// the same or a later pass blocks until the assertion times out. The old drain
// finished the pass and ran a second one, taking every fair group's mutex twice
// per release.
func TestFairDrainStopsWhenProviderConcurrencyFull(t *testing.T) {
	s := New(Options{})
	provider := "stop.example"
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holder, err := s.AcquireWith(ctx, provider+"|holder", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	type grant struct {
		key     string
		release Release
	}
	grants := make(chan grant, 2)
	var wg sync.WaitGroup
	launch := func(key string) {
		wg.Go(func() {
			release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
			if err != nil {
				if ctx.Err() == nil {
					t.Errorf("acquire %s: %v", key, err)
				}
				return
			}
			grants <- grant{key: key, release: release}
		})
	}
	launch(provider + "|a")
	waitFor(t, func() bool { return groupGateWaiting(s, provider+"|a") }, "key A in the fair-waiting set")
	launch(provider + "|b")
	waitFor(t, func() bool {
		return groupGateWaiting(s, provider+"|b") && s.Stats().Queued == 2
	}, "key B in the fair-waiting set")

	bGroup := s.groupFor(provider+"|b", 0)
	bGroup.mu.Lock()
	released := make(chan struct{})
	go func() {
		holder(0)
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(time.Second):
		bGroup.mu.Unlock()
		cancel()
		t.Fatal("release walked a fair-waiting peer after refilling the provider concurrency cap")
	}
	var first grant
	select {
	case first = <-grants:
	case <-time.After(time.Second):
		bGroup.mu.Unlock()
		cancel()
		t.Fatal("release did not grant key A")
	}
	if first.key != provider+"|a" {
		bGroup.mu.Unlock()
		cancel()
		t.Fatalf("first grant = %s, want key A", first.key)
	}
	bGroup.mu.Unlock()

	// Key A's release frees the slot again, so the next drain serves B.
	first.release(0)
	var second grant
	select {
	case second = <-grants:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("second release did not grant key B")
	}
	second.release(0)
	wg.Wait()
	if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
		t.Fatalf("saturated-cap drain leaked occupancy: %+v", st)
	}
	if got := providerGateWaiters(s, provider); got != 0 {
		t.Fatalf("fair-waiting groups = %d after cleanup, want 0", got)
	}
}

// TestFairDrainKeepsMultiSlotCapacity pins that the saturated-cap stop does not
// truncate a drain that legitimately has several free provider slots: raising
// the cap from two to four must grant both waiting keys in the same drain, so
// the stop fires only once the last free slot is refilled.
func TestFairDrainKeepsMultiSlotCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "multislot.example"
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

		type grant struct {
			key     string
			release Release
		}
		grants := make(chan grant, 2)
		var wg sync.WaitGroup
		for _, key := range []string{provider + "|a", provider + "|b"} {
			key := key
			wg.Go(func() {
				release, err := s.AcquireWith(ctx, key, 0, hooks)
				if err != nil {
					t.Errorf("acquire %s: %v", key, err)
					return
				}
				grants <- grant{key: key, release: release}
			})
		}
		synctest.Wait()
		if st := s.Stats(); st.Queued != 2 {
			t.Fatalf("queued = %d, want both waiters behind the full cap", st.Queued)
		}
		if got := providerGateWaiters(s, provider); got != 2 {
			t.Fatalf("fair-waiting groups = %d, want keys A and B", got)
		}

		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 4}})
		synctest.Wait()
		if got := len(grants); got != 2 {
			t.Fatalf("cap raise granted %d waiters, want both keys in one drain", got)
		}
		first, second := <-grants, <-grants
		if first.key == second.key {
			t.Fatalf("one key took both freed provider slots: %s twice", first.key)
		}
		first.release(0)
		second.release(0)
		h1(0)
		h2(0)
		synctest.Wait()
		wg.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("multi-slot drain leaked occupancy: %+v", st)
		}
		if got := providerGateWaiters(s, provider); got != 0 {
			t.Fatalf("fair-waiting groups = %d after cleanup, want 0", got)
		}
	})
}

// BenchmarkThrottledReleaseFairBacklog measures the release drain while G keys
// wait on the same provider's concurrency cap. Each backlog key is published in
// the exact state a shared-gate refusal produces: its queue head is installed,
// the group is marked in the provider's fair-waiting set, and the cap is
// saturated, so a real Acquire on it would park. Each iteration queues one
// target waiter through the same queue transition Acquire uses and releases the
// target's previous admission, so the measured work is one real grant plus the
// drain that follows it. The rotation is pinned to the target because the fair
// ring's G-turn wait scales with G by design; the fix only removes the drain's
// extra walk over keys that cannot be admitted once the cap is full again.
func BenchmarkThrottledReleaseFairBacklog(b *testing.B) {
	const provider = "provider.example"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks := WaiterHooks{Provider: provider}
	for _, backlog := range []int{1, 10, 100, 1000} {
		b.Run(strconv.Itoa(backlog), func(b *testing.B) {
			s := New(Options{})
			keys := make([]string, backlog)
			for i := range keys {
				keys[i] = provider + "|backlog-" + strconv.Itoa(i)
			}
			// Attribute every group before the cap exists, so setup never
			// queues behind the concurrency limit.
			for _, key := range append([]string{provider + "|target"}, keys...) {
				release, err := s.AcquireWith(ctx, key, 0, hooks)
				if err != nil {
					b.Fatal(err)
				}
				release(0)
			}
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
			holder, err := s.AcquireWith(ctx, provider+"|target", 0, hooks)
			if err != nil {
				b.Fatal(err)
			}

			backlogGroups := make([]*group, len(keys))
			for i, key := range keys {
				g := s.groupFor(key, 0)
				g.mu.Lock()
				w := &waiter{
					ch:       make(chan struct{}, 1),
					ctx:      ctx,
					provider: provider,
				}
				g.queue.push(&w.queueNode, w)
				g.syncQueueLocked()
				g.markGateWaitingLocked()
				g.mu.Unlock()
				backlogGroups[i] = g
			}
			if got := providerGateWaiters(s, provider); got != int64(backlog) {
				b.Fatalf("fair-waiting groups = %d, want %d", got, backlog)
			}

			target := s.groupFor(provider+"|target", 0)
			s.mu.Lock()
			idx := s.providerGroups[provider]
			s.mu.Unlock()
			if idx == nil {
				b.Fatal("provider index missing")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				// Queue the target's next waiter, then pin the rotation to the
				// group before its tail slot so the release reaches it first.
				target.mu.Lock()
				pendingWaiter := &waiter{
					ch:       make(chan struct{}, 1),
					ctx:      ctx,
					provider: provider,
				}
				target.queue.push(&pendingWaiter.queueNode, pendingWaiter)
				target.syncQueueLocked()
				target.markGateWaitingLocked()
				pending := pendingWaiter
				target.mu.Unlock()

				idx.mu.Lock()
				// Pin the rotation to the fair group before the target, so the
				// release reaches the target first.
				if prev := target.fairNode.prev; prev != nil {
					idx.cursor = prev.value
				} else {
					idx.cursor = nil
				}
				idx.mu.Unlock()

				holder(0)
				select {
				case <-pending.ch:
				default:
					b.Fatal("release did not grant the queued target waiter")
				}
				holder = target.slotRelease(pending.admission)
			}
			b.StopTimer()
			for _, g := range backlogGroups {
				g.mu.Lock()
				for g.queue.head != nil {
					g.queue.remove(g.queue.head)
				}
				g.syncQueueLocked()
				g.mu.Unlock()
			}
			holder(0)
			if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
				b.Fatalf("fair backlog benchmark leaked occupancy: %+v", st)
			}
			if got := providerGateWaiters(s, provider); got != 0 {
				b.Fatalf("fair-waiting groups = %d after cleanup, want 0", got)
			}
		})
	}
}

// BenchmarkThrottledReleasePacedPeers measures an unrelated Acquire+release on
// one key while G peers are parked on per-key pacing windows. Those peers wait
// on their own window, not the shared provider gate, so the release must not
// take their group locks or build a snapshot proportional to G. The old
// provider-wide queued witness paid that walk twice per release (gate settle
// wake plus the group drain); the fair-waiting set returns after one count
// load.
func BenchmarkThrottledReleasePacedPeers(b *testing.B) {
	const provider = "provider.example"
	ctx := context.Background()
	for _, peers := range []int{0, 1, 100, 1000} {
		b.Run(strconv.Itoa(peers), func(b *testing.B) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 10}})
			for i := 0; i < peers; i++ {
				key := provider + "|peer-" + strconv.Itoa(i)
				release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
				if err != nil {
					b.Fatal(err)
				}
				release(0)
				if !s.Trip(key, time.Hour) {
					b.Fatal("trip did not claim the send token")
				}
				g := s.groupFor(key, 0)
				g.mu.Lock()
				w := &waiter{
					ch:       make(chan struct{}, 1),
					ctx:      context.Background(),
					provider: provider,
				}
				g.queue.push(&w.queueNode, w)
				g.syncQueueLocked()
				g.mu.Unlock()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				release, err := s.AcquireWith(ctx, provider+"|target", 0, WaiterHooks{Provider: provider})
				if err != nil {
					b.Fatal(err)
				}
				release(0)
			}
		})
	}
}
