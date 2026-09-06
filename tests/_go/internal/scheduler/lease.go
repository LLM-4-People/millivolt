package scheduler

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestAcquisitionOwnsOriginalThrottle(t *testing.T) {
	for _, initialCap := range []bool{false, true} {
		t.Run(map[bool]string{false: "initially-unlimited", true: "cleared-and-reinstalled"}[initialCap], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := New(Options{})
				provider := "provider.example"
				cap := Throttle{Provider: provider, Limit: Limit{Concurrency: 1, Tokens: 100, TokWindow: time.Hour}}
				if initialCap {
					s.SetThrottle(cap)
				}
				first, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 40})
				if err != nil {
					t.Fatal(err)
				}
				s.ClearThrottle(provider)
				s.SetThrottle(cap)
				second, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 20})
				if err != nil {
					t.Fatal(err)
				}
				defer second(0)
				g := s.gate(provider)
				before := g.snapshot(0)
				first(30)
				if after := g.snapshot(0); after.InFlight != before.InFlight || after.TokRemaining != before.TokRemaining || g.reserved != 20 {
					t.Fatalf("old acquisition changed replacement gate: before=%+v after=%+v reserved=%d", before, after, g.reserved)
				}
			})
		})
	}
}

func TestReleaseSettlesExactlyOnceAcrossPolicyChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "provider.example"
		limit := Throttle{Provider: provider, Limit: Limit{Tokens: 100, TokWindow: time.Hour}}
		s.SetThrottle(limit)
		release, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 40})
		if err != nil {
			t.Fatal(err)
		}
		limit.Limit.Tokens = 200
		s.SetThrottle(limit) // an in-place change keeps the same acquisition owner
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Go(func() { release(25) })
		}
		wg.Wait()
		g := s.gate(provider)
		if after := g.snapshot(0); after.TokRemaining != 175 || after.InFlight != 0 || g.reserved != 0 || s.Stats().InFlight != 0 {
			t.Fatalf("usage or slot was settled more than once: %+v reserved=%d", after, g.reserved)
		}
	})
}

func TestCancelledGrantedWaiterUsesOriginalGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		provider := "provider.example"
		limit := Throttle{Provider: provider, Limit: Limit{Concurrency: 1, Tokens: 100, TokWindow: time.Hour}}
		s.SetThrottle(limit)
		g := s.groupFor("group", 0)
		g.provider = provider
		w := &waiter{ch: make(chan struct{}, 1), ctx: context.Background(), provider: provider, estTokens: 40}
		g.queue = []*waiter{w}
		g.drain() // grant sent, but the caller has not consumed it
		s.ClearThrottle(provider)
		s.SetThrottle(limit)
		release, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 20})
		if err != nil {
			t.Fatal(err)
		}
		defer release(0)
		current := s.gate(provider)
		before := current.snapshot(0)
		g.removeWaiter(w)
		if after := current.snapshot(0); after.InFlight != before.InFlight || after.TokRemaining != before.TokRemaining || current.reserved != 20 {
			t.Fatalf("phantom cancellation mutated replacement gate: %+v", after)
		}
	})
}

func TestHoldExpiresAtExactDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		h := Hold{ID: "hold", All: true, Until: time.Now().Add(time.Second)}
		if err := s.AddHold(h); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if h.Active() || s.Paused() || s.HoldsTarget("client", "provider.example") {
			t.Fatal("hold remained active at its exact deadline")
		}
	})
}

func TestAcquisitionOwnsTokenBucketGeneration(t *testing.T) {
	for _, initialTokens := range []int64{0, 100} {
		t.Run(map[int64]string{0: "enabled-after-grant", 100: "disabled-and-reenabled"}[initialTokens], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := New(Options{})
				provider := "provider.example"
				limit := Throttle{Provider: provider, Limit: Limit{Concurrency: 3, Tokens: initialTokens, TokWindow: time.Hour}}
				s.SetThrottle(limit)
				first, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 40})
				if err != nil {
					t.Fatal(err)
				}
				limit.Limit.Tokens = 0
				s.SetThrottle(limit)
				limit.Limit.Tokens = 100
				s.SetThrottle(limit)
				second, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: provider, EstTokens: 20})
				if err != nil {
					t.Fatal(err)
				}
				defer second(0)
				first(30)
				g := s.gate(provider)
				if after := g.snapshot(0); after.TokRemaining != 80 || after.InFlight != 1 || g.reserved != 20 {
					t.Fatalf("old token lease changed newly enabled bucket: %+v reserved=%d", after, g.reserved)
				}
			})
		})
	}
}

func TestWaitSendHoldSignalRechecksResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		if err := s.AddHold(Hold{ID: "hold", All: true}); err != nil {
			t.Fatal(err)
		}
		waits, holds := 0, 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := s.WaitSend(ctx, "group", WaiterHooks{
			OnWait: func() { waits++; s.RemoveHold("hold") },
			OnHold: func() { holds++ },
		}, true, false)
		if err != nil || waits != 1 || holds != 0 {
			t.Fatalf("known-wait signal lost resume or published stale hold: err=%v waits=%d holds=%d", err, waits, holds)
		}
	})
}
