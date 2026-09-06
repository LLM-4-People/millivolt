package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThrottleConcurrencyAcrossKeys(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "alpha.example", Limit: Limit{Concurrency: 1}})
	ctx := context.Background()

	r1, err := s.AcquireWith(ctx, "alpha.example|k1", 0, WaiterHooks{Provider: "alpha.example"})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		r2, err := s.AcquireWith(ctx, "alpha.example|k2", 0, WaiterHooks{Provider: "alpha.example"})
		if err != nil {
			return
		}
		r2(0)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("second key granted while provider concurrency is 1")
	case <-time.After(40 * time.Millisecond):
	}
	r1(0)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second key did not grant after release")
	}
}

func TestThrottleConcurrencyDoesNotAffectOtherProvider(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "alpha.example", Limit: Limit{Concurrency: 1}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "alpha.example|k", 0, WaiterHooks{Provider: "alpha.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer r1(0)
	r2, err := s.AcquireWith(ctx, "beta.example|k", 0, WaiterHooks{Provider: "beta.example"})
	if err != nil {
		t.Fatal(err)
	}
	r2(0)
}

func TestThrottleRequestsPerWindow(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Requests: 1, ReqWindow: 80 * time.Millisecond}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	r1(0) // RPM is consumed at admit, not returned at release

	blocked := make(chan struct{})
	go func() {
		close(blocked)
		r2, err := s.AcquireWith(ctx, "p|k2", 0, WaiterHooks{Provider: "p"})
		if err != nil {
			return
		}
		r2(0)
	}()
	<-blocked
	// r2 must be parked on the exhausted RPM window before r3 joins the
	// queue, so r3 truly measures the wait for the window to roll - poll the
	// observable (queue length) instead of sleeping a fixed window.
	waitFor(t, func() bool { return s.Stats().Queued >= 1 }, "throttled r2 to park on the RPM window")
	start := time.Now()
	r3, err := s.AcquireWith(ctx, "p|k3", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	r3(0)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("third admit returned in %v; want to wait out the 80ms window", elapsed)
	}
}

func TestThrottleTokensReserveAndSettle(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Tokens: 100, TokWindow: time.Hour}})
	ctx := context.Background()

	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p", EstTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if reserved := s.gate("p").reserved; reserved != 100 {
		t.Fatalf("reserved = %d, want 100", reserved)
	}
	r1(100)

	// Bucket is empty; another admit waits (hour window).
	start := time.Now()
	ctx2, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	_, err = s.AcquireWith(ctx2, "p|k2", 0, WaiterHooks{Provider: "p", EstTokens: 1})
	if err == nil {
		t.Fatal("expected wait/timeout on token bucket")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatalf("token wait returned too fast: %v", time.Since(start))
	}
}

func TestThrottleSettleRefundsReservation(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Tokens: 50, TokWindow: time.Hour}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p", EstTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	r1(0) // cancel: refund

	r2, err := s.AcquireWith(ctx, "p|k2", 0, WaiterHooks{Provider: "p", EstTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	r2(0)
}

func TestThrottleClearUnblocksWaiters(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 1}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer r1(0)

	done := make(chan struct{})
	go func() {
		r2, err := s.AcquireWith(ctx, "p|k2", 0, WaiterHooks{Provider: "p"})
		if err != nil {
			return
		}
		r2(0)
		close(done)
	}()
	// The waiter must be parked on the concurrency cap before the clear, so
	// the clear truly exercises the wakeup of a parked waiter.
	waitFor(t, func() bool { return s.Stats().Queued >= 1 }, "throttled waiter to queue")
	s.ClearThrottle("p")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("clear did not unblock waiter")
	}
}

func TestThrottleOffIsUnlimited(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	var rels []Release
	for i := 0; i < 8; i++ {
		r, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p"})
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, r)
	}
	for _, r := range rels {
		r(0)
	}
}

func TestThrottleOnThrottleHook(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 1}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}

	var throttled atomic.Bool
	done := make(chan struct{})
	go func() {
		r2, err := s.AcquireWith(ctx, "p|k2", 0, WaiterHooks{
			Provider: "p",
			OnThrottle: func() {
				throttled.Store(true)
			},
			OnUnthrottle: func() {
				throttled.Store(false)
			},
		})
		if err != nil {
			return
		}
		r2(0)
		close(done)
	}()
	// OnThrottle firing IS the observable - poll for it instead of hoping a
	// fixed window was long enough for the hook to run.
	waitFor(t, throttled.Load, "OnThrottle to fire while blocked")
	// Clearing the cap is the observable lift: kickThrottles (pauseMu, never
	// holdCountMu) wakes the parked waiter and its wait loop re-checks
	// throttleBlocked → OnUnthrottle must fire before the grant returns.
	s.ClearThrottle("p")
	waitFor(t, func() bool { return !throttled.Load() }, "OnUnthrottle to fire after ClearThrottle")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter did not finish")
	}
	r1(0)
}

func TestThrottleLoosenConcurrencyGrantsWaiters(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 1}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer r1(0)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			r, err := s.AcquireWith(ctx, "p|kx", 0, WaiterHooks{Provider: "p"})
			if err != nil {
				t.Error(err)
				return
			}
			r(0)
		}()
	}
	// Both waiters must be parked on the cap before loosening it.
	waitFor(t, func() bool { return s.Stats().Queued >= 2 }, "two throttled waiters to queue")
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 8}})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loosened cap did not grant waiters")
	}
}

func TestThrottleListAndRestore(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "b.com", Limit: Limit{Concurrency: 2}, Source: ThrottleSourceHeader, UpdatedBy: "client-a"})
	s.SetThrottle(Throttle{Provider: "a.com", Limit: Limit{Requests: 10, ReqWindow: time.Minute}, Source: ThrottleSourceUI, UpdatedBy: "dashboard"})
	got := s.ListThrottles()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Provider != "a.com" || got[1].Provider != "b.com" {
		t.Fatalf("order = %s,%s", got[0].Provider, got[1].Provider)
	}
	if got[1].Source != ThrottleSourceHeader || got[1].UpdatedBy != "client-a" {
		t.Fatalf("header source lost: %+v", got[1])
	}

	s2 := New(Options{})
	var ts []Throttle
	for _, inf := range got {
		ts = append(ts, inf.Throttle)
	}
	s2.RestoreThrottles(ts)
	if s2.ThrottleFor("b.com").Limit.Concurrency != 2 {
		t.Fatal("restore missed concurrency")
	}
	if s2.ThrottleFor("a.com").Limit.Requests != 10 {
		t.Fatal("restore missed requests")
	}
}

func TestThrottleOversizeRequestAdmitted(t *testing.T) {
	// A single request larger than the window is allowed (can't fragment it);
	// it exhausts the bucket so the next admit waits.
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Tokens: 10, TokWindow: time.Hour}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 0, WaiterHooks{Provider: "p", EstTokens: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	r1(10_000)
	ctx2, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, err := s.AcquireWith(ctx2, "p|k2", 0, WaiterHooks{Provider: "p", EstTokens: 1}); err == nil {
		t.Fatal("next request should wait on exhausted token bucket")
	}
}

func TestPerKeyCapStillAppliesUnderProviderThrottle(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 8}})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "p|k", 1, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		r2, err := s.AcquireWith(ctx, "p|k", 1, WaiterHooks{Provider: "p"})
		if err != nil {
			return
		}
		r2(0)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("per-key cap of 1 was bypassed")
	case <-time.After(30 * time.Millisecond):
	}
	r1(0)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("per-key waiter stuck")
	}
}
