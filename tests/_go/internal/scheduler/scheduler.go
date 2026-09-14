package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until it holds or a ~2s deadline elapses (t.Helper
// shape mirrors internal/proxy's waitForRecord): synchronizing on observable
// scheduler state (queue length, waiter counts, flags) is deterministic under
// parallel test load where a fixed sleep only hopes the goroutine got there.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// groupBackoffState reads a group's stored attempt-level and request-level
// backoff under the group lock. The scheduler exports no observer; production
// consumes these values through BackoffFor/FailSend/EndSend behavior.
func groupBackoffState(s *Scheduler, key string) (attempt, request time.Duration) {
	g := s.groupFor(key, 0)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backoff, g.requestBackoff
}

func TestFIFOOrdering(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	ctx := context.Background()

	// Acquire the first slot.
	release1, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}

	// Queue three waiters, one at a time so their enqueue order is deterministic.
	var mu sync.Mutex
	order := []int{}
	granted := make([]chan struct{}, 3)
	for i := range granted {
		granted[i] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			close(granted[i])
			release(0)
		}(i)
		// Wait until this waiter is enqueued so the FIFO order is deterministic.
		waitFor(t, func() bool { return s.Stats().Queued >= i+1 }, "waiter to enqueue")
	}

	// Release the first slot to start the chain.
	release1(0)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 granted, got %d", len(order))
	}
	for i := range order {
		if order[i] != i {
			t.Errorf("order[%d] = %d, want %d (FIFO violated)", i, order[i], i)
		}
	}
}

func TestConcurrencyCap(t *testing.T) {
	s := New(Options{MaxConcurrent: 2})
	ctx := context.Background()

	r1, _ := s.AcquireWith(ctx, "k", 2, WaiterHooks{})
	r2, _ := s.AcquireWith(ctx, "k", 2, WaiterHooks{})

	// Third should block until a release.
	done := make(chan struct{})
	go func() {
		r3, err := s.AcquireWith(ctx, "k", 2, WaiterHooks{})
		if err != nil {
			return
		}
		r3(0)
		close(done)
	}()

	select {
	case <-done:
		t.Error("third acquire should have blocked (cap=2)")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	r1(0) // free one
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("third acquire should have succeeded after release")
	}
	r2(0)
}

// TestConcurrentCapChangeNoRace hammers Acquire with a varying per-request
// concurrency cap (the proxy passes a client-controlled value) while other
// goroutines acquire/release, asserting the cap mutation is race-free. This is
// the regression test for the groupForLocked/acquire two-mutex data race
// (maxConcurrent was written under s.mu but read under g.mu). Run with -race.
func TestConcurrentCapChangeNoRace(t *testing.T) {
	s := New(Options{MaxConcurrent: 4})
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Goroutines that keep changing the group's cap via Acquire.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Vary the cap per call; Acquire applies it to the live group.
				rel, err := s.AcquireWith(ctx, "k", 1+(i%4), WaiterHooks{})
				if err == nil {
					rel(0)
				}
			}
		}(i)
	}
	// A goroutine that also pokes the group directly (groupFor path).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.extendWindow("k", 0, false)
			s.BackoffFor("k", 0)
		}
	}()

	// The hammer needs wall-clock time to run - the sleep IS the test's
	// duration, not synchronization on a condition.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestTripZeroKeepsWindow pins the fail-closed guard of the pacing window
// (Trip's extendWindow path): a non-positive duration must NOT erase an
// existing window (a past HTTP-date Retry-After yields a negative
// time.Until; that must never un-pause the group).
func TestTripZeroKeepsWindow(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()

	s.Trip("k", 300*time.Millisecond)
	// A zero/negative follow-up must not clobber the live window.
	s.Trip("k", 0)
	s.Trip("k", -5*time.Second)

	start := time.Now()
	rel, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("Acquire returned in %v; the pacing window was clobbered by a non-positive Trip", elapsed)
	}
}

func TestAcquireZeroRelaxesExistingCap(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	r1, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	g := s.groupFor("k", 0)
	g.mu.Lock()
	cap := g.maxConcurrent
	g.mu.Unlock()
	if cap != 1 {
		t.Fatalf("after AcquireWith(1) cap = %d, want 1", cap)
	}
	r1(0)
	r2, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2(0)
	g.mu.Lock()
	cap = g.maxConcurrent
	g.mu.Unlock()
	if cap != 0 {
		t.Fatalf("after AcquireWith(0) cap = %d, want 0 (unlimited)", cap)
	}
}

func TestRateLimitPacing(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()

	// Pace the group by 100ms (Trip's window; Acquire does not consult the
	// send token, only the pacing deadline).
	s.Trip("k", 100*time.Millisecond)

	start := time.Now()
	release, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	release(0)

	if elapsed < 90*time.Millisecond {
		t.Errorf("acquire returned too fast: %v; rate-limit pacing not honored", elapsed)
	}
}

func TestCancelInQueue(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	ctx, cancel := context.WithCancel(context.Background())

	r1, _ := s.AcquireWith(ctx, "k", 1, WaiterHooks{})

	// Start a waiter, then cancel its context.
	done := make(chan error, 1)
	go func() {
		_, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
		done <- err
	}()

	// Wait until the waiter is parked in the queue, then cancel it in place.
	waitFor(t, func() bool { return s.Stats().Queued >= 1 }, "waiter to queue")
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected context cancellation error")
		}
	case <-time.After(time.Second):
		t.Error("acquire should have returned on ctx cancel")
	}
	r1(0)
}

func TestMaxQueueSize(t *testing.T) {
	s := New(Options{MaxConcurrent: 1, MaxQueueSize: 2})
	ctx := context.Background()

	r1, _ := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	// queue 2
	go s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	go s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	// Both waiters must be parked before the queue-full attempt.
	waitFor(t, func() bool { return s.Stats().Queued >= 2 }, "two waiters to queue")

	// Fourth should fail (queue full).
	_, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	if err == nil {
		t.Error("expected queue full error")
	}
	r1(0)
}

func TestAdaptiveBackoff(t *testing.T) {
	s := New(Options{BaseBackoff: 100 * time.Millisecond, MaxBackoff: 1 * time.Second})

	// First rate limit with no hint → base backoff.
	d1 := s.BackoffFor("k", 0)
	if d1 < 75*time.Millisecond || d1 > 125*time.Millisecond {
		t.Errorf("first backoff = %v, want ~100ms", d1)
	}
	// Second → doubles (with jitter).
	d2 := s.BackoffFor("k", 0)
	if d2 < 150*time.Millisecond || d2 > 250*time.Millisecond {
		t.Errorf("second backoff = %v, want ~200ms", d2)
	}
	// Explicit long hint is a floor, unclamped by MaxBackoff. Adaptive has
	// grown to 400ms; 3s wins. Daily rate-limit Retry-After values are
	// routinely tens of minutes; clamping them to the adaptive cap (here 1s)
	// is the bug this pins.
	d3 := s.BackoffFor("k", 3*time.Second)
	if d3 != 3*time.Second {
		t.Errorf("explicit hint = %v, want 3s (unclamped by MaxBackoff)", d3)
	}
	if attempt, _ := groupBackoffState(s, "k"); attempt != 400*time.Millisecond {
		t.Errorf("stored adaptive after long hint = %v, want 400ms (hint must not reset doubling)", attempt)
	}
	// Short hint is a floor, not a reset: adaptive grows to 800ms, so the
	// wait is the jittered adaptive (~600-1000ms), not 500ms.
	d3b := s.BackoffFor("k", 500*time.Millisecond)
	if d3b < 600*time.Millisecond || d3b > 1000*time.Millisecond {
		t.Errorf("short hint = %v, want ~800ms adaptive (hint is a floor, not a reset)", d3b)
	}
	if attempt, _ := groupBackoffState(s, "k"); attempt != 800*time.Millisecond {
		t.Errorf("stored adaptive after short hint = %v, want 800ms (must keep growing)", attempt)
	}
	// After a success (EndSend owns the reset), backoff goes back to base.
	s.EndSend("k", false)
	d4 := s.BackoffFor("k", 0)
	if d4 < 75*time.Millisecond || d4 > 125*time.Millisecond {
		t.Errorf("post-reset backoff = %v, want ~100ms", d4)
	}
}

// TestBackoffForShortHintKeepsGrowing is the 503 Retry-After: 1s case: a
// short provider hint must not replace exponential backoff. Consecutive 50ms
// hints still double 100→200→400→800.
func TestBackoffForShortHintKeepsGrowing(t *testing.T) {
	s := New(Options{BaseBackoff: 100 * time.Millisecond, MaxBackoff: 2 * time.Second})
	const hint = 50 * time.Millisecond
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, w := range want {
		d := s.BackoffFor("k", hint)
		if d < w*3/4 || d > w*5/4 {
			t.Fatalf("attempt %d delay %v, want ~%v (max(hint, adaptive))", i+1, d, w)
		}
		if attempt, _ := groupBackoffState(s, "k"); attempt != w {
			t.Fatalf("attempt %d stored adaptive %v, want %v (short hint must not reset)", i+1, attempt, w)
		}
	}
}

// TestBackoffForHonorsLongRetryAfter is the regression for daily-limit 429s:
// a 35m Retry-After must be waited in full even when max_backoff is 2m.
// Previously BackoffFor clamped every hint to MaxBackoff, so the proxy
// retried after 2m and immediately 429'd again.
func TestBackoffForHonorsLongRetryAfter(t *testing.T) {
	s := New(Options{BaseBackoff: time.Second, MaxBackoff: 2 * time.Minute})
	const hint = 35*time.Minute + 14*time.Second
	if d := s.BackoffFor("k", hint); d != hint {
		t.Errorf("BackoffFor(35m14s) = %v, want %v (must not clamp to max_backoff=2m)", d, hint)
	}
}

// TestPauseLetsInFlightFinishAndQueuesNew is the operator-pause contract:
// a holder keeps its slot, release does not grant the next waiter while
// paused, and unpause drains FIFO.
func TestPauseLetsInFlightFinishAndQueuesNew(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	ctx := context.Background()

	rel, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	s.RestoreHolds([]Hold{{All: true}})
	if !s.Paused() {
		t.Fatal("Paused() = false after a global hold")
	}

	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()

	// The waiter must be parked (queued) while paused before the release, so
	// the release truly tests that a paused waiter is not granted.
	waitFor(t, func() bool { return s.Stats().Queued >= 1 }, "paused waiter to queue")
	rel(0) // in-flight finishes; must NOT grant the queued waiter
	select {
	case <-got:
		t.Fatal("queued acquire was granted while paused")
	case <-time.After(50 * time.Millisecond):
	}

	st := s.Stats()
	if !st.Paused || st.Queued != 1 || st.InFlight != 0 {
		t.Fatalf("stats while paused = %+v, want paused queued=1 in_flight=0", st)
	}

	s.RestoreHolds(nil)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("queued acquire should grant after unpause")
	}
}

func TestPauseBlocksEmptyGroup(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	s.RestoreHolds([]Hold{{All: true}})

	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()

	select {
	case <-got:
		t.Fatal("Acquire granted while paused on an empty group")
	case <-time.After(50 * time.Millisecond):
	}

	s.RestoreHolds(nil)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("Acquire should grant after unpause")
	}
}

// TestPauseDoesNotCountTowardMaxWait: a pause longer than MaxWait must not
// expire the waiter - "queue until we unpause".
func TestPauseDoesNotCountTowardMaxWait(t *testing.T) {
	s := New(Options{MaxWait: 40 * time.Millisecond})
	ctx := context.Background()
	s.RestoreHolds([]Hold{{All: true}})

	done := make(chan error, 1)
	go func() {
		rel, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{})
		if err == nil {
			rel(0)
		}
		done <- err
	}()

	// 2× MaxWait: the fixed window IS the negative proof - enough wall time
	// passes for a buggy MaxWait expiry to fire while paused.
	time.Sleep(80 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("acquire returned while paused: %v", err)
	default:
	}

	s.RestoreHolds(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acquire after unpause: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("acquire should grant after unpause (pause time must not count as MaxWait)")
	}
}

// TestRestoreHoldsIdempotent pins the policy-transition no-op: replaying the
// same hold set (or the empty set twice) must be a safe snapEqual short
// circuit, never a lost kick or a double epilogue.
func TestRestoreHoldsIdempotent(t *testing.T) {
	s := New(Options{})
	s.RestoreHolds([]Hold{{All: true}})
	s.RestoreHolds([]Hold{{All: true}})
	s.RestoreHolds(nil)
	s.RestoreHolds(nil)
	rel, err := s.AcquireWith(context.Background(), "k", 0, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

// TestPauseOneClientDoesNotBlockAnother: a held client stays queued while
// another client on the same key is granted (no head-of-line block).
func TestPauseOneClientDoesNotBlockAnother(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	ctx := context.Background()
	s.RestoreHolds([]Hold{{Clients: []string{"slow"}}})

	slowGot := make(chan struct{})
	go func() {
		rel, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "slow"})
		if err != nil {
			t.Error(err)
			return
		}
		close(slowGot)
		rel(0)
	}()
	// The slow client must be parked by the policy hold before the fast
	// client acquires, so the fast grant truly bypasses a parked waiter.
	waitFor(t, func() bool { return s.Stats().Queued >= 1 }, "held slow client to queue")

	rel, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-slowGot:
		t.Fatal("paused client was granted before an unpaused client")
	default:
	}
	rel(0)

	select {
	case <-slowGot:
		t.Fatal("paused client granted after the other client released")
	case <-time.After(40 * time.Millisecond):
	}

	s.RestoreHolds(nil)
	select {
	case <-slowGot:
	case <-time.After(time.Second):
		t.Fatal("paused client should grant after unpause")
	}
}

func TestPauseNewHoldsUnknownOnly(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	s.RestoreHolds([]Hold{{New: true, KnownAtNew: []string{"old"}}})

	rel, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "old"})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)

	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "stranger"})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()
	select {
	case <-got:
		t.Fatal("unseen client granted while New hold is on")
	case <-time.After(40 * time.Millisecond):
	}
	if !s.Holds("stranger") || s.Holds("old") {
		t.Fatalf("holds stranger=%v old=%v", s.Holds("stranger"), s.Holds("old"))
	}
	s.RestoreHolds(nil)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("unseen client should grant after unpause")
	}
}

func TestRestoredExpiredHoldDoesNotHold(t *testing.T) {
	s := New(Options{})
	s.RestoreHolds([]Hold{{All: true, Until: time.Now().Add(-time.Second)}})
	if s.Paused() || s.Holds("x") {
		t.Fatal("expired until must not hold")
	}
	rel, err := s.AcquireWith(context.Background(), "k", 0, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

// TestBackoffForCapsPathologicalHint pins the safety guardrail: a hostile
// Retry-After of years is clamped to MaxRetryHint so the group cannot stall
// forever. This is independent of MaxBackoff.
func TestBackoffForCapsPathologicalHint(t *testing.T) {
	s := New(Options{MaxBackoff: 2 * time.Minute})
	if d := s.BackoffFor("k", 99999999*time.Second); d != MaxRetryHint {
		t.Errorf("pathological hint = %v, want MaxRetryHint %v", d, MaxRetryHint)
	}
}
