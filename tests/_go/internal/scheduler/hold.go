package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHoldsOverlapCases(t *testing.T) {
	old := Hold{New: true, KnownAtNew: []string{"client-a"}}
	named := Hold{Clients: []string{"client-a"}}
	other := Hold{Clients: []string{"client-b"}}
	prov := Hold{Providers: []string{"alpha.example"}}
	pair := Hold{Clients: []string{"client-a"}, Providers: []string{"alpha.example"}}
	pair2 := Hold{Clients: []string{"client-a"}, Providers: []string{"beta.example"}}
	all := Hold{All: true}

	must := func(a, b Hold, want bool) {
		t.Helper()
		if got := HoldsOverlap(a, b); got != want {
			t.Fatalf("overlap(%+v, %+v) = %v, want %v", a, b, got, want)
		}
	}
	must(named, other, false)
	must(named, prov, true) // client-a × alpha.example
	must(named, all, true)
	must(old, named, false) // client-a is known, New skips it
	must(old, other, true)  // client-b is unseen
	must(old, prov, true)   // unseen × alpha.example
	must(pair, pair2, false)
	must(pair, Hold{Clients: []string{"client-b"}, Providers: []string{"alpha.example"}}, false)
	must(Hold{New: true}, Hold{New: true, KnownAtNew: []string{"x"}}, true)
}

func TestAddHoldRejectsOverlap(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{Clients: []string{"client-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{Providers: []string{"alpha.example"}}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("err = %v, want ErrOverlap", err)
	}
	if err := s.AddHold(Hold{Clients: []string{"client-b"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.HoldsList(); len(got) != 2 {
		t.Fatalf("holds = %d, want 2", len(got))
	}
}

func TestPauseByProviderDoesNotBlockOtherProvider(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if err := s.AddHold(Hold{Providers: []string{"alpha.example"}}); err != nil {
		t.Fatal(err)
	}
	if s.Holds("client-a") {
		t.Fatal("provider hold must not match with unknown provider")
	}
	if !s.HoldsTarget("client-a", "alpha.example") || s.HoldsTarget("client-a", "beta.example") {
		t.Fatal("provider hold mismatch")
	}

	rel, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "client-a", Provider: "beta.example"})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)

	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "client-a", Provider: "alpha.example"})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()
	select {
	case <-got:
		t.Fatal("alpha.example request granted while provider held")
	case <-time.After(40 * time.Millisecond):
	}
	s.RemoveHold(s.HoldsList()[0].ID)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("should grant after provider hold removed")
	}
}

func TestRemoveOneHoldLeavesTheOther(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if err := s.AddHold(Hold{Clients: []string{"a"}, Until: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{Clients: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	holds := s.HoldsList()
	if len(holds) != 2 {
		t.Fatalf("holds = %d", len(holds))
	}
	var idA string
	for _, h := range holds {
		for _, c := range h.Clients {
			if c == "a" {
				idA = h.ID
			}
		}
	}
	s.RemoveHold(idA)
	if s.Holds("a") || !s.Holds("b") {
		t.Fatalf("after remove A: holds a=%v b=%v", s.Holds("a"), s.Holds("b"))
	}
	rel, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "a"})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

func TestPauseQueueCapRefuses(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if err := s.AddHold(Hold{Clients: []string{"c"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	queued := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{
			Client: "c",
			OnHold: func() { close(queued) },
		})
		if err != nil {
			t.Error(err)
			return
		}
		r(0)
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("first waiter never held")
	}
	_, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "c"})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second acquire err = %v, want ErrQueueFull", err)
	}
	// A different client is not capped by this hold.
	rel, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "other"})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
	s.SetPaused(false)
}

func TestSetHoldsRejectsOverlappingList(t *testing.T) {
	s := New(Options{})
	err := s.SetHolds([]Hold{
		{Clients: []string{"a"}},
		{Providers: []string{"p"}},
	})
	if !errors.Is(err, ErrOverlap) {
		t.Fatalf("err = %v, want ErrOverlap", err)
	}
	if s.Paused() {
		t.Fatal("rejected SetHolds must not apply")
	}
}

func TestUntilWakesHeldWaiter(t *testing.T) {
	s := New(Options{})
	held := make(chan struct{})
	got := make(chan struct{})
	if err := s.AddHold(Hold{Clients: []string{"c"}, Until: time.Now().Add(40 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	go func() {
		r, err := s.AcquireWith(context.Background(), "k", 0, WaiterHooks{
			Client: "c",
			OnHold: func() { close(held) },
		})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("never held")
	}
	select {
	case <-got:
		t.Fatal("granted before Until")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("waiter stuck after Until (scheduler must wake without RemoveHold)")
	}
}

func TestConcurrentAddHoldKeepsBoth(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := New(Options{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.AddHold(Hold{Clients: []string{"a"}})
		}()
		go func() {
			defer wg.Done()
			_ = s.AddHold(Hold{Clients: []string{"b"}})
		}()
		wg.Wait()
		if n := len(s.HoldsList()); n != 2 {
			t.Fatalf("iter %d: holds = %d, want 2", i, n)
		}
		if !s.Holds("a") || !s.Holds("b") {
			t.Fatalf("iter %d: missing client hold", i)
		}
	}
}

func TestConcurrentRemoveHoldClearsBoth(t *testing.T) {
	for i := 0; i < 100; i++ {
		s := New(Options{})
		if err := s.AddHold(Hold{ID: "ha", Clients: []string{"a"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.AddHold(Hold{ID: "hb", Clients: []string{"b"}}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.RemoveHold("ha") }()
		go func() { defer wg.Done(); s.RemoveHold("hb") }()
		wg.Wait()
		if n := len(s.HoldsList()); n != 0 {
			t.Fatalf("iter %d: holds = %d, want 0", i, n)
		}
	}
}

func TestAlreadyQueuedExceedsCap(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var parked atomic.Int32
	for i := 0; i < 3; i++ {
		go func() {
			r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{
				Client: "c",
				OnHold: func() { parked.Add(1) },
			})
			if err != nil {
				t.Error(err)
				return
			}
			r(0)
		}()
	}
	deadline := time.Now().Add(time.Second)
	for s.Stats().Queued < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Stats().Queued != 3 {
		t.Fatalf("queued = %d, want 3", s.Stats().Queued)
	}
	if err := s.AddHold(Hold{Clients: []string{"c"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for parked.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if parked.Load() != 3 {
		t.Fatalf("already-queued held = %d, want 3", parked.Load())
	}
	_, err = s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("new acquire err = %v, want ErrQueueFull", err)
	}
	s.SetPaused(false)
	rel(0)
}

func TestMaxQueuedZeroUnlimited(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{Clients: []string{"c"}, MaxQueued: 0}); err != nil {
		t.Fatal(err)
	}
	var parked atomic.Int32
	for i := 0; i < 3; i++ {
		go func() {
			r, err := s.AcquireWith(context.Background(), "k", 0, WaiterHooks{
				Client: "c",
				OnHold: func() { parked.Add(1) },
			})
			if err != nil {
				t.Error(err)
				return
			}
			r(0)
		}()
	}
	deadline := time.Now().Add(time.Second)
	for parked.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if parked.Load() != 3 {
		t.Fatalf("unlimited cap parked = %d, want 3", parked.Load())
	}
	s.SetPaused(false)
}

func TestPairHoldANDMatch(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if err := s.AddHold(Hold{Clients: []string{"client-a"}, Providers: []string{"alpha.example"}}); err != nil {
		t.Fatal(err)
	}
	rel, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "client-a", Provider: "beta.example"})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
	rel, err = s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "client-b", Provider: "alpha.example"})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 0, WaiterHooks{Client: "client-a", Provider: "alpha.example"})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()
	select {
	case <-got:
		t.Fatal("pair hold granted the matching request")
	case <-time.After(40 * time.Millisecond):
	}
	s.SetPaused(false)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("should grant after resume")
	}
}

func TestTwoProviderHoldsAllowed(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{Providers: []string{"alpha.example"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{Providers: []string{"beta.example"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(s.HoldsList()); n != 2 {
		t.Fatalf("holds = %d, want 2", n)
	}
}

func TestRestoreHoldsNewIDsAreRemovable(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{ID: "old", Clients: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	s.RestoreHolds([]Hold{{ID: "new", Clients: []string{"c"}}})
	s.RemoveHold("new")
	if s.Holds("c") {
		t.Fatal("RestoreHolds kept the old id; RemoveHold(new) was a no-op")
	}
}

func TestHeldCancelDoesNotUnderCountInFlight(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "a"})
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	wctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	if err := s.AddHold(Hold{Clients: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := s.AcquireWith(wctx, "k", 1, WaiterHooks{
			Client: "b",
			OnHold: func() { close(held) },
		})
		errc <- err
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("never held")
	}
	cancel()
	if err := s.AddHold(Hold{Clients: []string{"d"}}); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()
	select {
	case <-got:
		t.Fatal("over-admitted while A still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("held cancel err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not return")
	}
	rel(0)
	s.SetPaused(false)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("should grant after A released")
	}
}

func TestReplaceHoldKeepsIDAndWaiters(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{ID: "h1", Clients: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	got := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(context.Background(), "k", 0, WaiterHooks{
			Client: "c",
			OnHold: func() { close(held) },
		})
		if err != nil {
			t.Error(err)
			return
		}
		close(got)
		r(0)
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("never held")
	}
	if err := s.ReplaceHold(Hold{ID: "h1", Clients: []string{"c"}, MaxQueued: 4}); err != nil {
		t.Fatal(err)
	}
	if n := len(s.HoldsList()); n != 1 || s.HoldsList()[0].ID != "h1" {
		t.Fatalf("holds after replace = %+v", s.HoldsList())
	}
	select {
	case <-got:
		t.Fatal("replace must not release a still-matching waiter")
	case <-time.After(40 * time.Millisecond):
	}
	if err := s.ReplaceHold(Hold{ID: "h1", Clients: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("waiter should grant after scope no longer matches")
	}
}

func TestReplaceHoldRejectsOverlapAndMissing(t *testing.T) {
	s := New(Options{})
	if err := s.AddHold(Hold{ID: "ha", Clients: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{ID: "hb", Clients: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceHold(Hold{ID: "ha", Clients: []string{"b"}}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("err = %v, want ErrOverlap", err)
	}
	if err := s.ReplaceHold(Hold{ID: "nope", Clients: []string{"c"}}); !errors.Is(err, ErrHoldNotFound) {
		t.Fatalf("err = %v, want ErrHoldNotFound", err)
	}
	if !s.Holds("a") || !s.Holds("b") {
		t.Fatal("failed replace must leave both holds")
	}
}

func TestAddHoldWakesCapWaiter(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{
			Client: "c",
			OnHold: func() { close(held) },
		})
		if err != nil {
			t.Error(err)
			return
		}
		r(0)
	}()
	deadline := time.Now().Add(time.Second)
	for s.Stats().Queued < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Stats().Queued != 1 {
		t.Fatalf("queued = %d, want 1", s.Stats().Queued)
	}
	time.Sleep(20 * time.Millisecond) // enter the non-hold select
	if err := s.AddHold(Hold{ID: "h1", Clients: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("OnHold never ran - AddHold lost wakeup on cap waiter")
	}
	s.RemoveHold("h1")
	rel(0)
}

// TestReplaceHoldWakesCapWaiter is the edit-then-update path: a waiter is
// already queued for capacity (not the hold), then ReplaceHold widens the
// existing pause so it newly matches. drain skips them (blocked) - OnHold
// must still run so the dashboard does not keep them as "streaming".
func TestReplaceHoldWakesCapWaiter(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{ID: "h1", Providers: []string{"other.land"}}); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{
			Client: "c", Provider: "gamma.example",
			OnHold: func() { close(held) },
		})
		if err != nil {
			t.Error(err)
			return
		}
		r(0)
	}()
	deadline := time.Now().Add(time.Second)
	for s.Stats().Queued < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Stats().Queued != 1 {
		t.Fatalf("queued = %d, want 1", s.Stats().Queued)
	}
	time.Sleep(20 * time.Millisecond) // enter the non-hold select
	if err := s.ReplaceHold(Hold{ID: "h1", Providers: []string{"gamma.example"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("OnHold never ran - ReplaceHold lost wakeup on cap waiter")
	}
	if s.HoldsTarget("c", "beta.example") {
		t.Fatal("replace widened hold past the named provider")
	}
	s.RemoveHold("h1")
	rel(0)
}

func TestReplaceHoldOnHoldDuringOnWait(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{ID: "h1", Providers: []string{"other.land"}}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	held := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{
			Client: "c", Provider: "gamma.example",
			OnWait: func() { close(entered); <-unblock },
			OnHold: func() { close(held) },
		})
		if err != nil {
			t.Error(err)
			return
		}
		r(0)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("OnWait never entered")
	}
	if err := s.ReplaceHold(Hold{ID: "h1", Providers: []string{"gamma.example"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("OnHold should fire from seed while OnWait is still blocked")
	}
	close(unblock)
	s.RemoveHold("h1")
	rel(0)
}

func TestAddHoldSeedsMaxQueuedBeforeAdopt(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
		if err != nil {
			return
		}
		r(0)
	}()
	deadline := time.Now().Add(time.Second)
	for s.Stats().Queued < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.AddHold(Hold{Clients: []string{"c"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	_, err = s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("new acquire err = %v, want ErrQueueFull (count seeded at AddHold)", err)
	}
	holds := s.HoldsList()
	if len(holds) != 1 {
		t.Fatalf("holds = %d, want 1", len(holds))
	}
	if got := s.HoldQueued(holds[0].ID); got != 1 {
		t.Fatalf("HoldQueued = %d, want 1 (seed×adopt must not double-count)", got)
	}
	s.SetPaused(false)
	rel(0)
}

// TestAdoptHoldExactlyOnceConcurrent is the regression for the seed ×
// wait-loop double-count: AddHold's seedQueuedHolds adopts every matching
// queued waiter while the same waiters adopt from their own wait loops
// (AddHold's kick wakes them), and adoption used to race on waiter.holding
// outside any lock - OnHold fired twice for one waiter ("already-queued
// held = 4, want 3") and the race detector flagged hold.go. Adoption must
// be ONE atomic transition under holdCountMu: N concurrent adoptHold calls
// attribute the waiter once and fire OnHold exactly once; N concurrent
// dropHold calls fire OnUnhold exactly once.
func TestAdoptHoldExactlyOnceConcurrent(t *testing.T) {
	s := New(Options{})
	h := snapHold(Hold{ID: "h1", Clients: []string{"c"}})
	w := &waiter{client: "c", provider: "p.example"}
	var fired atomic.Int32
	w.onHold = func() { fired.Add(1) }
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			w.adoptHold(s, h)
		}()
	}
	wg.Wait()
	if got := fired.Load(); got != 1 {
		t.Fatalf("OnHold fired %d times for one waiter, want exactly 1", got)
	}
	if got := s.HoldQueued("h1"); got != 1 {
		t.Fatalf("HoldQueued = %d, want 1 (adoption must not double-count)", got)
	}
	var unfired atomic.Int32
	w.onUnhold = func() { unfired.Add(1) }
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			w.dropHold(s)
		}()
	}
	wg.Wait()
	if got := unfired.Load(); got != 1 {
		t.Fatalf("OnUnhold fired %d times for one waiter, want exactly 1", got)
	}
	if got := s.HoldQueued("h1"); got != 0 {
		t.Fatalf("HoldQueued after drop = %d, want 0", got)
	}
}

func TestHoldQueuedEqualsWaitersAfterSeedAndAdopt(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
			if err != nil {
				return
			}
			r(0)
		}()
	}
	deadline := time.Now().Add(time.Second)
	for s.Stats().Queued < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Stats().Queued != n {
		t.Fatalf("queued = %d, want %d", s.Stats().Queued, n)
	}
	if err := s.AddHold(Hold{ID: "h1", Clients: []string{"c"}, MaxQueued: n}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for s.HoldQueued("h1") != n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.HoldQueued("h1"); got != n {
		t.Fatalf("HoldQueued = %d, want %d (seed×adopt must match waiter count)", got, n)
	}
	_, err = s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("new acquire err = %v, want ErrQueueFull", err)
	}
	s.RemoveHold("h1")
	rel(0)
	wg.Wait()
}

func TestAddHoldSeedsWhenOnWaitBlocked(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	got := make(chan error, 1)
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{
			Client: "c",
			OnWait: func() {
				close(entered)
				<-unblock
			},
		})
		if err != nil {
			got <- err
			return
		}
		r(0)
		got <- nil
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("OnWait never entered")
	}
	if err := s.AddHold(Hold{ID: "h1", Clients: []string{"c"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.HoldQueued("h1"); got != 1 {
		t.Fatalf("HoldQueued while OnWait blocked = %d, want 1 (seed, not adopt)", got)
	}
	_, err = s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "c"})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull from seed", err)
	}
	close(unblock)
	s.RemoveHold("h1")
	rel(0)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued waiter stuck after unpause")
	}
}

func TestReplaceHoldDoesNotLeakQueuedCount(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{
			Client: "c",
			OnWait: func() { close(entered) },
		})
		if err != nil {
			return
		}
		r(0)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("waiter never queued")
	}
	deadline := time.Now().Add(time.Second)
	for s.Stats().Queued < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := s.AddHold(Hold{ID: "h1", Clients: []string{"c"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.HoldQueued("h1"); got != 1 {
		t.Fatalf("HoldQueued after AddHold = %d, want 1", got)
	}
	if err := s.ReplaceHold(Hold{ID: "h1", Clients: []string{"d"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.HoldQueued("h1"); got != 0 {
		t.Fatalf("HoldQueued after scope change = %d, want 0 (seed must uncount unmatched)", got)
	}
	got := make(chan error, 1)
	go func() {
		r, err := s.AcquireWith(ctx, "k", 1, WaiterHooks{Client: "d"})
		if err != nil {
			got <- err
			return
		}
		r(0)
		got <- nil
	}()
	select {
	case err := <-got:
		if errors.Is(err, ErrQueueFull) {
			t.Fatal("client d refused by leaked count from client c")
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(40 * time.Millisecond):
		// d is parked by the new hold (MaxQueued 1) - that is success.
	}
	if got := s.HoldQueued("h1"); got != 1 {
		t.Fatalf("HoldQueued for d = %d, want 1", got)
	}
	s.RemoveHold("h1")
	rel(0)
}

// TestSeedNeverAdoptsDepartedWaiter is the regression for the departed-waiter
// hole in seedQueuedHolds: the scan used to stage (waiter, hold) pairs and
// run adoptHold after the locks dropped, so a waiter that departed in the
// staging→adopt window (client cancel, or a grant once the hold expired) was
// then adopted post-mortem - holdCount was re-added with nobody left to
// decrement it (a permanent leak that burns the hold's MaxQueued cap, 429ing
// every new matching Acquire, and overcounting GET /admin/pause "queued"), and
// OnHold fired for a request whose Acquire had already returned (a paused live
// row that never unpauses).
//
// Determinism: waiter 1 is blocked inside OnWait, so it cannot self-adopt and
// its OnHold - the seam - fires only from seed's own dispatch phase. The seam
// SIGNALS both departures (ctx cancels + unblock) without joining them: with
// dispatch serialized under holdCountMu, a hook that waited for a departure
// would deadlock on its own critical section (the departing waiter's dropHold
// needs the very lock the hook holds), so joins happen after AddHold returns
// - bounded. Both waiters share one group, so FIFO queue order makes the scan
// and the dispatch order deterministic: waiter 1 is always first. The
// departures land wherever the lock hands them: after seed's dispatch w2
// either never sees OnHold (its dropHold won holdCountMu first) or sees
// [hold, unhold] (dispatch won first) - never [unhold, hold], and never an
// OnHold after its Acquire returned.
func TestSeedNeverAdoptsDepartedWaiter(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}

	w1Ctx, cancelW1 := context.WithCancel(ctx)
	w2Ctx, cancelW2 := context.WithCancel(ctx)
	w1Entered := make(chan struct{})
	unblockW1 := make(chan struct{})
	w1Done := make(chan error, 1)
	w2Done := make(chan error, 1)
	seamFired := make(chan struct{})

	var w2Mu sync.Mutex
	var w2Seq []string
	var w2Departed atomic.Bool
	var w1Fires, w1Unholds atomic.Int32
	var seamOnce sync.Once

	// departure joins a waiter's Acquire return on the main test goroutine
	// AFTER AddHold has returned (never from inside the seam hook - the
	// departure's dropHold serializes on holdCountMu behind the hook itself).
	departure := func(done chan error, who string) {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s acquire err = %v, want context.Canceled", who, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s departure did not complete", who)
		}
	}

	// w1: parked inside OnWait (after enqueue) - it can never self-adopt, so
	// its OnHold fires only from seed's dispatch phase. That OnHold is the
	// seam that signals both departures mid-seed.
	go func() {
		r, err := s.AcquireWith(w1Ctx, "k", 1, WaiterHooks{
			Client: "c",
			OnWait: func() { close(w1Entered); <-unblockW1 },
			OnHold: func() {
				w1Fires.Add(1)
				seamOnce.Do(func() {
					// Signal both departures mid-seed - after the scan
					// staged them, while/before seed's dispatch fires.
					// Signal ONLY (cancels + close): joining here would
					// deadlock, since each departure's dropHold must
					// acquire the holdCountMu this hook now runs under.
					cancelW2()
					cancelW1()
					close(unblockW1)
					close(seamFired)
				})
			},
			OnUnhold: func() { w1Unholds.Add(1) },
		})
		if err == nil {
			r(0)
		}
		w1Done <- err
	}()
	<-w1Entered

	// w2: parks in the normal (non-hold) select; cancelW2 is its departure.
	go func() {
		_, err := s.AcquireWith(w2Ctx, "k", 1, WaiterHooks{
			Client: "c",
			OnHold: func() {
				w2Mu.Lock()
				w2Seq = append(w2Seq, "hold")
				w2Mu.Unlock()
				if w2Departed.Load() {
					t.Error("seed adopted a departed waiter: OnHold fired after its Acquire returned")
				}
			},
			OnUnhold: func() {
				w2Mu.Lock()
				w2Seq = append(w2Seq, "unhold")
				w2Mu.Unlock()
			},
		})
		w2Done <- err
	}()
	waitFor(t, func() bool { return s.Stats().Queued >= 2 }, "both waiters to queue")

	// AddHold's dispatch phase runs the seam; the departures complete as the
	// lock hands it over, so the joins happen after AddHold returns.
	if err := s.AddHold(Hold{Clients: []string{"c"}, MaxQueued: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seamFired:
	case <-time.After(2 * time.Second):
		t.Fatal("seam never fired - seed did not dispatch w1's OnHold")
	}
	departure(w2Done, "w2")
	departure(w1Done, "w1")
	w2Departed.Store(true)
	holds := s.HoldsList()
	if len(holds) != 1 {
		t.Fatalf("holds = %d, want 1", len(holds))
	}
	if got := s.HoldQueued(holds[0].ID); got != 0 {
		t.Fatalf("HoldQueued = %d, want 0 - seed adopted a departed waiter and leaked its hold count", got)
	}
	if got := w1Fires.Load(); got != 1 {
		t.Fatalf("w1 OnHold fired %d times, want 1 (seed's callback, exactly once)", got)
	}
	if got := w1Unholds.Load(); got != 1 {
		t.Fatalf("w1 OnUnhold fired %d times, want 1", got)
	}
	w2Mu.Lock()
	last := ""
	if n := len(w2Seq); n > 0 {
		last = w2Seq[n-1]
	}
	seq := append([]string(nil), w2Seq...)
	w2Mu.Unlock()
	if last != "unhold" {
		t.Fatalf("w2 hold callbacks = %v, want the last callback to be unhold (a post-mortem OnHold strands the live row paused)", seq)
	}

	// The leaked count burned the hold's MaxQueued cap: a fresh matching
	// Acquire must park on the hold, not be refused with ErrQueueFull.
	w3Ctx, cancelW3 := context.WithCancel(ctx)
	w3Done := make(chan error, 1)
	w3Held := make(chan struct{})
	go func() {
		_, err := s.AcquireWith(w3Ctx, "k", 1, WaiterHooks{
			Client: "c",
			OnHold: func() { close(w3Held) },
		})
		w3Done <- err
	}()
	waitFor(t, func() bool {
		select {
		case <-w3Held:
			return true
		default:
			return false
		}
	}, "fresh matching Acquire to be admitted (cap not burned by a leaked count)")
	cancelW3()
	select {
	case err := <-w3Done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("w3 acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("w3 did not return after cancel")
	}
	if got := s.HoldQueued(holds[0].ID); got != 0 {
		t.Fatalf("HoldQueued after w3 departure = %d, want 0", got)
	}
	rel(0)
}

// TestSeededDispatchNeverInvertsAgainstDeparture pins the ORDER invariant of
// hold-callback dispatch: OnHold never fires after OnUnhold for one waiter.
// seedQueuedHolds' scan attributes a queued waiter (holding=true) and defers
// its OnHold to fireSeededHold; a waiter that departs between the scan and the
// dispatch (client cancel, or a grant once the hold expired) runs dropHold
// concurrently with that dispatch. The pre-fix shape re-validated liveness
// under holdCountMu, RELEASED the lock, and only then invoked the hook - in
// that gap the departing waiter's dropHold completed and fired OnUnhold first,
// so the stale OnHold landed after it: the proxy's OnHold sets rec.Paused=true
// and publishes a live update, so a request that already ended got re-paused
// after its own unhold (a stranded paused publish; the count itself cannot
// leak - attribution happens under g.mu in the scan and every departure
// decrements).
//
// Dispatch now invokes the hook while STILL holding holdCountMu, so a
// concurrent dropHold cannot complete inside the validate→invoke window: it
// blocks on the same lock until the hook returns, making OnUnhold strictly
// ordered after any OnHold it races. Hooks must therefore be non-blocking and
// never re-enter the scheduler (audited: the proxy's publishUpdate/pacer
// hooks satisfy this; every scheduler test hook is a close/atomic/append).
//
// Determinism: this drives fireSeededHold's seam directly (package-internal).
// The hook sleeps briefly - under the lock the sleep merely extends the
// critical section, but pre-fix it widened the released validate→invoke gap so
// the racing dropHold reliably wins it ([unhold, hold], the inversion this
// test forbids). Post-fix the only legal sequences are [unhold] (the drop won
// the lock first: dispatch sees holding=false and does not invoke) or
// [hold, unhold] (dispatch won, then the drop completes). The loop ×N under
// -race makes the pre-fix inversion reproduce reliably.
func TestSeededDispatchNeverInvertsAgainstDeparture(t *testing.T) {
	const iters = 50
	for i := 0; i < iters; i++ {
		s := New(Options{})
		h := snapHold(Hold{ID: "h1", Clients: []string{"c"}})
		var seqMu sync.Mutex
		var seq []string
		w := &waiter{client: "c", provider: "p.example"}
		w.onHold = func() {
			time.Sleep(time.Millisecond)
			seqMu.Lock()
			seq = append(seq, "hold")
			seqMu.Unlock()
		}
		w.onUnhold = func() {
			seqMu.Lock()
			seq = append(seq, "unhold")
			seqMu.Unlock()
		}
		// Stage exactly as seedQueuedHolds' scan does (under g.mu +
		// holdCountMu): attributed, holding=true, OnHold deferred.
		s.holdCountMu.Lock()
		s.moveHoldCountLocked(w, h)
		w.holding = true
		s.holdCountMu.Unlock()
		// The departure races the deferred dispatch end to end - before,
		// during, or after the validate→invoke window.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.dropHold(s)
		}()
		s.fireSeededHold(w, h)
		wg.Wait()
		seqMu.Lock()
		got := append([]string(nil), seq...)
		seqMu.Unlock()
		switch {
		case len(got) == 1 && got[0] == "unhold":
		case len(got) == 2 && got[0] == "hold" && got[1] == "unhold":
		default:
			t.Fatalf("iter %d: callbacks = %v, want [unhold] or [hold, unhold] - OnHold after OnUnhold is a stale paused publish for a departed waiter", i, got)
		}
		if n := s.HoldQueued("h1"); n != 0 {
			t.Fatalf("iter %d: HoldQueued = %d, want 0", i, n)
		}
	}
}

// TestSeedDepartureRaceStressNoLeak is the bounded-stress companion to
// TestSeedNeverAdoptsDepartedWaiter. The deterministic test drives one exact
// interleaving (the departure completes inside seed's callback phase); this
// races departures across the whole AddHold window (before, during, or after
// the scan, with several groups so the scan overlaps the departures) N
// times and asserts the same invariants whichever way the goroutines land:
// the hold count drains to zero and the MaxQueued cap is not burned. Every
// assertion is a settled-state check (joins plus polls), so the test is
// flake-free.
func TestSeedDepartureRaceStressNoLeak(t *testing.T) {
	const iters = 100
	const groups = 4
	for i := 0; i < iters; i++ {
		s := New(Options{})
		ctx := context.Background()
		var rels []Release
		var cancels []context.CancelFunc
		var wDones []chan error
		for g := 0; g < groups; g++ {
			rel, err := s.Acquire(ctx, fmt.Sprintf("k%d", g), 1)
			if err != nil {
				t.Fatal(err)
			}
			rels = append(rels, rel)
			wctx, cancel := context.WithCancel(ctx)
			cancels = append(cancels, cancel)
			done := make(chan error, 1)
			wDones = append(wDones, done)
			key := fmt.Sprintf("k%d", g)
			go func() {
				_, err := s.AcquireWith(wctx, key, 1, WaiterHooks{Client: "c"})
				done <- err
			}()
		}
		waitFor(t, func() bool { return s.Stats().Queued >= groups }, "waiters to queue")

		addDone := make(chan struct{})
		go func() {
			_ = s.AddHold(Hold{Clients: []string{"c"}, MaxQueued: 1})
			close(addDone)
		}()
		// The departures race the seed end to end - before, during, or
		// after the scan and the callback phase.
		for _, cancel := range cancels {
			cancel()
		}
		select {
		case <-addDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: AddHold stuck", i)
		}
		for g, done := range wDones {
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("iter %d: waiter %d err = %v, want context.Canceled", i, g, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("iter %d: waiter %d did not return after cancel", i, g)
			}
		}
		holds := s.HoldsList()
		if len(holds) != 1 {
			t.Fatalf("iter %d: holds = %d, want 1", i, len(holds))
		}
		waitFor(t, func() bool { return s.HoldQueued(holds[0].ID) == 0 },
			fmt.Sprintf("iter %d: hold count to drain to zero (leaked departed waiter)", i))
		// The cap must not be burned by a departed waiter: a fresh matching
		// Acquire parks on the hold instead of being refused.
		c2Ctx, cancel2 := context.WithCancel(ctx)
		c2Done := make(chan error, 1)
		c2Held := make(chan struct{})
		go func() {
			_, err := s.AcquireWith(c2Ctx, "k0", 1, WaiterHooks{
				Client: "c",
				OnHold: func() { close(c2Held) },
			})
			c2Done <- err
		}()
		waitFor(t, func() bool {
			select {
			case <-c2Held:
				return true
			default:
				return false
			}
		}, fmt.Sprintf("iter %d: fresh matching Acquire to be admitted (cap burned by leaked count)", i))
		cancel2()
		select {
		case err := <-c2Done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("iter %d: fresh waiter err = %v, want context.Canceled", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: fresh waiter did not return after cancel", i)
		}
		for _, rel := range rels {
			rel(0)
		}
	}
}
