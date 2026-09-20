package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// quotaTestOptions arms only the quota gate family: every threshold-storm
// facet is disabled so the tests prove the quota gate is independent.
func quotaTestOptions() StormOptions {
	return StormOptions{Enabled: false, ModelEnabled: false, ProviderEnabled: false,
		Window: time.Minute, MinSamples: 1, ErrorPercent: 1,
		InitialBackoff: 15 * time.Millisecond, MaxBackoff: 30 * time.Millisecond, BackoffMultiplier: 2,
		JitterPercent: 0, RecoverySuccesses: 2,
		QuotaEnabled: true, QuotaRecoverySuccesses: 1,
		MaxQueue: 8, MaxScopes: 32, MaxWait: time.Second}
}

func quotaRow(s *Scheduler) (StormStatus, bool) {
	for _, row := range s.StormSnapshot() {
		if row.Quota {
			return row, true
		}
	}
	return StormStatus{}, false
}

func quotaScope(s *Scheduler, provider string) *stormScope {
	s.storms.mu.Lock()
	defer s.storms.mu.Unlock()
	return s.storms.scopes[stormKey{provider: provider}]
}

func TestQuotaGateOpensOnFirstObservationAndParksSends(t *testing.T) {
	s := newStormTestScheduler(t, quotaTestOptions())
	// With every storm facet disabled the permit still carries the provider
	// scope, so the first durable quota observation opens the gate. The
	// provider hint keeps the window open long enough to observe parking.
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", time.Hour)
	row, ok := quotaRow(s)
	if !ok || row.Provider != "provider.test" || row.Scope != "provider" || row.Reason != "insufficient_quota" ||
		row.RecoveryRequired != 1 || row.RecoverySuccesses != 0 || row.State != "open" || row.Queued != 0 {
		t.Fatalf("quota gate did not open on first observation: %+v ok=%v", row, ok)
	}
	// A new send parks on the open gate.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitStorm(ctx, "provider.test", "model-a", nil)
		done <- err
	}()
	waitFor(t, func() bool { return stormQueued(s) == 1 }, "quota gate parks new sends")
	select {
	case err := <-done:
		t.Fatalf("send passed the open quota gate: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	if !errors.Is(<-done, context.Canceled) || stormQueued(s) != 0 {
		t.Fatal("parked waiter did not cancel cleanly")
	}
	// A departed waiter leaves the gate open; the next due send claims the
	// probe and its success closes the gate at the recovery threshold.
	if _, ok := quotaRow(s); !ok {
		t.Fatal("quota gate closed without a recovery probe")
	}
	stormDue(s)
	stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
	if _, ok := quotaRow(s); ok {
		t.Fatal("successful probe did not close the quota gate")
	}
	// Closed gates stop parking: a fresh send admits immediately.
	stormPermit(t, s, "provider.test", "model-a").Cancel()
}

func TestQuotaGatePacingGrowsOnProbeFailuresAndHonorsRetryAfter(t *testing.T) {
	opts := quotaTestOptions()
	s := newStormTestScheduler(t, opts)
	// A sibling admitted before the gate opens keeps its non-probe claim;
	// settling it later must not disturb the gate's shared pacing.
	sibling := stormPermit(t, s, "provider.test", "model-a")
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 0)
	if scope := quotaScope(s, "provider.test"); scope.backoff != opts.InitialBackoff {
		t.Fatalf("initial backoff = %v, want %v", scope.backoff, opts.InitialBackoff)
	}
	// The due probe finds the account still exhausted: backoff grows and a
	// provider Retry-After hint is a floor on the next window.
	stormDue(s)
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 90*time.Minute)
	scope := quotaScope(s, "provider.test")
	if got := scope.backoff; got != opts.InitialBackoff*time.Duration(opts.BackoffMultiplier) {
		t.Fatalf("failed probe did not grow backoff: %v", got)
	}
	if until := time.Until(scope.retryAt); until < 89*time.Minute {
		t.Fatalf("Retry-After hint was not honored as a floor: %v left", until)
	}
	// Pathological hints clamp to the shared ceiling like every other path.
	stormDue(s)
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 1000*time.Hour)
	if until := time.Until(quotaScope(s, "provider.test").retryAt); until > MaxRetryHint {
		t.Fatalf("hint exceeded the MaxRetryHint ceiling: %v", until)
	}
	// The sibling's observation while the gate is already open neither
	// resets nor grows the shared pacing (the probe owns it).
	before := quotaScope(s, "provider.test").backoff
	sibling.ObserveQuota("insufficient_quota", 0)
	if got := quotaScope(s, "provider.test").backoff; got != before {
		t.Fatalf("non-probe observation changed pacing: %v -> %v", before, got)
	}
}

func TestQuotaGateReleaseResumesImmediately(t *testing.T) {
	s := newStormTestScheduler(t, quotaTestOptions())
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitStorm(ctx, "provider.test", "model-a", nil)
		done <- err
	}()
	waitFor(t, func() bool { return stormQueued(s) == 1 }, "quota gate parks new sends")
	// The operator release closes the gate without waiting out the window.
	if !s.ReleaseQuota("provider.test") {
		t.Fatal("release reported no open gate")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parked send did not resume after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("parked send did not resume after release")
	}
	if s.ReleaseQuota("provider.test") {
		t.Fatal("double release reported an open gate")
	}
	if _, ok := quotaRow(s); ok {
		t.Fatal("release left the gate visible")
	}
}

func TestQuotaGateSurvivesScopeEvictionPressure(t *testing.T) {
	opts := quotaTestOptions()
	opts.MaxScopes = 4
	opts.Window = time.Millisecond
	s := newStormTestScheduler(t, opts)
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 0)
	// Churn other providers past the scope ceiling so the eviction walk runs
	// repeatedly; an open quota gate must never be evicted.
	for i := 0; i < 60; i++ {
		stormPermit(t, s, fmt.Sprintf("other-%d.test", i), "m").Cancel()
		time.Sleep(time.Millisecond)
	}
	if quotaScope(s, "provider.test") == nil || quotaScope(s, "provider.test").quota != true {
		t.Fatal("open quota gate was evicted under scope pressure")
	}
	if _, ok := quotaRow(s); !ok {
		t.Fatal("open quota gate disappeared from the snapshot")
	}
}

func TestQuotaObservationsNeverBecomeStormEvidence(t *testing.T) {
	opts := quotaTestOptions()
	opts.Enabled = true
	opts.ModelEnabled = true
	opts.ProviderEnabled = true
	s := newStormTestScheduler(t, opts)
	// Repeated durable quota outcomes - including failed probes - never book
	// storm samples or open threshold gates, even with storm protection on
	// and its thresholds trivially reachable.
	stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 0)
	for i := 0; i < 3; i++ {
		stormDue(s)
		stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 0)
	}
	rows := s.StormSnapshot()
	if len(rows) != 1 || !rows[0].Quota || rows[0].Samples != 0 || rows[0].Failures != 0 || rows[0].ErrorRequests != 0 {
		t.Fatalf("quota outcomes became storm evidence: %+v", rows)
	}
}

// TestQuotaGateNeverLeaksTheProbeFlag pins the probe-exclusivity lifecycle
// against the leak paths a review confirmed: a release while a probe is in
// flight, and a quota observation arriving on a storm probe's permit. A
// leaked probing flag parks its scope forever, because no new permit can
// claim and release it.
func TestQuotaGateNeverLeaksTheProbeFlag(t *testing.T) {
	t.Run("release while a probe is in flight", func(t *testing.T) {
		opts := quotaTestOptions()
		opts.InitialBackoff = time.Hour
		opts.MaxBackoff = time.Hour
		s := newStormTestScheduler(t, opts)
		stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 0)
		stormDue(s)
		probe := stormPermit(t, s, "provider.test", "model-a") // claims the probe
		if !s.ReleaseQuota("provider.test") {
			t.Fatal("release reported no open gate")
		}
		// The in-flight probe settles after the release; its flag died with
		// the gate and a later reopen must park on its own window, not on a
		// leaked exclusivity bit.
		probe.Observe(false, "", 0)
		stormPermit(t, s, "provider.test", "model-a").ObserveQuota("insufficient_quota", 0) // reopen
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.WaitStorm(ctx, "provider.test", "model-a", nil)
			done <- err
		}()
		waitFor(t, func() bool { return stormQueued(s) == 1 }, "reopen parks new sends")
		select {
		case err := <-done:
			t.Fatalf("send passed the reopened quota gate: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		cancel()
		if !errors.Is(<-done, context.Canceled) {
			t.Fatal("parked waiter did not cancel cleanly")
		}
		// The reopened window elapses: the next send must admit as a probe
		// and its success must close the gate (no stuck probing).
		stormDue(s)
		stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
		if _, ok := quotaRow(s); ok {
			t.Fatal("probe never admitted or gate never closed after release+reopen")
		}
		stormPermit(t, s, "provider.test", "model-a").Cancel() // immediate: nothing parks
	})

	t.Run("quota observation on a storm probe releases both scopes", func(t *testing.T) {
		opts := quotaTestOptions()
		opts.Enabled = true
		opts.ModelEnabled = true
		opts.InitialBackoff = time.Hour
		opts.MinSamples = 1
		opts.ErrorPercent = 1
		opts.RecoverySuccesses = 1
		s := newStormTestScheduler(t, opts)
		// One sample trips the trivially reachable model storm; its probe is
		// claimed after the window and then finds the account exhausted.
		stormPermit(t, s, "provider.test", "model-a").Observe(true, "HTTP 503", 0)
		if rows := s.StormSnapshot(); len(rows) != 1 || rows[0].Scope != "model" {
			t.Fatalf("model storm did not open: %+v", rows)
		}
		stormDue(s)
		probe := stormPermit(t, s, "provider.test", "model-a")
		probe.ObserveQuota("insufficient_quota", 0)
		s.storms.mu.Lock()
		leaked := false
		for _, scope := range s.storms.scopes {
			if scope.probing {
				leaked = true
			}
		}
		s.storms.mu.Unlock()
		if leaked {
			t.Fatal("quota observation on a storm probe leaked the probing flag")
		}
		if !s.ReleaseQuota("provider.test") {
			t.Fatal("quota gate did not open from the storm probe")
		}
		// The storm gate's own recovery continues: a probe admits after the
		// window and its success closes the storm.
		stormDue(s)
		stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
		if rows := s.StormSnapshot(); len(rows) != 0 {
			t.Fatalf("storm recovery stuck after the quota detour: %+v", rows)
		}
	})
}

// TestQuotaProbeFlagReleaseIsClaimOwned pins the probe-flag ownership fix:
// the unique unsettled probe claim owns the flag, so a cross-facet gate
// opening under an in-flight probe (or the operator release) can neither
// strand the flag nor mint a competing probe. The epoch guard protects only
// the gate state; the flag release follows the claim across epoch bumps.
func TestQuotaProbeFlagReleaseIsClaimOwned(t *testing.T) {
	quotaMixedFacetOptions := func() StormOptions {
		opts := quotaTestOptions()
		opts.Enabled = true
		opts.ModelEnabled = true
		opts.ProviderEnabled = true
		opts.MinSamples = 1
		opts.ErrorPercent = 1
		opts.RecoverySuccesses = 1
		return opts
	}

	t.Run("threshold open under an in-flight quota probe", func(t *testing.T) {
		s := newStormTestScheduler(t, quotaMixedFacetOptions())
		// Two sends admitted before any gate: the first opens the quota
		// facet, the second (still unsettled) opens the storm facets under
		// the quota probe claimed in between.
		preGateA := stormPermit(t, s, "provider.test", "model-a")
		preGateB := stormPermit(t, s, "provider.test", "model-a")
		preGateA.ObserveQuota("insufficient_quota", 0)
		stormDue(s)
		probe := stormPermit(t, s, "provider.test", "model-a")
		preGateB.Observe(true, "HTTP 503", 0)
		scope := quotaScope(s, "provider.test")
		if !scope.active || !scope.quota {
			t.Fatalf("mixed gate did not open: active=%v quota=%v", scope.active, scope.quota)
		}
		// The probe settles 2xx: its claim still owns the flag release even
		// though the epoch moved under it.
		probe.Observe(false, "", 0)
		scope = quotaScope(s, "provider.test")
		if scope.probing {
			t.Fatal("threshold open under the in-flight probe stranded the probing flag")
		}
		if !scope.active || !scope.quota || scope.recovered != 0 {
			t.Fatalf("stale probe mutated the newer gate generation: active=%v quota=%v recovered=%d",
				scope.active, scope.quota, scope.recovered)
		}
		// Recovery continues on the newer generation: the next probe drains
		// the gate and a following send admits immediately.
		stormDue(s)
		stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
		if rows := s.StormSnapshot(); len(rows) != 0 {
			t.Fatalf("recovery stuck after the epoch bump: %+v", rows)
		}
		stormPermit(t, s, "provider.test", "model-a").Cancel()
	})

	t.Run("quota open under an in-flight storm probe", func(t *testing.T) {
		s := newStormTestScheduler(t, quotaMixedFacetOptions())
		preGate := stormPermit(t, s, "provider.test", "model-a")
		stormPermit(t, s, "provider.test", "model-a").Observe(true, "HTTP 503", 0)
		stormDue(s)
		probe := stormPermit(t, s, "provider.test", "model-a")
		preGate.ObserveQuota("insufficient_quota", 0)
		probe.Observe(false, "", 0)
		scope := quotaScope(s, "provider.test")
		if scope.probing {
			t.Fatal("quota open under the in-flight probe stranded the probing flag")
		}
		if !scope.active || !scope.quota || scope.recovered != 0 {
			t.Fatalf("stale probe mutated the newer gate generation: active=%v quota=%v recovered=%d",
				scope.active, scope.quota, scope.recovered)
		}
		stormDue(s)
		stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
		if rows := s.StormSnapshot(); len(rows) != 0 {
			t.Fatalf("recovery stuck after the epoch bump: %+v", rows)
		}
		stormPermit(t, s, "provider.test", "model-a").Cancel()
	})

	t.Run("release keeps the in-flight probe the flag owner", func(t *testing.T) {
		// A provider-facet-only storm gate: the in-flight probe holds only
		// the provider scope's flag, so the release's manual clear is the
		// one difference between a parked and a minted second probe.
		opts := quotaMixedFacetOptions()
		opts.ModelEnabled = false
		s := newStormTestScheduler(t, opts)
		preGate := stormPermit(t, s, "provider.test", "model-a")
		stormPermit(t, s, "provider.test", "model-a").Observe(true, "HTTP 503", 0)
		preGate.ObserveQuota("insufficient_quota", 0)
		stormDue(s)
		probe := stormPermit(t, s, "provider.test", "model-a")
		if !s.ReleaseQuota("provider.test") {
			t.Fatal("release reported no open gate")
		}
		// The storm facet stays open, so the next send must park on the
		// in-flight probe's flag instead of minting a second probe.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.WaitStorm(ctx, "provider.test", "model-a", nil)
			done <- err
		}()
		waitFor(t, func() bool { return stormQueued(s) == 1 }, "release let a second probe mint while the first was in flight")
		cancel()
		if !errors.Is(<-done, context.Canceled) {
			t.Fatal("parked waiter did not cancel cleanly")
		}
		// The in-flight probe's own settle releases the flag it minted and
		// drains the remaining storm facet.
		probe.Observe(false, "", 0)
		scope := quotaScope(s, "provider.test")
		if scope.probing {
			t.Fatal("probe settle did not release the flag after the release")
		}
		if rows := s.StormSnapshot(); len(rows) != 0 {
			t.Fatalf("storm facet did not drain after the release: %+v", rows)
		}
		stormPermit(t, s, "provider.test", "model-a").Cancel()
	})
}

// TestMixedFacetCloseKeepsTheQuotaProbePaced pins the shared cooldown owner:
// a facet close clears it only when no facet remains open, so a storm facet
// recovering under an open quota gate never admits an unspaced billed probe.
func TestMixedFacetCloseKeepsTheQuotaProbePaced(t *testing.T) {
	opts := quotaTestOptions()
	opts.Enabled = true
	opts.ModelEnabled = true
	opts.ProviderEnabled = true
	opts.MinSamples = 1
	opts.ErrorPercent = 1
	opts.RecoverySuccesses = 1
	opts.QuotaRecoverySuccesses = 3
	opts.InitialBackoff = time.Hour
	opts.MaxBackoff = time.Hour
	s := newStormTestScheduler(t, opts)
	preGate := stormPermit(t, s, "provider.test", "model-a")
	stormPermit(t, s, "provider.test", "model-a").Observe(true, "HTTP 503", 0)
	preGate.ObserveQuota("insufficient_quota", 0)
	// Both facets open; one success crosses the storm threshold but not the
	// quota one (RecoverySuccesses < QuotaRecoverySuccesses).
	stormDue(s)
	stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
	scope := quotaScope(s, "provider.test")
	if scope.active || !scope.quota {
		t.Fatalf("storm facet did not close under the open quota facet: active=%v quota=%v", scope.active, scope.quota)
	}
	// The next quota probe stays paced by the shared cooldown instead of
	// admitting unspaced after the storm close.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitStorm(ctx, "provider.test", "model-a", nil)
		done <- err
	}()
	waitFor(t, func() bool { return stormQueued(s) == 1 }, "mixed close admitted an unspaced quota probe")
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("parked waiter did not cancel cleanly")
	}
	// The paced probes close the quota facet across its own threshold.
	for i := 0; i < 2; i++ {
		stormDue(s)
		stormPermit(t, s, "provider.test", "model-a").Observe(false, "", 0)
	}
	if rows := s.StormSnapshot(); len(rows) != 0 {
		t.Fatalf("quota facet did not close after paced recovery: %+v", rows)
	}
	stormPermit(t, s, "provider.test", "model-a").Cancel()
}
