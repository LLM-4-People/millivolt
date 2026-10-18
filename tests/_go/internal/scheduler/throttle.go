package scheduler

import (
	"context"
	"fmt"
	"runtime"
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
	s.SetThrottle(Throttle{Provider: "p"})
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
	s.SetThrottle(Throttle{Provider: "p"})
	waitFor(t, func() bool { return !throttled.Load() }, "OnUnthrottle to fire after the cap clear")
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
	restored := func(provider string) Throttle {
		g := s2.gate(provider)
		g.mu.Lock()
		cur := g.throttle
		g.mu.Unlock()
		return cur
	}
	if restored("b.com").Limit.Concurrency != 2 {
		t.Fatal("restore missed concurrency")
	}
	if restored("a.com").Limit.Requests != 10 {
		t.Fatal("restore missed requests")
	}
}

// TestThrottlePoliciesMatchesListThrottles pins the policy-only listing to the
// Throttle portion of the dashboard's live listing for every active provider,
// including one with in-flight and queued occupancy and providers with none.
// Persistence uses this listing, so its sorted policy set must stay identical
// to what ListThrottles reports.
func TestThrottlePoliciesMatchesListThrottles(t *testing.T) {
	s := New(Options{})
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, th := range []Throttle{
		{Provider: "alpha.example", Limit: Limit{Requests: 30, ReqWindow: time.Minute}, Source: ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: base},
		{Provider: "busy.example", Limit: Limit{Concurrency: 1}, Source: ThrottleSourceHeader, UpdatedBy: "client-a", UpdatedAt: base.Add(time.Minute)},
		{Provider: "zulu.example", Limit: Limit{Tokens: 9000, TokWindow: time.Hour}, Source: ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: base.Add(2 * time.Minute)},
	} {
		if !s.SetThrottle(th) {
			t.Fatalf("failed to install policy for %s", th.Provider)
		}
	}

	ctx := context.Background()
	holder, err := s.AcquireWith(ctx, "busy.example|holder", 0, WaiterHooks{Provider: "busy.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer holder(0)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.AcquireWith(ctx, "busy.example|queued", 0, WaiterHooks{Provider: "busy.example"})
			if err != nil {
				t.Error(err)
				return
			}
			release(0)
		}()
	}
	waitFor(t, func() bool { return s.Stats().Queued >= 2 }, "two throttled waiters to queue")

	infos := s.ListThrottles()
	policies := s.ThrottlePolicies()
	if len(infos) != len(policies) {
		t.Fatalf("listing lengths differ: ListThrottles=%d ThrottlePolicies=%d", len(infos), len(policies))
	}
	for i := range infos {
		if infos[i].Provider != policies[i].Provider || infos[i].Throttle != policies[i] {
			t.Fatalf("policy %d differs: ListThrottles=%+v ThrottlePolicies=%+v", i, infos[i].Throttle, policies[i])
		}
	}
	if infos[1].Provider != "busy.example" || infos[1].InFlight != 1 || infos[1].Queued != 2 {
		t.Fatalf("busy occupancy = %+v, want 1 in flight and 2 queued", infos[1])
	}
	if infos[0].InFlight != 0 || infos[0].Queued != 0 || infos[2].InFlight != 0 || infos[2].Queued != 0 {
		t.Fatalf("idle occupancy leaked into live listing: %+v", infos)
	}

	holder(0)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued waiters did not finish")
	}
}

// TestThrottlePoliciesSnapshotSurvivesGateUpdate pins value semantics: a
// returned policy snapshot must not change when its gate is later updated.
// Throttle currently owns only value fields (its time.Time location pointer is
// immutable), so the struct copy is sufficient; a future reference field would
// fail this test without a deep copy.
func TestThrottlePoliciesSnapshotSurvivesGateUpdate(t *testing.T) {
	s := New(Options{})
	base := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	initial := Throttle{
		Provider:  "provider.example",
		Limit:     Limit{Concurrency: 1, Requests: 7, ReqWindow: time.Minute, Tokens: 70, TokWindow: time.Hour},
		Source:    ThrottleSourceUI,
		UpdatedBy: "dashboard",
		UpdatedAt: base,
	}
	if !s.SetThrottle(initial) {
		t.Fatal("initial policy was not installed")
	}
	before := s.ThrottlePolicies()
	if len(before) != 1 || before[0] != initial {
		t.Fatalf("policy snapshot = %+v, want %+v", before, initial)
	}

	changed := initial
	changed.Limit = Limit{Concurrency: 2, Requests: 8, ReqWindow: 2 * time.Minute, Tokens: 80, TokWindow: 2 * time.Hour}
	changed.Source = ThrottleSourceHeader
	changed.UpdatedBy = "client-b"
	changed.UpdatedAt = base.Add(time.Hour)
	if !s.SetThrottle(changed) {
		t.Fatal("changed policy was not installed")
	}
	if before[0] != initial {
		t.Fatalf("gate update mutated the earlier snapshot: %+v, want %+v", before[0], initial)
	}
	after := s.ThrottlePolicies()
	if len(after) != 1 || after[0] != changed {
		t.Fatalf("new snapshot = %+v, want %+v", after, changed)
	}
}

// TestThrottlePoliciesDoesNotLockGroups pins the persistence path's lock
// scope: policy listing must read gate policies under throttleMu without
// taking a group mutex, so a busy group can never delay a durable save.
func TestThrottlePoliciesDoesNotLockGroups(t *testing.T) {
	s := New(Options{})
	if !s.SetThrottle(Throttle{Provider: "provider.example", Limit: Limit{Concurrency: 1}}) {
		t.Fatal("policy was not installed")
	}
	g := s.groupFor("provider.example|key", 0)
	g.mu.Lock()
	listed := make(chan []Throttle, 1)
	go func() { listed <- s.ThrottlePolicies() }()
	select {
	case got := <-listed:
		if len(got) != 1 || got[0].Provider != "provider.example" {
			g.mu.Unlock()
			t.Fatalf("policy listing = %+v", got)
		}
	case <-time.After(2 * time.Second):
		g.mu.Unlock()
		t.Fatal("policy listing waited for a group lock")
	}
	g.mu.Unlock()
}

// TestThrottleQueuedAttributionPerProvider pins throttleQueued's per-provider
// attribution: ListThrottles' Queued occupancy (the wire `queued` field) must
// count each provider's own parked waiters and only those. Two waiters queue
// on provider p's concurrency cap while provider q's equally active gate has
// none, so a misattributed or global count cannot pass.
func TestThrottleQueuedAttributionPerProvider(t *testing.T) {
	s := New(Options{})
	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 1}})
	s.SetThrottle(Throttle{Provider: "q", Limit: Limit{Concurrency: 1}})
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
	// Both waiters must be parked on the cap before the attribution read.
	waitFor(t, func() bool { return s.Stats().Queued >= 2 }, "two throttled waiters to queue")

	for _, ti := range s.ListThrottles() {
		switch ti.Provider {
		case "p":
			if ti.Queued != 2 {
				t.Fatalf("provider p queued = %d, want 2 (its own parked waiters)", ti.Queued)
			}
		case "q":
			if ti.Queued != 0 {
				t.Fatalf("provider q queued = %d, want 0 (no waiters parked on it)", ti.Queued)
			}
		default:
			t.Fatalf("unexpected provider %q in ListThrottles", ti.Provider)
		}
	}

	s.SetThrottle(Throttle{Provider: "p", Limit: Limit{Concurrency: 8}})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loosened cap did not grant waiters")
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

// TestBucketWaitClampedToCeiling pins the overflow guard in bucket.waitFor.
// A legal 1-token/24h cap plus one settled huge usage leaves a debt whose
// exact refill time (200001 tokens at 1/86400 tokens/s, roughly 547 years)
// exceeds time.Duration's ~292-year span: the naive
// time.Duration(need / rate * float64(time.Second)) conversion overflowed to
// minInt64, the sub-millisecond floor then clamped it to 1ms, and
// armThrottleDrain re-armed about a thousand times per second while the
// provider stayed token-blocked. The wait must clamp to the MaxRetryHint
// ceiling (throttle.go's maxBucketWait) before converting; a larger debt
// wakes once per ceiling and re-arms, while settles and cap changes re-check
// admission anyway - the timer is only a fallback heartbeat.
func TestBucketWaitClampedToCeiling(t *testing.T) {
	var b bucket
	b.reset(1, 24*time.Hour, time.Now())
	// Settled-debt shape: one request settling a huge negative usage.
	b.tokens = -200000
	if got := b.waitFor(1); got != MaxRetryHint {
		t.Fatalf("overflowed bucket wait = %v, want the %v ceiling", got, MaxRetryHint)
	}

	// Control: a small in-range need still computes the exact duration (a
	// zero last lets refill only stamp the clock, so tokens stay put).
	var c bucket
	c.enabled, c.rate, c.capacity, c.tokens = true, 2, 10, -5
	if got := c.waitFor(1); got != 3*time.Second {
		t.Fatalf("in-range bucket wait = %v, want exactly 3s", got)
	}
}

// TestProviderDrainScopedToReleasedProvider pins the provider-to-groups index:
// releasing alpha's last slot drains only alpha's groups. Beta's waiter stays
// parked until beta itself releases, even though its gate is eligible and
// alpha's drain visits every group that could become admissible for alpha.
func TestProviderDrainScopedToReleasedProvider(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	providers := []string{"alpha.example", "beta.example"}
	// Attribute several idle groups per provider before the caps are active so
	// the index has more than the two queued groups below.
	for _, provider := range providers {
		for _, key := range []string{"idle-1", "idle-2"} {
			release, err := s.AcquireWith(ctx, provider+"|"+key, 0, WaiterHooks{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			release(0)
		}
	}
	for _, provider := range providers {
		s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
	}

	holdAlpha, err := s.AcquireWith(ctx, "alpha.example|held", 0, WaiterHooks{Provider: "alpha.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer holdAlpha(0)
	holdBeta, err := s.AcquireWith(ctx, "beta.example|held", 0, WaiterHooks{Provider: "beta.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer holdBeta(0)

	granted := make(chan string, 2)
	for _, provider := range providers {
		go func() {
			release, err := s.AcquireWith(ctx, provider+"|queued", 0, WaiterHooks{Provider: provider})
			if err != nil {
				t.Errorf("%s waiter: %v", provider, err)
				return
			}
			release(0)
			granted <- provider
		}()
	}
	waitFor(t, func() bool { return s.Stats().Queued >= 2 }, "both provider waiters to queue")

	holdAlpha(0)
	select {
	case provider := <-granted:
		if provider != "alpha.example" {
			t.Fatalf("granted %q after alpha release, want alpha.example", provider)
		}
	case <-time.After(time.Second):
		t.Fatal("alpha release did not drain alpha's queued waiter")
	}
	select {
	case provider := <-granted:
		t.Fatalf("alpha release drained %q's queued work", provider)
	case <-time.After(40 * time.Millisecond):
	}

	holdBeta(0)
	select {
	case provider := <-granted:
		if provider != "beta.example" {
			t.Fatalf("granted %q after beta release, want beta.example", provider)
		}
	case <-time.After(time.Second):
		t.Fatal("beta waiter was stranded after alpha's provider-scoped drain")
	}
}

// TestProviderIndexAttributesGroupLearnedAfterCreation covers the only
// provider transition a group has: empty to its first non-empty attribution.
// The group is created by an unattributed Acquire, then learns its provider
// while a same-provider holder on another group fills the cap. The holder's
// release must find it through the index or the waiter would be stranded.
func TestProviderIndexAttributesGroupLearnedAfterCreation(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	provider := "late.example"
	release, err := s.AcquireWith(ctx, provider+"|late", 0, WaiterHooks{})
	if err != nil {
		t.Fatal(err)
	}
	release(0)
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
	holder, err := s.AcquireWith(ctx, provider+"|holder", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	defer holder(0)

	done := make(chan struct{})
	go func() {
		release, err := s.AcquireWith(ctx, provider+"|late", 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Errorf("late-attributed waiter: %v", err)
			return
		}
		release(0)
		close(done)
	}()
	waitFor(t, func() bool { return s.Stats().Queued >= 1 }, "late-attributed waiter to queue")

	holder(0)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("group learned after creation was not in the provider index")
	}
}

// TestProviderDrainReleasesSchedulerLockBeforeGroup pins the lock order
// documented on drainProvider. With a group mutex held, a provider drain must
// still let another actor take the scheduler mutex; holding s.mu while blocked
// on g.mu would deadlock against paths that take s.mu -> g.mu.
func TestProviderDrainReleasesSchedulerLockBeforeGroup(t *testing.T) {
	s := New(Options{})
	provider := "lock.example"
	release, err := s.AcquireWith(context.Background(), provider+"|k", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	release(0)
	g := s.groupFor(provider+"|k", 0)
	g.mu.Lock()
	drained := make(chan struct{})
	go func() {
		s.drainProvider(provider)
		close(drained)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s.mu.TryLock() {
			s.mu.Unlock()
			break
		}
		if time.Now().After(deadline) {
			g.mu.Unlock()
			<-drained
			t.Fatal("provider drain held the scheduler mutex while waiting for a group lock")
		}
		runtime.Gosched()
	}
	g.mu.Unlock()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("provider drain did not finish after the group lock was released")
	}
}

// TestProviderDrainIdleProviderDoesNotLockGroups pins the empty-provider early
// return: with no queued waiters the provider's queued-group counter is zero,
// so a drain must return without taking any group mutex. The deliberately held
// group lock would otherwise block the drain.
func TestProviderDrainIdleProviderDoesNotLockGroups(t *testing.T) {
	s := New(Options{})
	provider := "idle.example"
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		release, err := s.AcquireWith(ctx, fmt.Sprintf("%s|%d", provider, i), 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatal(err)
		}
		release(0)
	}
	idle := s.groupFor(provider+"|0", 0)
	idle.mu.Lock()
	drained := make(chan struct{})
	go func() {
		s.drainProvider(provider)
		close(drained)
	}()
	select {
	case <-drained:
		idle.mu.Unlock()
	case <-time.After(time.Second):
		idle.mu.Unlock()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			t.Fatal("provider drain did not finish after the idle group lock was released")
		}
		t.Fatal("provider drain locked an idle group with an empty queue")
	}
}

// TestProviderDrainSkipsIdleGroupsAndGrantsQueued pins the skip half of the
// witness: one queued group behind a provider concurrency cap, among idle
// groups attributed earlier, must still receive the release grant while the
// idle groups are skipped without taking their mutex. The test holds the first
// idle group's lock, so a drain that walks every group blocks instead.
func TestProviderDrainSkipsIdleGroupsAndGrantsQueued(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	provider := "mixed.example"
	for i := 0; i < 32; i++ {
		release, err := s.AcquireWith(ctx, fmt.Sprintf("%s|idle-%d", provider, i), 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatal(err)
		}
		release(0)
	}
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
	holder, err := s.AcquireWith(ctx, provider+"|held", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	granted := make(chan struct{})
	acquireErr := make(chan error, 1)
	go func() {
		release, err := s.AcquireWith(ctx, provider+"|queued", 0, WaiterHooks{Provider: provider})
		if err != nil {
			acquireErr <- err
			return
		}
		release(0)
		close(granted)
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 1 }, "the queued waiter to park behind the provider cap")

	idle := s.groupFor(provider+"|idle-0", 0)
	idle.mu.Lock()
	released := make(chan struct{})
	go func() {
		holder(0)
		close(released)
	}()
	select {
	case <-released:
		idle.mu.Unlock()
	case <-time.After(time.Second):
		idle.mu.Unlock()
		select {
		case <-released:
		case <-time.After(2 * time.Second):
			t.Fatal("release did not finish after the idle group lock was released")
		}
		t.Fatal("release blocked on an idle group instead of skipping it")
	}
	select {
	case <-granted:
	case err := <-acquireErr:
		t.Fatalf("queued waiter: %v", err)
	case <-time.After(time.Second):
		t.Fatal("queued waiter was not granted by the provider release")
	}
	if st := s.Stats(); st.Queued != 0 || st.InFlight != 0 {
		t.Fatalf("release left occupancy behind: %+v", st)
	}
}

// providerQueuedGroups reads a provider's queued-group witness counter under
// the scheduler lock (the map itself is only mutated under s.mu).
func providerQueuedGroups(s *Scheduler, provider string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx := s.providerGroups[provider]; idx != nil {
		return idx.queued.Load()
	}
	return 0
}

// snapshot returns every attributed group. The caller must not hold idx.mu.
func (idx *providerIndex) snapshot() []*group {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := make([]*group, 0, idx.live)
	for g := idx.head; g != nil; g = g.providerNext {
		out = append(out, g)
	}
	return out
}

// providerGroupsSnapshot returns a provider's live attributed groups. The
// index may lose its last group and be dropped from the map after the lookup,
// but the pointer itself stays valid, so the snapshot locks the index rather
// than the scheduler.
func providerGroupsSnapshot(s *Scheduler, provider string) []*group {
	s.mu.Lock()
	idx := s.providerGroups[provider]
	s.mu.Unlock()
	if idx == nil {
		return nil
	}
	return idx.snapshot()
}

// TestProviderIndexAdoptsQueuedGroupAtAttribution pins the empty->non-empty
// transition that happens before attribution: an empty-provider Acquire can
// park a waiter behind an operator hold, and a later provider-bearing Acquire
// attributes that already-non-empty group. The index counter must adopt the
// queued group exactly once, so the provider drain sees it and the counter
// returns to zero - never negative - when the queue empties.
func TestProviderIndexAdoptsQueuedGroupAtAttribution(t *testing.T) {
	s := New(Options{})
	provider := "late.example"
	key := provider + "|late"
	ctx := context.Background()
	s.RestoreHolds([]Hold{{All: true}})

	firstDone := make(chan struct{})
	go func() {
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{})
		if err != nil {
			t.Errorf("empty-provider waiter: %v", err)
			return
		}
		release(0)
		close(firstDone)
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 1 }, "empty-provider waiter to queue")

	secondDone := make(chan struct{})
	go func() {
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Errorf("attributed waiter: %v", err)
			return
		}
		release(0)
		close(secondDone)
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 2 }, "attributed waiter to queue")
	if got := providerQueuedGroups(s, provider); got != 1 {
		t.Fatalf("provider queued groups = %d, want 1 after attributing an already-queued group", got)
	}

	s.RestoreHolds(nil)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("empty-provider waiter stranded after unpause")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("attributed waiter stranded after unpause")
	}
	if got := providerQueuedGroups(s, provider); got != 0 {
		t.Fatalf("provider queued groups = %d, want 0 after the queue drained", got)
	}
}

// TestProviderQueuedCounterRaceWithCancellationAndCreation hammers every
// counter transition owner under the race detector: waiters enqueue, cancel,
// drain, and create brand-new groups while provider drains run. Every acquire
// is bounded, so a lost wake surfaces as a deadline error; each group's
// witness must always agree with its live count, and the provider counter must
// return to zero.
func TestProviderQueuedCounterRaceWithCancellationAndCreation(t *testing.T) {
	s := New(Options{})
	provider := "race.example"
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 1}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	holder, err := s.AcquireWith(ctx, provider+"|holder", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var drainers sync.WaitGroup
	for i := 0; i < 4; i++ {
		drainers.Add(1)
		go func() {
			defer drainers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.drainProvider(provider)
				runtime.Gosched()
			}
		}()
	}
	var stopOnce sync.Once
	stopDrains := func() {
		stopOnce.Do(func() {
			close(stop)
			drainers.Wait()
		})
	}
	defer stopDrains()

	checkStop := make(chan struct{})
	var checks sync.WaitGroup
	checks.Add(1)
	go func() {
		defer checks.Done()
		for {
			select {
			case <-checkStop:
				return
			default:
			}
			for _, g := range providerGroupsSnapshot(s, provider) {
				g.mu.Lock()
				nonEmpty := g.queued > 0
				if g.hasQueued.Load() != nonEmpty {
					t.Errorf("group witness = %v with queued=%d", g.hasQueued.Load(), g.queued)
				}
				g.mu.Unlock()
			}
			runtime.Gosched()
		}
	}()
	var checkOnce sync.Once
	stopChecks := func() {
		checkOnce.Do(func() {
			close(checkStop)
			checks.Wait()
		})
	}
	defer stopChecks()

	const workers = 16
	cancels := make([]context.CancelFunc, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		workCtx, workCancel := context.WithCancel(ctx)
		cancels[i] = workCancel
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer workCancel()
			key := provider + "|shared"
			if i%4 == 0 {
				key = fmt.Sprintf("%s|new-%d", provider, i)
			}
			release, err := s.AcquireWith(workCtx, key, 1, WaiterHooks{Provider: provider})
			if err != nil {
				if workCtx.Err() == nil {
					t.Errorf("worker %d acquire: %v", i, err)
				}
				return
			}
			release(0)
		}(i)
	}
	waitFor(t, func() bool { return s.Stats().Queued >= workers }, "all race workers to queue")
	for i := 0; i < workers; i += 2 {
		cancels[i]()
	}
	waitFor(t, func() bool { return s.Stats().Queued == workers/2 }, "cancelled workers to leave the queue")

	holder(0)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("acquire/release chain did not settle")
	}
	stopDrains()
	stopChecks()
	if st := s.Stats(); st.Queued != 0 || st.InFlight != 0 {
		t.Fatalf("counter race left occupancy behind: %+v", st)
	}
	if got := providerQueuedGroups(s, provider); got != 0 {
		t.Fatalf("provider queued groups = %d, want 0 after every queue drained", got)
	}
	for _, g := range providerGroupsSnapshot(s, provider) {
		g.mu.Lock()
		if g.hasQueued.Load() || g.queued != 0 {
			t.Errorf("drained group retained witness=%v queued=%d", g.hasQueued.Load(), g.queued)
		}
		g.mu.Unlock()
	}
}

// TestProviderDrainConcurrentGroupCreation exercises the index publication
// race the drain path must tolerate: waiters acquire, release and create new
// provider groups while concurrent drains walk the index. Every acquire uses a
// bounded context, so a dropped wake or lock-order deadlock surfaces as an
// error instead of an unbounded hang.
func TestProviderDrainConcurrentGroupCreation(t *testing.T) {
	s := New(Options{})
	provider := "concurrent.example"
	s.SetThrottle(Throttle{Provider: provider, Limit: Limit{Concurrency: 4}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const (
		workers = 24
		rounds  = 60
	)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				key := fmt.Sprintf("%s|shared-%d", provider, i%8)
				if i%4 == 0 {
					// A brand-new group must become visible to concurrent drains
					// before this Acquire can park behind the provider gate.
					key = fmt.Sprintf("%s|new-%d-%d", provider, worker, i)
				}
				release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
				if err != nil {
					errs <- err
					return
				}
				runtime.Gosched()
				release(0)
			}
		}(worker)
	}

	stop := make(chan struct{})
	var drainers sync.WaitGroup
	for i := 0; i < 4; i++ {
		drainers.Add(1)
		go func() {
			defer drainers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.drainProvider(provider)
				runtime.Gosched()
			}
		}()
	}
	var stopOnce sync.Once
	stopDrainers := func() {
		stopOnce.Do(func() {
			close(stop)
			drainers.Wait()
		})
	}
	defer stopDrainers()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent acquire/release and group creation did not settle")
	}
	stopDrainers()
	close(errs)
	for err := range errs {
		t.Errorf("acquire while draining: %v", err)
	}
	if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
		t.Fatalf("concurrent drain left occupancy behind: %+v", st)
	}
}
