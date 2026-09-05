package scheduler

import (
	"context"
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
		timer, _ := wakeTimerState(&g.wake)
		for _, source := range []string{ThrottleSourceHeader, ThrottleSourceUI} {
			repeated := initial
			repeated.Source, repeated.UpdatedBy, repeated.UpdatedAt = source, "another-client", time.Now()
			repeated.Limit.TokWindow = time.Minute // ignored inactive dimension
			if s.SetThrottle(repeated) {
				t.Fatal("identical effective cap counted as a policy change")
			}
			current, _ := wakeTimerState(&g.wake)
			if s.ThrottleFor(initial.Provider) != initial || g.req != req || g.inFlight != 1 || s.kickC() != kick || current != timer {
				t.Fatal("no-op changed metadata, occupancy, bucket state or wakeups")
			}
		}
		changed := initial
		changed.Limit.Concurrency++
		changed.UpdatedAt = time.Time{}
		if !s.SetThrottle(changed) || !s.ThrottleFor(initial.Provider).UpdatedAt.Equal(time.Now()) {
			t.Fatal("actual policy change was not timestamped")
		}
		if timer, _ := wakeTimerState(&g.wake); timer != nil {
			t.Fatal("policy change retained the old timer with no queued demand")
		}
		if !s.SetThrottle(Throttle{Provider: initial.Provider}) || s.SetThrottle(Throttle{Provider: initial.Provider}) {
			t.Fatal("clear must change an existing policy exactly once")
		}
		s.RestoreThrottles([]Throttle{initial})
		if s.ThrottleFor(initial.Provider) != initial || s.SetThrottle(initial) {
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
		s.SetRateLimit("group", time.Second)
		g := s.groupFor("group", 0)
		ctx, cancel := context.WithCancel(context.Background())
		var granted, sent, cancelled atomic.Int32
		for i := 0; i < 16; i++ {
			go func() {
				release, err := s.Acquire(ctx, "group", 0)
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
		s.SetRateLimit("group", 2*time.Second)
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
		s.SetRateLimit("group", time.Hour)
		go func() {
			release, err := s.Acquire(context.Background(), "group", 0)
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
		s.ClearThrottle(provider)
		synctest.Wait()
		if st := s.Stats(); st.Queued != 0 || st.InFlight != 0 {
			t.Fatalf("concurrent timer/policy/cancel leaked slots: %+v", st)
		}
		if timer, _ := wakeTimerState(&g.wake); timer != nil {
			t.Fatal("cleared throttle retained its timer")
		}
	})
}

func TestQueueCompactionPreservesWaitersAndClearsTail(t *testing.T) {
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
	g.queue = append([]*waiter(nil), ws...)
	backing := g.queue[:cap(g.queue)]
	g.drain()
	if !slices.Equal(g.queue, []*waiter{ws[1], ws[2], ws[4], ws[5], ws[6], ws[7]}) || g.inFlight != 2 {
		t.Fatal("compaction lost held/cancelled waiters or changed FIFO suffix")
	}
	for _, w := range backing[len(g.queue):] {
		if w != nil {
			t.Fatal("departed grant retained in queue backing array")
		}
	}
	g.removeWaiter(ws[2])
	if g.inFlight != 2 || !slices.Equal(g.queue, []*waiter{ws[1], ws[4], ws[5], ws[6], ws[7]}) {
		t.Fatal("cancelled queued waiter was mistaken for a phantom grant")
	}
	for _, w := range backing[len(g.queue):] {
		if w != nil {
			t.Fatal("cancelled waiter retained in queue backing array")
		}
	}
	g.removeWaiter(ws[0]) // granted but caller cancelled before consuming its signal
	if g.inFlight != 2 || len(ws[5].ch) != 1 {
		t.Fatal("phantom grant did not free its slot for the next FIFO waiter")
	}
}

func TestCancelledThrottleHeadWakesFittingFollower(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "provider.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Tokens: 10, TokWindow: time.Hour}})
		defer s.ClearThrottle(provider)
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
	release, _ := s.Acquire(ctx, "group", 0)
	release(0)
	b.ReportAllocs()
	for b.Loop() {
		release, err := s.Acquire(ctx, "group", 0)
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
	defer s.ClearThrottle(provider)
	s.armThrottleDrain(provider, time.Hour)
	b.ReportAllocs()
	for b.Loop() {
		s.armThrottleDrain(provider, time.Hour)
	}
}

func BenchmarkAcquireQueuedCancel(b *testing.B) {
	s := New(Options{})
	release, _ := s.Acquire(context.Background(), "group", 1)
	defer release(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Acquire(ctx, "group", 1); err != context.Canceled {
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
			g.queue = make([]*waiter, 0, n)
			b.ReportAllocs()
			for b.Loop() {
				g.queue = append(g.queue[:0], ws...)
				g.inFlight = 0
				g.drain()
				for _, w := range ws {
					<-w.ch
				}
			}
		})
	}
}
