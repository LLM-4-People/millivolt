package scheduler

import (
	"context"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func wakeTimerState(w *wakeTimer) (*time.Timer, uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.timer, w.generation
}

func TestThrottleUnchangedPreservesPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		initial := Throttle{Provider: "provider.example", Limit: Limit{Concurrency: 2, Requests: 1, ReqWindow: time.Hour}, Source: ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: time.Now().Add(-time.Hour)}
		if !s.SetThrottle(initial) {
			t.Fatal("initial policy was not installed")
		}
		g := s.gate(initial.Provider)
		if ok, _, _ := g.tryAdmit(0); !ok {
			t.Fatal("initial request denied")
		}
		s.armThrottleDrain(initial.Provider, time.Hour)
		defer g.wake.stop()
		req, kick := g.req, s.kickC()
		// currentCap reads the live gate's policy the way ListThrottles does
		// (the gate is re-looked-up: a clear deletes it and a restore builds
		// a new one).
		currentCap := func() Throttle {
			gg := s.gate(initial.Provider)
			gg.mu.Lock()
			cur := gg.throttle
			gg.mu.Unlock()
			return cur
		}
		timer, _ := wakeTimerState(&g.wake)
		for _, source := range []string{ThrottleSourceHeader, ThrottleSourceUI} {
			repeated := initial
			repeated.Source, repeated.UpdatedBy, repeated.UpdatedAt = source, "another-client", time.Now()
			repeated.Limit.TokWindow = time.Minute // ignored inactive dimension
			if s.SetThrottle(repeated) {
				t.Fatal("identical effective cap counted as a policy change")
			}
			current, _ := wakeTimerState(&g.wake)
			if currentCap() != initial || g.req != req || g.inFlight != 1 || s.kickC() != kick || current != timer {
				t.Fatal("no-op changed metadata, occupancy, bucket state or wakeups")
			}
		}
		changed := initial
		changed.Limit.Concurrency++
		changed.UpdatedAt = time.Time{}
		if !s.SetThrottle(changed) || !currentCap().UpdatedAt.Equal(time.Now()) {
			t.Fatal("actual policy change was not timestamped")
		}
		if timer, _ := wakeTimerState(&g.wake); timer != nil {
			t.Fatal("policy change retained the old timer with no queued demand")
		}
		if !s.SetThrottle(Throttle{Provider: initial.Provider}) || s.SetThrottle(Throttle{Provider: initial.Provider}) {
			t.Fatal("clear must change an existing policy exactly once")
		}
		s.RestoreThrottles([]Throttle{initial})
		if currentCap() != initial || s.SetThrottle(initial) {
			t.Fatal("restore did not preserve policy metadata")
		}
	})
}

func TestWakeTimerCoalescesRearmsAndCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		w := wakeTimer{fn: func() { calls.Add(1) }}
		start := time.Now()
		w.arm(start.Add(time.Hour), true)
		first, gen := wakeTimerState(&w)
		for i := 0; i < 1000; i++ {
			w.arm(start.Add(time.Hour+time.Duration(i)), true)
		}
		if timer, currentGen := wakeTimerState(&w); timer != first || currentGen != gen {
			t.Fatal("later provider deadlines did not reuse one pending timer")
		}
		w.arm(start.Add(time.Second), true)
		w.arm(start.Add(2*time.Second), false) // extending the group window
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatal("superseded timer fired")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if timer, _ := wakeTimerState(&w); calls.Load() != 1 || timer != nil {
			t.Fatal("replacement timer did not fire exactly once")
		}
		w.arm(time.Now().Add(time.Second), true)
		w.stop()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if timer, _ := wakeTimerState(&w); calls.Load() != 1 || timer != nil {
			t.Fatal("cancelled timer fired")
		}
	})
}

func TestSharedPacingExpiryAndCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		s.Trip("group", time.Second)
		g := s.groupFor("group", 0)
		ctx, cancel := context.WithCancel(context.Background())
		var granted, sent, cancelled atomic.Int32
		for i := 0; i < 16; i++ {
			go func() {
				release, err := s.AcquireWith(ctx, "group", 0, WaiterHooks{})
				if err != nil {
					cancelled.Add(1)
					return
				}
				granted.Add(1)
				release(0)
			}()
			go func() {
				if _, err := s.WaitSend(context.Background(), "group", WaiterHooks{}, false, true); err != nil {
					t.Error(err)
				}
				sent.Add(1)
			}()
		}
		synctest.Wait()
		s.Trip("group", 2*time.Second)
		time.Sleep(time.Second)
		synctest.Wait()
		if granted.Load() != 0 || sent.Load() != 0 {
			t.Fatal("pacing extension admitted requests early")
		}
		cancel()
		synctest.Wait()
		if cancelled.Load() != 16 || s.Stats().Queued != 0 {
			t.Fatal("cancelled paced waiters did not leave the queue")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if timer, _ := wakeTimerState(&g.pacer); sent.Load() != 16 || timer != nil {
			t.Fatal("shared deadline did not wake every sender")
		}
		s.Trip("group", time.Hour)
		go func() {
			release, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{})
			if err != nil {
				t.Error(err)
				return
			}
			granted.Add(1)
			release(0)
		}()
		synctest.Wait()
		s.EndSend("group", false)
		synctest.Wait()
		if timer, _ := wakeTimerState(&g.pacer); granted.Load() != 1 || timer != nil {
			t.Fatal("EndSend did not cancel pacing and wake Acquire")
		}
	})
}

func TestThrottleConcurrentPolicyTimerAndCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "provider.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Requests: 1, ReqWindow: time.Second}})
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Go(func() {
				release, err := s.AcquireWith(ctx, "group", 0, WaiterHooks{Provider: provider})
				if err == nil {
					release(0)
				} else if err != context.Canceled {
					t.Error(err)
				}
			})
		}
		synctest.Wait()
		g := s.gate(provider)
		first, _ := wakeTimerState(&g.wake)
		if first == nil || s.Stats().Queued != 31 {
			t.Fatal("provider throttle did not park requests behind one timer")
		}
		for i := 0; i < 100; i++ {
			s.armThrottleDrain(provider, time.Second)
		}
		if timer, _ := wakeTimerState(&g.wake); timer != first {
			t.Fatal("identical provider deadline reallocated timers")
		}
		wg.Go(func() {
			time.Sleep(time.Second)
			for i := 0; i < 20; i++ {
				s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Requests: int64(i%2 + 1), ReqWindow: time.Second}})
			}
		})
		wg.Go(func() { time.Sleep(time.Second); cancel() })
		wg.Wait()
		s.SetThrottle(Throttle{Provider: provider})
		synctest.Wait()
		if st := s.Stats(); st.Queued != 0 || st.InFlight != 0 {
			t.Fatalf("concurrent timer/policy/cancel leaked slots: %+v", st)
		}
		if timer, _ := wakeTimerState(&g.wake); timer != nil {
			t.Fatal("cleared throttle retained its timer")
		}
	})
}

func TestQueueRemovalPreservesWaitersAndOrder(t *testing.T) {
	s := New(Options{})
	s.AddHold(Hold{ID: "hold", Clients: []string{"held"}})
	g := s.groupFor("group", 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ws := make([]*waiter, 8)
	for i := range ws {
		ws[i] = &waiter{ch: make(chan struct{}, 1), ctx: context.Background()}
	}
	ws[1].client, ws[4].client, ws[2].ctx = "held", "held", ctx
	g.mu.Lock()
	for _, w := range ws {
		g.queue.push(w)
	}
	g.mu.Unlock()
	g.drain()
	if got, want := queueWaiters(g), []*waiter{ws[1], ws[2], ws[4], ws[5], ws[6], ws[7]}; !slices.Equal(got, want) || g.inFlight != 2 {
		t.Fatalf("drain lost held/cancelled waiters or changed FIFO order: queue=%v in_flight=%d", got, g.inFlight)
	}
	g.removeWaiter(ws[2])
	if got, want := queueWaiters(g), []*waiter{ws[1], ws[4], ws[5], ws[6], ws[7]}; !slices.Equal(got, want) || g.inFlight != 2 {
		t.Fatalf("cancelled queued waiter was mistaken for a phantom grant: queue=%v in_flight=%d", got, g.inFlight)
	}
	g.removeWaiter(ws[0]) // granted but caller cancelled before consuming its signal
	if g.inFlight != 2 || len(ws[5].ch) != 1 {
		t.Fatalf("phantom grant did not free its slot for the next FIFO waiter: in_flight=%d", g.inFlight)
	}
}

func TestCancelledThrottleHeadWakesFittingFollower(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "provider.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Tokens: 10, TokWindow: time.Hour}})
		defer s.SetThrottle(Throttle{Provider: provider})
		release, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 8})
		if err != nil {
			t.Fatal(err)
		}
		release(8) // only two tokens remain
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			release, err := s.AcquireWith(ctx, "group", 0, WaiterHooks{Provider: provider, EstTokens: 10})
			if err != context.Canceled {
				t.Errorf("heavy waiter cancellation = %v", err)
			}
			if release != nil {
				release(0)
			}
		}()
		synctest.Wait()
		var granted atomic.Bool
		go func() {
			release, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 1})
			if err != nil {
				t.Error(err)
				return
			}
			granted.Store(true)
			release(1)
		}()
		synctest.Wait()
		if granted.Load() || s.Stats().Queued != 2 {
			t.Fatal("follower bypassed the live FIFO head")
		}
		cancel()
		synctest.Wait()
		if !granted.Load() || s.Stats().Queued != 0 {
			t.Fatal("fitting follower waited for the cancelled head's 48-minute timer")
		}
	})
}

func BenchmarkAcquireUncontended(b *testing.B) {
	s := New(Options{})
	ctx := context.Background()
	release, _ := s.AcquireWith(ctx, "group", 0, WaiterHooks{})
	release(0)
	b.ReportAllocs()
	for b.Loop() {
		release, err := s.AcquireWith(ctx, "group", 0, WaiterHooks{})
		if err != nil {
			b.Fatal(err)
		}
		release(0)
	}
}

func BenchmarkThrottleUnchanged(b *testing.B) {
	s := New(Options{})
	t := Throttle{Provider: "provider.example", Limit: Limit{Requests: 100, ReqWindow: time.Minute}}
	s.SetThrottle(t)
	b.ReportAllocs()
	for b.Loop() {
		s.SetThrottle(t)
	}
}

func BenchmarkThrottleWakeCoalesced(b *testing.B) {
	s := New(Options{})
	provider := "provider.example"
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Requests: 1, ReqWindow: time.Hour}})
	defer s.SetThrottle(Throttle{Provider: provider})
	s.armThrottleDrain(provider, time.Hour)
	b.ReportAllocs()
	for b.Loop() {
		s.armThrottleDrain(provider, time.Hour)
	}
}

func BenchmarkAcquireQueuedCancel(b *testing.B) {
	s := New(Options{})
	release, _ := s.AcquireWith(context.Background(), "group", 1, WaiterHooks{})
	defer release(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.AcquireWith(ctx, "group", 1, WaiterHooks{}); err != context.Canceled {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueueDrain(b *testing.B) {
	for _, n := range []int{32, 256, 2048} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := New(Options{})
			g := s.groupFor("group", 0)
			ws := make([]*waiter, n)
			for i := range ws {
				ws[i] = &waiter{ch: make(chan struct{}, 1), ctx: context.Background()}
			}
			b.ReportAllocs()
			for b.Loop() {
				for _, w := range ws {
					g.queue.push(w)
				}
				g.inFlight = 0
				g.drain()
				for _, w := range ws {
					<-w.ch
				}
			}
		})
	}
}

// BenchmarkThrottledRelease measures the provider-scoped release drain. Groups
// are attributed before the cap is installed (each setup release is then a
// plain group drain, keeping setup O(G)); the measured AcquireWith + release
// runs under an active provider concurrency gate. The provider-to-groups index
// must make the per-release allocation independent of G instead of copying
// every group in the scheduler.
func BenchmarkThrottledRelease(b *testing.B) {
	const provider = "provider.example"
	ctx := context.Background()
	for _, groups := range []int{1, 100, 1000, 10000} {
		b.Run(strconv.Itoa(groups), func(b *testing.B) {
			s := New(Options{})
			keys := make([]string, groups)
			for i := range keys {
				keys[i] = provider + "|" + strconv.Itoa(i)
				release, err := s.AcquireWith(ctx, keys[i], 0, WaiterHooks{Provider: provider})
				if err != nil {
					b.Fatal(err)
				}
				release(0)
			}
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				release, err := s.AcquireWith(ctx, keys[0], 0, WaiterHooks{Provider: provider})
				if err != nil {
					b.Fatal(err)
				}
				release(0)
			}
		})
	}
}

// BenchmarkThrottledReleaseOtherProviders holds the released provider at one
// group and adds G groups on other providers. A provider-scoped drain must be
// independent of unrelated group count in both time and allocation.
func BenchmarkThrottledReleaseOtherProviders(b *testing.B) {
	const provider = "provider.example"
	ctx := context.Background()
	for _, groups := range []int{1, 100, 1000, 10000} {
		b.Run(strconv.Itoa(groups), func(b *testing.B) {
			s := New(Options{})
			release, err := s.AcquireWith(ctx, provider+"|target", 0, WaiterHooks{Provider: provider})
			if err != nil {
				b.Fatal(err)
			}
			release(0)
			for i := 0; i < groups; i++ {
				other := "other-" + strconv.Itoa(i) + ".example"
				release, err := s.AcquireWith(ctx, other+"|key", 0, WaiterHooks{Provider: other})
				if err != nil {
					b.Fatal(err)
				}
				release(0)
			}
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
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

// BenchmarkIdleProviderDrain measures a provider-scoped drain when every
// attributed group has an empty queue. The provider queued-group counter must
// let the drain return before locking any group, so time and allocation must
// stay flat as G grows instead of scaling with the group count.
func BenchmarkIdleProviderDrain(b *testing.B) {
	const provider = "provider.example"
	ctx := context.Background()
	for _, groups := range []int{1, 1000, 10000} {
		b.Run(strconv.Itoa(groups), func(b *testing.B) {
			s := New(Options{})
			for i := 0; i < groups; i++ {
				release, err := s.AcquireWith(ctx, provider+"|"+strconv.Itoa(i), 0, WaiterHooks{Provider: provider})
				if err != nil {
					b.Fatal(err)
				}
				release(0)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				s.drainProvider(provider)
			}
		})
	}
}

// BenchmarkThrottlePolicyListing contrasts the policy-only persistence listing
// with the dashboard's live-occupancy listing. G groups are precreated and
// attributed to one provider with one active gate; queued waiters are installed
// directly in group queues because the benchmark measures the listing walk, not
// queue admission. ThrottlePolicies must stay independent of group count and
// queue depth, while ListThrottles must visibly scale with both.
func BenchmarkThrottlePolicyListing(b *testing.B) {
	const provider = "provider.example"
	for _, groups := range []int{1, 1000, 10000} {
		for _, queued := range []int{0, 4} {
			b.Run("groups="+strconv.Itoa(groups)+"/queued="+strconv.Itoa(queued), func(b *testing.B) {
				s := New(Options{})
				s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1, Requests: 1, ReqWindow: time.Hour}})
				for i := 0; i < groups; i++ {
					g := s.groupFor(provider+"|"+strconv.Itoa(i), 0)
					for j := 0; j < queued; j++ {
						g.queue.push(&waiter{
							ch:       make(chan struct{}, 1),
							ctx:      context.Background(),
							provider: provider,
						})
					}
				}
				b.Run("policies", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						s.ThrottlePolicies()
					}
				})
				b.Run("live", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						s.ListThrottles()
					}
				})
			})
		}
	}
}

// TestGateLookupNeverWaitsForThrottleWriters pins the lock-order fix: gate()
// is called from admission while a group mutex is held, and throttle writers
// hold throttleMu while taking that same group mutex. A gate lookup must read
// an immutable snapshot, never acquire throttleMu, or the two orders deadlock.
func TestGateLookupNeverWaitsForThrottleWriters(t *testing.T) {
	s := New(Options{})
	provider := "provider.example"
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 2}})
	g := s.gate(provider)
	if g == nil {
		t.Fatal("gate not installed")
	}
	g.mu.Lock() // simulate admission holding the group mutex
	updated := make(chan struct{})
	go func() {
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 3}})
		close(updated)
	}()
	// Wait until the writer holds throttleMu and is blocked on g.mu.
	deadline := time.Now().Add(2 * time.Second)
	for s.throttleMu.TryLock() {
		s.throttleMu.Unlock()
		if time.Now().After(deadline) {
			g.mu.Unlock()
			t.Fatal("throttle writer did not take throttleMu")
		}
		runtime.Gosched()
	}
	lookup := make(chan *providerGate, 1)
	go func() { lookup <- s.gate(provider) }()
	select {
	case got := <-lookup:
		if got != g {
			g.mu.Unlock()
			t.Fatal("gate lookup returned a different gate")
		}
	case <-time.After(2 * time.Second):
		g.mu.Unlock()
		t.Fatal("gate lookup blocked behind the throttle writer: lock-order inversion")
	}
	g.mu.Unlock()
	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		t.Fatal("throttle writer did not finish after the group lock was released")
	}
}
