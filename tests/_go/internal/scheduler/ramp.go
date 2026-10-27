package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// rampState reads a group's anti-burst epoch fields under its lock.
func rampState(t *testing.T, s *Scheduler, key string) (start, idle time.Time) {
	t.Helper()
	g := s.groupFor(key, 0)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rampStart, g.idleSince
}

// rampAllowanceNow evaluates the group's live allowance with the same
// floor-based formula the admission path uses.
func rampAllowanceNow(t *testing.T, s *Scheduler, key string, target int, duration time.Duration) int {
	t.Helper()
	g := s.groupFor(key, 0)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rampAllowanceLocked(time.Now(), target, duration)
}

// rampGrant receives one granted Release. The virtual timer only exists to
// make a missing grant fail the test instead of hanging the bubble.
func rampGrant(t *testing.T, ch <-chan Release, what string) Release {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case release := <-ch:
		return release
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// queueAcquire launches one blocking Acquire and sends its Release, so the
// test keeps the slot occupied until it chooses to release it.
func queueAcquire(t *testing.T, s *Scheduler, ctx context.Context, key string, hooks WaiterHooks, out chan<- Release) {
	t.Helper()
	go func() {
		release, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Errorf("acquire %s: %v", key, err)
			return
		}
		out <- release
	}()
}

// TestRampDisabledIsNoOp pins each "off" shape: a zero policy, AntiBurst false
// with a duration set, and a zero RampDuration must all admit at full
// concurrency immediately and leave no epoch or provider timer behind.
func TestRampDisabledIsNoOp(t *testing.T) {
	const (
		target = 4
		ramp   = 40 * time.Millisecond
	)
	provider := "ramp.example"
	key := provider + "|k"
	cases := []struct {
		name   string
		capped bool
		setup  func(*Scheduler)
	}{
		{name: "zero-policy", setup: func(*Scheduler) {}},
		{name: "flag-off", capped: true, setup: func(s *Scheduler) {
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: target, RampDuration: ramp}})
		}},
		{name: "zero-duration", capped: true, setup: func(s *Scheduler) {
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: target, AntiBurst: true}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := New(Options{})
				tc.setup(s)
				ctx := context.Background()
				hooks := WaiterHooks{Provider: provider}

				releases := make([]Release, 0, target)
				for i := 0; i < target; i++ {
					release, err := s.AcquireWith(ctx, key, 0, hooks)
					if err != nil {
						t.Fatal(err)
					}
					releases = append(releases, release)
				}
				if st := s.Stats(); st.InFlight != target || st.Queued != 0 {
					t.Fatalf("occupancy = %+v, want all %d admitted immediately", st, target)
				}
				if gate := s.gate(provider); gate != nil {
					if timer, _ := wakeTimerState(&gate.wake); timer != nil {
						t.Fatal("disabled ramp armed a provider wake")
					}
				}
				if start, idle := rampState(t, s, key); !start.IsZero() || !idle.IsZero() {
					t.Fatalf("disabled ramp retained epoch start=%v idle=%v", start, idle)
				}

				if tc.capped {
					blocked := make(chan Release, 1)
					queueAcquire(t, s, ctx, key, hooks, blocked)
					synctest.Wait()
					if got := s.Stats().Queued; got != 1 {
						t.Fatalf("queued = %d, want the fifth request parked on the provider cap", got)
					}
					releases[0](0)
					fifth := rampGrant(t, blocked, "fifth admission")
					fifth(0)
					for _, release := range releases[1:] {
						release(0)
					}
				} else {
					// No provider cap at all: every request must still admit
					// immediately, and the extra grants must not create state.
					for i := 0; i < target; i++ {
						release, err := s.AcquireWith(ctx, key, 0, hooks)
						if err != nil {
							t.Fatal(err)
						}
						releases = append(releases, release)
					}
					if st := s.Stats(); st.InFlight != 2*target || st.Queued != 0 {
						t.Fatalf("unlimited occupancy = %+v, want %d in flight", st, 2*target)
					}
					for _, release := range releases {
						release(0)
					}
				}
				synctest.Wait()
				if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
					t.Fatalf("disabled ramp leaked occupancy: %+v", st)
				}
				if start, idle := rampState(t, s, key); !start.IsZero() || !idle.IsZero() {
					t.Fatalf("disabled ramp wrote state after traffic: start=%v idle=%v", start, idle)
				}
				if gate := s.gate(provider); gate != nil {
					if timer, _ := wakeTimerState(&gate.wake); timer != nil {
						t.Fatal("disabled ramp left a provider timer")
					}
				}
				for _, policy := range s.ThrottlePolicies() {
					if policy.Limit.Concurrency != target {
						t.Fatalf("configured target changed to %d, want %d", policy.Limit.Concurrency, target)
					}
				}
			})
		})
	}
}

// TestRampGrowsAllowanceToTarget pins the step curve for cap 4 over 40ms: one
// grant at t0, two after the first 1/3 step, three after the second, and four
// at the full duration, with a provider timer only while below target.
func TestRampGrowsAllowanceToTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			target = 4
			ramp   = 40 * time.Millisecond
		)
		s := New(Options{})
		provider := "ramp.example"
		key := provider + "|k"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
			Concurrency: target, AntiBurst: true, RampDuration: ramp,
		}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		first, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Fatal(err)
		}
		start, idle := rampState(t, s, key)
		if start.IsZero() || !idle.IsZero() {
			t.Fatalf("first grant epoch = start:%v idle:%v, want a running epoch", start, idle)
		}

		grants := make(chan Release, target)
		for i := 0; i < target-1; i++ {
			queueAcquire(t, s, ctx, key, hooks, grants)
		}
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 1 || st.Queued != target-1 {
			t.Fatalf("at t0 occupancy = %+v, want 1 in flight and %d queued", st, target-1)
		}
		if timer, _ := wakeTimerState(&s.gate(provider).wake); timer == nil {
			t.Fatal("first ramp step was not armed")
		}

		time.Sleep(14 * time.Millisecond)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 2 || st.Queued != target-2 {
			t.Fatalf("after first step occupancy = %+v, want 2 in flight", st)
		}

		time.Sleep(13 * time.Millisecond)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 3 || st.Queued != target-3 {
			t.Fatalf("after second step occupancy = %+v, want 3 in flight", st)
		}

		time.Sleep(13 * time.Millisecond)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != target || st.Queued != 0 {
			t.Fatalf("at full duration occupancy = %+v, want %d in flight", st, target)
		}
		if timer, _ := wakeTimerState(&s.gate(provider).wake); timer != nil {
			t.Fatal("ramp retained a timer after reaching the target")
		}

		first(0)
		for i := 0; i < target-1; i++ {
			rampGrant(t, grants, "ramp grant")(0)
		}
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("ramp drain leaked occupancy: %+v", st)
		}
	})
}

// TestRampIdleEpochReset pins both sides of the idle rule: a full RampDuration
// of idleness starts a fresh epoch on the next first grant, while a short idle
// gap keeps the current epoch and its elapsed ramp.
func TestRampIdleEpochReset(t *testing.T) {
	const (
		target = 4
		ramp   = 40 * time.Millisecond
	)
	provider := "ramp.example"
	key := provider + "|k"

	t.Run("full-duration-resets", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
				Concurrency: target, AntiBurst: true, RampDuration: ramp,
			}})
			ctx := context.Background()
			hooks := WaiterHooks{Provider: provider}

			first, err := s.AcquireWith(ctx, key, 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			start0, _ := rampState(t, s, key)
			first(0)
			_, idle := rampState(t, s, key)
			if idle.IsZero() {
				t.Fatal("full idle did not stamp idleSince")
			}
			time.Sleep(ramp)
			second, err := s.AcquireWith(ctx, key, 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			start1, idle1 := rampState(t, s, key)
			if start1.Equal(start0) {
				t.Fatalf("epoch %v survived a full idle duration", start0)
			}
			if !idle1.IsZero() {
				t.Fatal("new epoch kept a stale idleSince")
			}

			waitCtx, cancelWait := context.WithCancel(ctx)
			var wg sync.WaitGroup
			grants := make(chan Release, target)
			for i := 0; i < target-1; i++ {
				wg.Go(func() {
					release, err := s.AcquireWith(waitCtx, key, 0, hooks)
					if err == context.Canceled {
						return
					}
					if err != nil {
						t.Errorf("fresh-epoch waiter: %v", err)
						return
					}
					grants <- release
				})
			}
			synctest.Wait()
			if got := s.Stats().Queued; got != target-1 {
				t.Fatalf("fresh epoch queued = %d, want %d behind allowance 1", got, target-1)
			}
			cancelWait()
			synctest.Wait()
			wg.Wait()
			// Let the pending ramp wake fire against an empty queue.
			time.Sleep(14 * time.Millisecond)
			synctest.Wait()
			second(0)
			if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
				t.Fatalf("fresh-epoch cleanup leaked occupancy: %+v", st)
			}
		})
	})

	t.Run("short-idle-keeps-epoch", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
				Concurrency: target, AntiBurst: true, RampDuration: ramp,
			}})
			ctx := context.Background()
			hooks := WaiterHooks{Provider: provider}

			first, err := s.AcquireWith(ctx, key, 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			start0, _ := rampState(t, s, key)
			first(0)
			time.Sleep(10 * time.Millisecond)
			second, err := s.AcquireWith(ctx, key, 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			start1, idle1 := rampState(t, s, key)
			if !start1.Equal(start0) {
				t.Fatalf("short idle restarted the epoch: %v then %v", start0, start1)
			}
			if !idle1.IsZero() {
				t.Fatal("first grant after a short idle kept idleSince")
			}

			grants := make(chan Release, target)
			for i := 0; i < target-1; i++ {
				queueAcquire(t, s, ctx, key, hooks, grants)
			}
			synctest.Wait()
			if got := s.Stats().Queued; got != target-1 {
				t.Fatalf("short-idle queued = %d, want allowance still 1 at 10ms", got)
			}
			time.Sleep(30 * time.Millisecond)
			synctest.Wait()
			if st := s.Stats(); st.InFlight != target || st.Queued != 0 {
				t.Fatalf("kept epoch occupancy = %+v, want %d by the original 40ms deadline", st, target)
			}
			second(0)
			for i := 0; i < target-1; i++ {
				rampGrant(t, grants, "kept-epoch grant")(0)
			}
			synctest.Wait()
			if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
				t.Fatalf("kept-epoch cleanup leaked occupancy: %+v", st)
			}
		})
	})
}

// TestRampEnabledMidFlightAnchors pins the enable transition: work already in
// flight when anti-burst turns on must anchor the epoch at its occupancy, so
// the next request is admitted immediately and the remaining ramp resumes a
// few milliseconds later instead of stalling for a full duration.
func TestRampEnabledMidFlightAnchors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			target = 8
			ramp   = 40 * time.Millisecond
		)
		s := New(Options{})
		provider := "ramp.example"
		key := provider + "|k"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: target}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}

		holders := make([]Release, 0, 3)
		for i := 0; i < 3; i++ {
			release, err := s.AcquireWith(ctx, key, 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			holders = append(holders, release)
		}
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
			Concurrency: target, AntiBurst: true, RampDuration: ramp,
		}})

		enabledAt := time.Now()
		fourth, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Fatal(err)
		}
		start, _ := rampState(t, s, key)
		wantStart := enabledAt.Add(-rampStepDuration(3, target, ramp))
		if !start.Equal(wantStart) {
			t.Fatalf("anchored epoch = %v, want %v (occupancy 3)", start, wantStart)
		}
		if st := s.Stats(); st.InFlight != 4 || st.Queued != 0 {
			t.Fatalf("fourth admission occupancy = %+v, want immediate grant at occupancy 4", st)
		}

		grants := make(chan Release, 2)
		queueAcquire(t, s, ctx, key, hooks, grants)
		queueAcquire(t, s, ctx, key, hooks, grants)
		synctest.Wait()
		if got := s.Stats().Queued; got != 2 {
			t.Fatalf("queued = %d, want 2 behind anchored allowance 4", got)
		}
		time.Sleep(6 * time.Millisecond)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 5 || st.Queued != 1 {
			t.Fatalf("first anchored step occupancy = %+v, want 5 in flight", st)
		}
		time.Sleep(6 * time.Millisecond)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 6 || st.Queued != 0 {
			t.Fatalf("second anchored step occupancy = %+v, want 6 in flight", st)
		}

		fourth(0)
		for i := 0; i < 2; i++ {
			rampGrant(t, grants, "anchored grant")(0)
		}
		for _, release := range holders {
			release(0)
		}
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("anchored ramp leaked occupancy: %+v", st)
		}
	})
}

// TestRampPolicyChangeResetsEpoch pins epoch lifetime under policy edits: an
// identical limit and a token-only change preserve the epoch, while a changed
// concurrency target clears it and the next admission anchors at occupancy
// against the new target.
func TestRampPolicyChangeResetsEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			target = 4
			ramp   = 40 * time.Millisecond
		)
		s := New(Options{})
		provider := "ramp.example"
		key := provider + "|k"
		base := Throttle{
			Provider:  provider,
			Limit:     Limit{Concurrency: target, AntiBurst: true, RampDuration: ramp},
			Source:    ThrottleSourceUI,
			UpdatedBy: "dashboard",
		}
		if !s.SetThrottle(base) {
			t.Fatal("initial policy not installed")
		}
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}
		first, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Fatal(err)
		}
		start0, _ := rampState(t, s, key)
		time.Sleep(20 * time.Millisecond)

		repeated := base
		repeated.UpdatedBy = "another-client"
		if s.SetThrottle(repeated) {
			t.Fatal("identical effective policy counted as a change")
		}
		if start, _ := rampState(t, s, key); !start.Equal(start0) {
			t.Fatal("identical policy reset the epoch")
		}

		tokenOnly := base
		tokenOnly.Limit.Tokens = 1000
		tokenOnly.Limit.TokWindow = time.Hour
		if !s.SetThrottle(tokenOnly) {
			t.Fatal("token-only change was not installed")
		}
		if start, _ := rampState(t, s, key); !start.Equal(start0) {
			t.Fatal("token-only change reset the ramp epoch")
		}

		clamped := tokenOnly
		clamped.Limit.Concurrency = 2
		if !s.SetThrottle(clamped) {
			t.Fatal("target change was not installed")
		}
		if start, _ := rampState(t, s, key); !start.IsZero() {
			t.Fatalf("epoch %v survived a concurrency target change", start)
		}
		fourth, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Fatal(err)
		}
		start, _ := rampState(t, s, key)
		if start.IsZero() {
			t.Fatal("admission after the target change did not anchor an epoch")
		}
		if allowance := rampAllowanceNow(t, s, key, 2, ramp); allowance != 2 {
			t.Fatalf("anchored allowance = %d, want the new target 2", allowance)
		}
		if got := s.ThrottlePolicies()[0].Limit.Concurrency; got != 2 {
			t.Fatalf("live policy reports concurrency %d, want the configured target 2", got)
		}
		if st := s.Stats(); st.InFlight != 2 || st.Queued != 0 {
			t.Fatalf("post-reset occupancy = %+v, want 2 in flight", st)
		}

		fourth(0)
		first(0)
		synctest.Wait()
	})
}

// TestRampInertBelowTwoConcurrency pins that anti-burst requires a real ramp
// slope: concurrency 1 is just the existing provider cap, and concurrency 0 is
// unlimited concurrency rather than an allowance of one.
func TestRampInertBelowTwoConcurrency(t *testing.T) {
	provider := "ramp.example"
	key := provider + "|k"
	const ramp = 40 * time.Millisecond

	t.Run("concurrency-1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
				Concurrency: 1, AntiBurst: true, RampDuration: ramp,
			}})
			ctx := context.Background()
			hooks := WaiterHooks{Provider: provider}
			first, err := s.AcquireWith(ctx, key, 0, hooks)
			if err != nil {
				t.Fatal(err)
			}
			if start, _ := rampState(t, s, key); !start.IsZero() {
				t.Fatal("concurrency 1 started a ramp epoch")
			}
			second := make(chan Release, 1)
			queueAcquire(t, s, ctx, key, hooks, second)
			synctest.Wait()
			if got := s.Stats().Queued; got != 1 {
				t.Fatalf("queued = %d, want the ordinary provider cap", got)
			}
			if timer, _ := wakeTimerState(&s.gate(provider).wake); timer != nil {
				t.Fatal("concurrency 1 armed a ramp timer")
			}
			first(0)
			rampGrant(t, second, "cap-1 grant")(0)
			synctest.Wait()
			if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
				t.Fatalf("cap-1 cleanup leaked occupancy: %+v", st)
			}
		})
	})

	t.Run("concurrency-0", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := New(Options{})
			s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
				Concurrency: 0, AntiBurst: true, RampDuration: ramp,
				Requests: 100, ReqWindow: time.Hour,
			}})
			ctx := context.Background()
			hooks := WaiterHooks{Provider: provider}
			releases := make([]Release, 0, 5)
			for i := 0; i < 5; i++ {
				release, err := s.AcquireWith(ctx, key, 0, hooks)
				if err != nil {
					t.Fatal(err)
				}
				releases = append(releases, release)
			}
			if st := s.Stats(); st.InFlight != 5 || st.Queued != 0 {
				t.Fatalf("concurrency-0 occupancy = %+v, want unlimited concurrency", st)
			}
			if start, _ := rampState(t, s, key); !start.IsZero() {
				t.Fatal("concurrency 0 started a ramp epoch")
			}
			if timer, _ := wakeTimerState(&s.gate(provider).wake); timer != nil {
				t.Fatal("concurrency 0 armed a ramp timer")
			}
			for _, release := range releases {
				release(0)
			}
			synctest.Wait()
		})
	})
}

// TestRampCancelDoesNoLeak pins cancellation while an epoch and its wake are
// live: cancelled waiters leave the queue, a granted waiter can release its
// slot, and the pending ramp wake fires harmlessly against an empty queue.
func TestRampCancelDoesNoLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			target = 4
			ramp   = 40 * time.Millisecond
		)
		s := New(Options{})
		provider := "ramp.example"
		key := provider + "|k"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
			Concurrency: target, AntiBurst: true, RampDuration: ramp,
		}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}
		first, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Fatal(err)
		}

		waitCtx, cancelWaits := context.WithCancel(ctx)
		var (
			wg        sync.WaitGroup
			cancelled atomic.Int32
			grants    = make(chan Release, target)
		)
		for i := 0; i < target; i++ {
			wg.Go(func() {
				release, err := s.AcquireWith(waitCtx, key, 0, hooks)
				if err == context.Canceled {
					cancelled.Add(1)
					return
				}
				if err != nil {
					t.Errorf("ramp waiter: %v", err)
					return
				}
				grants <- release
			})
		}
		synctest.Wait()
		if got := s.Stats().Queued; got != target {
			t.Fatalf("queued = %d, want %d", got, target)
		}

		time.Sleep(14 * time.Millisecond)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 2 || st.Queued != target-1 {
			t.Fatalf("first ramp grant occupancy = %+v, want 2 in flight", st)
		}
		granted := rampGrant(t, grants, "ramp grant")

		cancelWaits()
		synctest.Wait()
		wg.Wait()
		if cancelled.Load() != target-1 {
			t.Fatalf("cancelled = %d, want %d queued waiters", cancelled.Load(), target-1)
		}
		if st := s.Stats(); st.Queued != 0 || st.InFlight != 2 {
			t.Fatalf("post-cancel occupancy = %+v, want the holder and one ramp grant", st)
		}

		// The next step wake was armed when the ramp grant left a backlog.
		time.Sleep(15 * time.Millisecond)
		synctest.Wait()
		if timer, _ := wakeTimerState(&s.gate(provider).wake); timer != nil {
			t.Fatal("ramp wake survived after firing on an empty queue")
		}
		granted(0)
		first(0)
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("cancelled ramp leaked occupancy: %+v", st)
		}
		if got := providerQueuedGroups(s, provider); got != 0 {
			t.Fatalf("provider queued groups = %d, want 0", got)
		}
		if got := groupQueued(s, key); got != 0 {
			t.Fatalf("group queued = %d, want 0", got)
		}
	})
}

// TestRampFailedFirstAdmissionDoesNotStartClock pins that a provider bucket or
// cap rejection is not a grant: the group's epoch must stay unset until an
// actual admission succeeds. Key A exhausts the token bucket first so key B's
// very first request blocks before it ever obtains an epoch.
func TestRampFailedFirstAdmissionDoesNotStartClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ramp = 40 * time.Millisecond
		s := New(Options{})
		provider := "ramp.example"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
			Concurrency: 4, Tokens: 1, TokWindow: time.Hour,
			AntiBurst: true, RampDuration: ramp,
		}})
		ctx := context.Background()

		holder, err := s.AcquireWith(ctx, provider+"|a", 0, WaiterHooks{Provider: provider, EstTokens: 10})
		if err != nil {
			t.Fatal(err)
		}
		holder(10) // one oversize request puts the bucket into debt

		blockedCtx, cancelBlocked := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := s.AcquireWith(blockedCtx, provider+"|b", 0, WaiterHooks{Provider: provider, EstTokens: 1}); err != context.Canceled {
				t.Errorf("token-blocked first admission = %v", err)
			}
		})
		synctest.Wait()
		if got := s.Stats().Queued; got != 1 {
			t.Fatalf("queued = %d, want key B parked on the token bucket", got)
		}
		if start, idle := rampState(t, s, provider+"|b"); !start.IsZero() || !idle.IsZero() {
			t.Fatalf("failed first admission wrote epoch start=%v idle=%v", start, idle)
		}

		cancelBlocked()
		synctest.Wait()
		wg.Wait()
		gate := s.gate(provider)
		s.SetThrottle(Throttle{Provider: provider})
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("failed-admission cleanup leaked occupancy: %+v", st)
		}
		if timer, _ := wakeTimerState(&gate.wake); timer != nil {
			t.Fatal("cleared policy retained a wake timer")
		}
	})
}

// TestRampRepeatedArmsCoalesce pins the shared provider wake: every enqueue and
// every drain asks for the same next ramp step, and the earliest-deadline
// coalescing keeps one timer with one generation.
func TestRampRepeatedArmsCoalesce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			target = 4
			ramp   = time.Second
		)
		s := New(Options{})
		provider := "ramp.example"
		key := provider + "|k"
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{
			Concurrency: target, AntiBurst: true, RampDuration: ramp,
		}})
		ctx := context.Background()
		hooks := WaiterHooks{Provider: provider}
		first, err := s.AcquireWith(ctx, key, 0, hooks)
		if err != nil {
			t.Fatal(err)
		}

		waitCtx, cancelWaits := context.WithCancel(ctx)
		var wg sync.WaitGroup
		grants := make(chan Release, target)
		for i := 0; i < target; i++ {
			wg.Go(func() {
				release, err := s.AcquireWith(waitCtx, key, 0, hooks)
				if err == context.Canceled {
					return
				}
				if err != nil {
					t.Errorf("coalesce waiter: %v", err)
					return
				}
				grants <- release
			})
		}
		synctest.Wait()
		gate := s.gate(provider)
		timer0, gen0 := wakeTimerState(&gate.wake)
		if timer0 == nil {
			t.Fatal("ramp step wake not armed")
		}
		for i := 0; i < 100; i++ {
			s.drainProvider(provider)
		}
		synctest.Wait()
		timer1, gen1 := wakeTimerState(&gate.wake)
		if timer1 != timer0 || gen1 != gen0 {
			t.Fatalf("repeated ramp arms replaced the timer: timer same=%v generation %d -> %d", timer1 == timer0, gen0, gen1)
		}

		cancelWaits()
		synctest.Wait()
		wg.Wait()
		// Let the single coalesced step fire with no queued work.
		time.Sleep(ramp/3 + time.Millisecond)
		synctest.Wait()
		first(0)
		if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
			t.Fatalf("coalesced ramp leaked occupancy: %+v", st)
		}
	})
}
