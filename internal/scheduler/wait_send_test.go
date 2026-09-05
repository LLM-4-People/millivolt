package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitSendClosedIsNoop(t *testing.T) {
	s := New(Options{})
	start := time.Now()
	own, err := s.WaitSend(context.Background(), "k", WaiterHooks{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if own {
		t.Fatal("closed group must not hand out a retry token")
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("WaitSend on a closed group blocked for %v", time.Since(start))
	}
}

func TestTripClaimsOneOwner(t *testing.T) {
	s := New(Options{})
	if !s.Trip("k", 10*time.Millisecond) {
		t.Fatal("first Trip must claim the token")
	}
	if s.Trip("k", 10*time.Millisecond) {
		t.Fatal("second Trip must not steal the token")
	}
}

func TestWaitSendOwnerBlocksSiblingsUntilEndSend(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if !s.Trip("k", 20*time.Millisecond) {
		t.Fatal("expected to own")
	}

	sib := make(chan error, 1)
	go func() {
		_, err := s.WaitSend(ctx, "k", WaiterHooks{}, false, false)
		sib <- err
	}()

	select {
	case err := <-sib:
		t.Fatalf("sibling sent while owner is retrying: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	own, err := s.WaitSend(ctx, "k", WaiterHooks{}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !own {
		t.Fatal("owner WaitSend must keep the token")
	}

	select {
	case err := <-sib:
		t.Fatalf("sibling sent while owner is still in flight: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	s.EndSend("k", true)
	select {
	case err := <-sib:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sibling stuck after EndSend")
	}
}

func TestWaitSendHalfOpenOneThenUnblock(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if !s.Trip("k", time.Millisecond) {
		t.Fatal("expected to own")
	}

	var through atomic.Int32
	sib := func(done chan struct{}) {
		_, err := s.WaitSend(ctx, "k", WaiterHooks{}, false, false)
		if err != nil {
			t.Error(err)
		}
		through.Add(1)
		close(done)
	}
	b := make(chan struct{})
	c := make(chan struct{})
	d := make(chan struct{})
	go sib(b)
	go sib(c)
	go sib(d)
	// A sibling parked inside WaitSend has no observable state (it selects on
	// the group's kick channels); the fixed window lets all three reach the
	// parked select before the negative check below.
	time.Sleep(20 * time.Millisecond)
	if through.Load() != 0 {
		t.Fatal("siblings sent while owner is retrying")
	}

	s.EndSend("k", true) // retried → half-open, one probe
	// The probe passing IS the observable; exactly one goes through under the
	// half-open gate (the other two re-park until EndSend below).
	waitFor(t, func() bool { return through.Load() >= 1 }, "the half-open probe to send")
	if got := through.Load(); got != 1 {
		t.Fatalf("half-open let %d through, want 1", got)
	}

	s.EndSend("k", false) // probe succeeded first try → full concurrency
	select {
	case <-b:
	case <-time.After(time.Second):
		t.Fatal("sibling b stuck")
	}
	select {
	case <-c:
	case <-time.After(time.Second):
		t.Fatal("sibling c stuck")
	}
	select {
	case <-d:
	case <-time.After(time.Second):
		t.Fatal("sibling d stuck")
	}
	if got := through.Load(); got != 3 {
		t.Fatalf("after close through = %d, want 3", got)
	}
}

func TestWaitSendHonorHoldUntilUnpause(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if !s.Trip("k", 15*time.Millisecond) {
		t.Fatal("expected to own")
	}
	if err := s.AddHold(Hold{All: true}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.WaitSend(ctx, "k", WaiterHooks{Client: "c"}, true, true)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("owner retried while paused: %v", err)
	case <-time.After(40 * time.Millisecond):
	}

	holds := s.HoldsList()
	if len(holds) != 1 {
		t.Fatalf("holds = %d, want 1", len(holds))
	}
	s.RemoveHold(holds[0].ID)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner stuck after unpause")
	}
}

func TestWaitSendRetryAfterRemainingAcrossPause(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if !s.Trip("k", 80*time.Millisecond) {
		t.Fatal("expected to own")
	}
	// Wall-clock pacing: part of the Retry-After window elapses before the
	// pause, the rest opens during it - that timing IS the scenario.
	time.Sleep(20 * time.Millisecond)
	if err := s.AddHold(Hold{All: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // window opens during pause
	holds := s.HoldsList()
	s.RemoveHold(holds[0].ID)

	start := time.Now()
	if _, err := s.WaitSend(ctx, "k", WaiterHooks{Client: "c"}, true, true); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("after unpause waited %v; remaining nextAllowedAt should already be 0", elapsed)
	}
}

func TestWaitSendRetryAfterRemainingAfterShortPause(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if !s.Trip("k", 200*time.Millisecond) {
		t.Fatal("expected to own")
	}
	if err := s.AddHold(Hold{All: true}); err != nil {
		t.Fatal(err)
	}
	// Wall-clock pacing: the pause consumes 40ms of the 200ms window, so the
	// remaining ~160ms must still be waited after unpause.
	time.Sleep(40 * time.Millisecond)
	s.RemoveHold(s.HoldsList()[0].ID)
	start := time.Now()
	if _, err := s.WaitSend(ctx, "k", WaiterHooks{Client: "c"}, true, true); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("WaitSend after short pause returned in %v; leftover nextAllowedAt must still be waited", elapsed)
	}
}

func TestWaitSendFirstAttemptIgnoresHold(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{All: true}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	own, err := s.WaitSend(context.Background(), "k", WaiterHooks{Client: "c"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if own {
		t.Fatal("first-attempt WaitSend must not claim a token")
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("first-attempt send must finish despite pause")
	}
}

func TestWaitSendKeysIsolated(t *testing.T) {
	s := New(Options{})
	if !s.Trip("a|key1", time.Hour) {
		t.Fatal("expected to own")
	}
	start := time.Now()
	if _, err := s.WaitSend(context.Background(), "a|key2", WaiterHooks{}, false, false); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("cooldown on key1 must not pace key2")
	}
}

func TestSetRateLimitDoesNotShrinkWindow(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	s.SetRateLimit("k", 200*time.Millisecond)
	s.SetRateLimit("k", 20*time.Millisecond)
	start := time.Now()
	rel, err := s.Acquire(ctx, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("Acquire returned in %v; shorter SetRateLimit shrank the window", elapsed)
	}
}

func TestBackoffForDoesNotTripGroup(t *testing.T) {
	s := New(Options{BaseBackoff: 50 * time.Millisecond})
	s.BackoffFor("k", 0)
	start := time.Now()
	if _, err := s.WaitSend(context.Background(), "k", WaiterHooks{}, false, false); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 40*time.Millisecond {
		t.Fatal("BackoffFor alone must not trip WaitSend")
	}
}

func TestWaitSendCancelDoesNotReleaseOwner(t *testing.T) {
	s := New(Options{})
	if !s.Trip("k", time.Hour) {
		t.Fatal("expected to own")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitSend(ctx, "k", WaiterHooks{}, false, false)
		done <- err
	}()
	// A sibling parked inside WaitSend has no observable state (it selects on
	// the group's kick channels); the fixed window lets it reach the parked
	// select before the cancel, so cancel truly hits a parked sibling.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("sibling WaitSend did not return on cancel")
	}

	sib := make(chan struct{})
	go func() {
		_, _ = s.WaitSend(context.Background(), "k", WaiterHooks{}, false, false)
		close(sib)
	}()
	select {
	case <-sib:
		t.Fatal("cancelled sibling released the owner token")
	case <-time.After(40 * time.Millisecond):
	}
	s.EndSend("k", true)
	select {
	case <-sib:
	case <-time.After(time.Second):
		t.Fatal("sibling stuck after real EndSend")
	}
}

func TestFailSendDoublesRequestBackoff(t *testing.T) {
	s := New(Options{BaseBackoff: 80 * time.Millisecond, MaxBackoff: 400 * time.Millisecond})
	s.FailSend("k", false)
	if got := s.RequestBackoff("k"); got != 80*time.Millisecond {
		t.Fatalf("first fail backoff = %v, want 80ms", got)
	}
	s.FailSend("k", false)
	if got := s.RequestBackoff("k"); got != 160*time.Millisecond {
		t.Fatalf("second fail backoff = %v, want 160ms", got)
	}
	s.FailSend("k", false)
	if got := s.RequestBackoff("k"); got != 320*time.Millisecond {
		t.Fatalf("third fail backoff = %v, want 320ms", got)
	}
	s.FailSend("k", false)
	if got := s.RequestBackoff("k"); got != 400*time.Millisecond {
		t.Fatalf("capped fail backoff = %v, want 400ms", got)
	}
}

func TestFailSendPacesNextWaitSend(t *testing.T) {
	s := New(Options{BaseBackoff: 80 * time.Millisecond, MaxBackoff: 200 * time.Millisecond})
	s.FailSend("k", false)
	start := time.Now()
	if _, err := s.WaitSend(context.Background(), "k", WaiterHooks{}, false, false); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("WaitSend after FailSend returned in %v; next probe must wait request backoff", elapsed)
	}
}

func TestEndSendResetsRequestBackoff(t *testing.T) {
	s := New(Options{BaseBackoff: 80 * time.Millisecond, MaxBackoff: 200 * time.Millisecond})
	s.FailSend("k", false)
	s.FailSend("k", false)
	if s.RequestBackoff("k") == 0 {
		t.Fatal("expected leftover request backoff")
	}
	if !s.Trip("k", time.Millisecond) {
		t.Fatal("expected to own")
	}
	s.EndSend("k", true) // recovered after retries
	if got := s.RequestBackoff("k"); got != 0 {
		t.Fatalf("request backoff after success = %v, want 0", got)
	}
	start := time.Now()
	if _, err := s.WaitSend(context.Background(), "k", WaiterHooks{}, false, false); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("probe after recovered request waited %v, want immediate", elapsed)
	}
	s.FailSend("k", false)
	if got := s.RequestBackoff("k"); got != 80*time.Millisecond {
		t.Fatalf("fail after success = %v, want base 80ms not leftover 2^n", got)
	}
}

func TestFailSendDoesNotShrinkExistingWindow(t *testing.T) {
	s := New(Options{BaseBackoff: 20 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	s.SetRateLimit("k", 120*time.Millisecond)
	s.FailSend("k", false)
	start := time.Now()
	rel, err := s.Acquire(context.Background(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("Acquire in %v; FailSend shrank a longer Retry-After window", elapsed)
	}
}

func TestWaitSendOnHoldCallbacks(t *testing.T) {
	s := New(Options{})
	var held, unheld sync.WaitGroup
	held.Add(1)
	unheld.Add(1)
	var onceHeld, onceUnheld sync.Once
	if err := s.AddHold(Hold{All: true}); err != nil {
		t.Fatal(err)
	}
	if !s.Trip("k", time.Millisecond) {
		t.Fatal("expected to own")
	}
	go func() {
		_, _ = s.WaitSend(context.Background(), "k", WaiterHooks{
			Client:   "c",
			OnHold:   func() { onceHeld.Do(held.Done) },
			OnUnhold: func() { onceUnheld.Do(unheld.Done) },
		}, true, true)
	}()
	done := make(chan struct{})
	go func() { held.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnHold not fired")
	}
	s.RemoveHold(s.HoldsList()[0].ID)
	done2 := make(chan struct{})
	go func() { unheld.Wait(); close(done2) }()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("OnUnhold not fired")
	}
}

func TestFailSendOwnReleasesProbing(t *testing.T) {
	s := New(Options{BaseBackoff: 10 * time.Millisecond, MaxBackoff: 10 * time.Millisecond})
	if !s.Trip("k", time.Millisecond) {
		t.Fatal("expected to own")
	}
	g := s.groupFor("k", 0)
	g.mu.Lock()
	if !g.probing {
		g.mu.Unlock()
		t.Fatal("Trip must set probing")
	}
	g.mu.Unlock()

	sib := make(chan error, 1)
	go func() {
		_, err := s.WaitSend(context.Background(), "k", WaiterHooks{}, false, false)
		sib <- err
	}()
	// No observable exists for a sibling parked inside WaitSend (it selects on
	// the group's kick channels); the fixed window lets it reach the parked
	// select before the non-blocking negative check.
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-sib:
		t.Fatalf("sibling sent while owner still probing: %v", err)
	default:
	}

	s.FailSend("k", true)
	g.mu.Lock()
	if g.probing {
		g.mu.Unlock()
		t.Fatal("FailSend(true) must release probing")
	}
	g.mu.Unlock()

	select {
	case err := <-sib:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sibling stuck after FailSend(true)")
	}
}

func TestFailSendSiblingKeepsOwnerAttemptBackoff(t *testing.T) {
	s := New(Options{BaseBackoff: 80 * time.Millisecond, MaxBackoff: time.Second})
	if d := s.BackoffFor("k", 0); d <= 0 {
		t.Fatal("expected attempt backoff")
	}
	want := s.Backoff("k")
	if want != 80*time.Millisecond {
		t.Fatalf("attempt backoff = %v, want 80ms", want)
	}
	if !s.Trip("k", time.Millisecond) {
		t.Fatal("expected to own")
	}
	s.FailSend("k", false)
	if got := s.Backoff("k"); got != want {
		t.Fatalf("sibling FailSend wiped owner attempt backoff: %v, want %v", got, want)
	}
	g := s.groupFor("k", 0)
	g.mu.Lock()
	probing := g.probing
	g.mu.Unlock()
	if !probing {
		t.Fatal("sibling FailSend must not release owner probing")
	}
}
