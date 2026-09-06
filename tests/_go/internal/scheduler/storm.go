package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func stormTestOptions() StormOptions {
	return StormOptions{Enabled: true, ModelEnabled: true,
		Window: time.Minute, MinSamples: 2, ErrorPercent: 50,
		InitialBackoff: time.Hour, MaxBackoff: 4 * time.Hour, BackoffMultiplier: 2,
		RecoverySuccesses: 2, MaxQueue: 8, MaxScopes: 32, MaxWait: time.Second}
}

func newStormTestScheduler(t *testing.T, opts StormOptions) *Scheduler {
	t.Helper()
	s := New(Options{Storm: opts})
	t.Cleanup(func() { s.UpdateOptions(Options{}) })
	return s
}

func stormPermit(t *testing.T, s *Scheduler, provider, model string) *StormPermit {
	t.Helper()
	p, err := s.WaitStorm(context.Background(), provider, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func stormFail(t *testing.T, s *Scheduler, provider, model string) bool {
	t.Helper()
	return stormPermit(t, s, provider, model).Observe(true, "HTTP 503", 0)
}

func stormDue(s *Scheduler) {
	s.storms.mu.Lock()
	defer s.storms.mu.Unlock()
	for _, scope := range s.storms.scopes {
		scope.retryAt = time.Now().Add(-time.Second)
		scope.timer.stop()
	}
	s.storms.wakeLocked()
}

func stormQueued(s *Scheduler) int {
	s.storms.mu.Lock()
	defer s.storms.mu.Unlock()
	return s.storms.queued
}

func TestStormDisabledNoop(t *testing.T) {
	s := New(Options{})
	p := stormPermit(t, s, "provider.test", "model")
	if p.Observe(true, "HTTP 503", 0) || len(s.StormSnapshot()) != 0 {
		t.Fatal("disabled storm tracked or gated a request")
	}
	p.Cancel()
	if len(s.storms.scopes) != 0 {
		t.Fatal("disabled storm allocated scopes")
	}
}

func TestStormModelIsolationAndProviderPromotion(t *testing.T) {
	opts := stormTestOptions()
	opts.ProviderEnabled = true
	s := newStormTestScheduler(t, opts)
	stormPermit(t, s, "provider.test", "model-b").Cancel()
	if stormFail(t, s, "provider.test", "model-a") {
		t.Fatal("one sample tripped minimum of two")
	}
	if !stormFail(t, s, "provider.test", "model-a") {
		t.Fatal("model did not trip")
	}
	rows := s.StormSnapshot()
	if len(rows) != 1 || rows[0].Scope != "model" || rows[0].Model != "model-a" || rows[0].ErrorPercent != 100 || rows[0].Samples != 2 {
		t.Fatalf("one failing model broadened provider scope: %+v", rows)
	}
	stormPermit(t, s, "other.test", "model-a").Cancel()
	stormFail(t, s, "provider.test", "model-b")
	stormFail(t, s, "provider.test", "model-b")
	rows = s.StormSnapshot()
	if len(rows) != 3 || rows[0].Scope != "provider" || rows[0].Failures != 4 || rows[0].Samples != 4 || rows[0].ActiveModels != 2 || rows[0].AffectedModels != 2 || rows[0].AffectedModelPercent != 100 {
		t.Fatalf("two failing models did not promote provider: %+v", rows)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitStorm(ctx, "provider.test", "healthy", nil)
		done <- err
	}()
	waitFor(t, func() bool { return stormQueued(s) == 1 }, "provider hold across healthy model")
	cancel()
	if !errors.Is(<-done, context.Canceled) || stormQueued(s) != 0 {
		t.Fatal("provider waiter did not cancel cleanly")
	}
}

func TestStormProviderEvidenceWithoutModelGate(t *testing.T) {
	opts := stormTestOptions()
	opts.ModelEnabled = false
	opts.ProviderEnabled = true
	s := newStormTestScheduler(t, opts)
	stormPermit(t, s, "provider.test", "b").Cancel()
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	if len(s.StormSnapshot()) != 0 {
		t.Fatal("disabled model gate activated")
	}
	stormFail(t, s, "provider.test", "b")
	stormFail(t, s, "provider.test", "b")
	rows := s.StormSnapshot()
	if len(rows) != 1 || rows[0].Scope != "provider" {
		t.Fatalf("provider lost model evidence: %+v", rows)
	}
}

func TestStormSingleProbeRecoveryAndCancellation(t *testing.T) {
	s := newStormTestScheduler(t, stormTestOptions())
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	stormDue(s)
	probe := stormPermit(t, s, "provider.test", "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next := make(chan *StormPermit, 1)
	errCh := make(chan error, 1)
	go func() {
		p, err := s.WaitStorm(ctx, "provider.test", "a", nil)
		if err != nil {
			errCh <- err
			return
		}
		next <- p
	}()
	waitFor(t, func() bool { return stormQueued(s) == 1 }, "exclusive probe waiter")
	probe.Cancel()
	var second *StormPermit
	select {
	case second = <-next:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("cancelled probe did not release ownership")
	}
	if !second.Observe(false, "", 0) {
		t.Fatal("one success prematurely closed two-success recovery")
	}
	rows := s.StormSnapshot()
	if rows[0].RecoverySuccesses != 1 || rows[0].Samples != 3 {
		t.Fatalf("cancel incorrectly counted as a sample: %+v", rows)
	}
	if time.Until(rows[0].RetryAt) < time.Hour-time.Second {
		t.Fatal("partial recovery did not pace the next probe")
	}
	stormDue(s)
	if stormPermit(t, s, "provider.test", "a").Observe(false, "", 0) || len(s.StormSnapshot()) != 0 {
		t.Fatal("successful probes did not recover")
	}
}

func TestStormBackoffRetryHintAndStaleSuccess(t *testing.T) {
	opts := stormTestOptions()
	opts.RecoverySuccesses = 1
	s := newStormTestScheduler(t, opts)
	stale := stormPermit(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	stale.Observe(false, "", 0)
	if len(s.StormSnapshot()) != 1 {
		t.Fatal("pre-trip success closed active storm")
	}
	for _, want := range []time.Duration{2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		stormDue(s)
		stormFail(t, s, "provider.test", "a")
		s.storms.mu.Lock()
		got := s.storms.scopes[stormKey{provider: "provider.test", model: "a", modelScope: true}].backoff
		s.storms.mu.Unlock()
		if got != want {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	}
	stormDue(s)
	stormPermit(t, s, "provider.test", "a").Observe(true, "HTTP 503", 10*24*time.Hour)
	remaining := time.Until(s.StormSnapshot()[0].RetryAt)
	if remaining > maxRetryHint || remaining < maxRetryHint-time.Second {
		t.Fatalf("retry hint was shrunk or unbounded: %v", remaining)
	}
}

func TestStormQueueBoundsCancellationAndTimeout(t *testing.T) {
	opts := stormTestOptions()
	opts.MaxQueue = 1
	opts.MaxWait = 60 * time.Millisecond
	s := newStormTestScheduler(t, opts)
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.WaitStorm(context.Background(), "provider.test", "a", func() { close(waiting) })
		done <- err
	}()
	<-waiting
	if _, err := s.WaitStorm(context.Background(), "provider.test", "a", nil); !errors.Is(err, ErrStormQueueFull) {
		t.Fatalf("queue cap error = %v", err)
	}
	if err := <-done; !errors.Is(err, ErrStormMaxWait) || stormQueued(s) != 0 {
		t.Fatalf("timeout cleanup: %v, queued %d", err, stormQueued(s))
	}
}

func TestStormReloadWakesAndInvalidatesPermits(t *testing.T) {
	opts := stormTestOptions()
	s := newStormTestScheduler(t, opts)
	stale := stormPermit(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	done := make(chan *StormPermit, 1)
	go func() {
		p, _ := s.WaitStorm(context.Background(), "provider.test", "a", nil)
		done <- p
	}()
	waitFor(t, func() bool { return stormQueued(s) == 1 }, "wait before reload")
	opts.PolicyKey = "changed-classification"
	s.UpdateOptions(Options{Storm: opts})
	select {
	case p := <-done:
		if p == nil {
			t.Fatal("reload failed queued request")
		}
		p.Cancel()
	case <-time.After(time.Second):
		t.Fatal("reload did not wake waiter")
	}
	if stale.Observe(true, "HTTP 503", 0) || len(s.StormSnapshot()) != 0 || stormQueued(s) != 0 {
		t.Fatal("stale permit restored cleared storm state")
	}
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	s.UpdateOptions(Options{})
	if len(s.StormSnapshot()) != 0 {
		t.Fatal("disable retained active storm")
	}
}

func TestStormRollingWindowAndScopeCapacity(t *testing.T) {
	opts := stormTestOptions()
	opts.ProviderEnabled = false
	opts.MaxScopes = 1
	s := newStormTestScheduler(t, opts)
	stormFail(t, s, "provider.test", "a")
	if _, err := s.WaitStorm(context.Background(), "provider.test", "b", nil); !errors.Is(err, ErrStormCapacity) {
		t.Fatalf("scope capacity bypassed: %v", err)
	}
	s.storms.mu.Lock()
	scope := s.storms.scopes[stormKey{provider: "provider.test", model: "a", modelScope: true}]
	scope.buckets = [stormBucketCount]stormBucket{}
	scope.sample(time.Now().Add(-2*opts.Window), opts.Window, true)
	s.storms.mu.Unlock()
	stormPermit(t, s, "provider.test", "b").Cancel()
	if len(s.storms.scopes) != 1 {
		t.Fatal("scope eviction exceeded capacity")
	}
	var bucketScope stormScope
	now := time.Now()
	bucketScope.sample(now.Add(-2*opts.Window), opts.Window, true)
	bucketScope.sample(now, opts.Window, false)
	if samples, failed := bucketScope.totals(now, opts.Window); samples != 1 || failed != 0 {
		t.Fatalf("expired sample retained: %d/%d", failed, samples)
	}
	if _, err := s.WaitStorm(context.Background(), "provider.test", strings.Repeat("m", maxStormLabelBytes+1), nil); !errors.Is(err, ErrStormCapacity) {
		t.Fatal("oversized scope retained")
	}
}

func TestStormConcurrentSettleCountsOnce(t *testing.T) {
	opts := stormTestOptions()
	opts.MinSamples = 1000
	s := newStormTestScheduler(t, opts)
	p := stormPermit(t, s, "provider.test", "a")
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { p.Observe(true, "HTTP 503", 0) })
	}
	wg.Wait()
	scope := s.storms.scopes[stormKey{provider: "provider.test", model: "a", modelScope: true}]
	if samples, failures := scope.totals(time.Now(), opts.Window); samples != 1 || failures != 1 || scope.refs != 0 {
		t.Fatalf("permit settled more than once: %d/%d refs=%d", failures, samples, scope.refs)
	}
	if stormAdd(math.MaxInt64, 1) != math.MaxInt64 {
		t.Fatal("sample counter overflow")
	}
}

func TestStormTimerReleasesProbe(t *testing.T) {
	opts := stormTestOptions()
	opts.InitialBackoff = 10 * time.Millisecond
	opts.MaxBackoff = time.Second
	opts.RecoverySuccesses = 1
	s := newStormTestScheduler(t, opts)
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := s.WaitStorm(ctx, "provider.test", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Observe(false, "", 0) {
		t.Fatal("timer-admitted successful probe did not recover")
	}
}

func TestStormProviderRequiresAllActiveModels(t *testing.T) {
	t.Run("only active model", func(t *testing.T) {
		opts := stormTestOptions()
		opts.ProviderEnabled = true
		s := newStormTestScheduler(t, opts)
		stormFail(t, s, "provider.test", "a")
		stormFail(t, s, "provider.test", "a")
		rows := s.StormSnapshot()
		if len(rows) != 2 || rows[0].Scope != "provider" || rows[0].ActiveModels != 1 || rows[0].AffectedModels != 1 {
			t.Fatalf("only active failing model must permit provider scope: %+v", rows)
		}
	})
	t.Run("two of three then idle expires", func(t *testing.T) {
		opts := stormTestOptions()
		opts.ProviderEnabled = true
		s := newStormTestScheduler(t, opts)
		stormPermit(t, s, "provider.test", "a").Cancel()
		stormPermit(t, s, "provider.test", "b").Cancel()
		stormPermit(t, s, "provider.test", "idle").Cancel()
		stormFail(t, s, "provider.test", "a")
		stormFail(t, s, "provider.test", "a")
		stormFail(t, s, "provider.test", "b")
		stormFail(t, s, "provider.test", "b")
		for _, row := range s.StormSnapshot() {
			if row.Scope == "provider" {
				t.Fatal("two of three active models broadened scope")
			}
		}
		s.storms.mu.Lock()
		idle := s.storms.scopes[stormKey{provider: "provider.test", model: "idle", modelScope: true}]
		idle.lastActivity = time.Now().Add(-2 * opts.Window)
		s.storms.mu.Unlock()
		stormDue(s)
		stormFail(t, s, "provider.test", "a")
		rows := s.StormSnapshot()
		if rows[0].Scope != "provider" || rows[0].ActiveModels != 2 || rows[0].AffectedModelPercent != 100 {
			t.Fatalf("inactive model stayed in denominator: %+v", rows)
		}
	})
	t.Run("incoming traffic refreshes activity", func(t *testing.T) {
		opts := stormTestOptions()
		opts.ProviderEnabled = true
		s := newStormTestScheduler(t, opts)
		stormPermit(t, s, "provider.test", "healthy").Cancel()
		s.storms.mu.Lock()
		scope := s.storms.scopes[stormKey{provider: "provider.test", model: "healthy", modelScope: true}]
		scope.lastActivity = time.Now().Add(-2 * opts.Window)
		s.storms.mu.Unlock()
		incoming := stormPermit(t, s, "provider.test", "healthy")
		defer incoming.Cancel()
		stormFail(t, s, "provider.test", "failing")
		stormFail(t, s, "provider.test", "failing")
		rows := s.StormSnapshot()
		if len(rows) != 1 || rows[0].Scope != "model" {
			t.Fatalf("unfinished active request excluded from denominator: %+v", rows)
		}
	})
}

func TestStormOrdinarySamplesDoNotWakeWaiters(t *testing.T) {
	opts := stormTestOptions()
	opts.MinSamples = 3
	s := newStormTestScheduler(t, opts)
	kick := s.storms.kick
	stormPermit(t, s, "provider.test", "a").Observe(false, "", 0)
	stormFail(t, s, "provider.test", "a")
	select {
	case <-kick:
		t.Fatal("sample below threshold woke storm waiters")
	default:
	}
	stormFail(t, s, "provider.test", "a")
	select {
	case <-kick:
	default:
		t.Fatal("new gate did not wake waiters")
	}
}

func TestStormJitterStaysBoundedAndHonorsHint(t *testing.T) {
	opts := stormTestOptions()
	opts.InitialBackoff = time.Second
	opts.MaxBackoff = time.Second
	opts.JitterPercent = 25
	s := newStormTestScheduler(t, opts)
	scope := &stormScope{}
	scope.timer.fn = func() {}
	defer scope.timer.stop()
	now := time.Now()
	for range 100 {
		s.storms.delay(scope, now, 0, false)
		delay := scope.retryAt.Sub(now)
		if delay < 750*time.Millisecond || delay > opts.MaxBackoff {
			t.Fatalf("jitter escaped configured bounds: %v", delay)
		}
	}
	s.storms.delay(scope, now, 10*time.Second, false)
	if scope.retryAt.Sub(now) != 10*time.Second {
		t.Fatal("jitter shrank provider retry hint")
	}
}

func TestStormReadyDoesNotReserveProbeBeforeKeyAdmission(t *testing.T) {
	s := newStormTestScheduler(t, stormTestOptions())
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	stormDue(s)
	if err := s.WaitStormReady(context.Background(), "provider.test", "a", nil); err != nil {
		t.Fatal(err)
	}
	s.storms.mu.Lock()
	scope := s.storms.scopes[stormKey{provider: "provider.test", model: "a", modelScope: true}]
	reserved := scope.probing || scope.refs != 0
	s.storms.mu.Unlock()
	if reserved {
		t.Fatal("readiness reserved a probe before key admission")
	}
	probe := stormPermit(t, s, "provider.test", "a")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := s.WaitStormReady(ctx, "provider.test", "a", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readiness bypassed current send probe: %v", err)
	}
	probe.Cancel()
	s.UpdateOptions(Options{})
	if err := s.WaitStormReady(context.Background(), "provider.test", "a", nil); err != nil {
		t.Fatal(err)
	}
}

func TestStormInflightHintExtendsExistingWindow(t *testing.T) {
	s := newStormTestScheduler(t, stormTestOptions())
	inflight := stormPermit(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	stormFail(t, s, "provider.test", "a")
	inflight.Observe(true, "HTTP 503", 3*time.Hour)
	if remaining := time.Until(s.StormSnapshot()[0].RetryAt); remaining < 3*time.Hour-time.Second {
		t.Fatalf("in-flight sibling hint ignored: %v", remaining)
	}
}

func BenchmarkStormAdmission(b *testing.B) {
	for _, mode := range []string{"disabled", "healthy", "partial-provider"} {
		b.Run(mode, func(b *testing.B) {
			opts := stormTestOptions()
			opts.Enabled = mode != "disabled"
			opts.ProviderEnabled = true
			opts.ModelEnabled = mode != "partial-provider"
			opts.MaxScopes = 512
			s := New(Options{Storm: opts})
			defer s.UpdateOptions(Options{})
			ctx := context.Background()
			for i := range 128 {
				p, err := s.WaitStorm(ctx, "provider.test", fmt.Sprintf("model-%d", i), nil)
				if err != nil {
					b.Fatal(err)
				}
				p.Observe(false, "", 0)
			}
			if mode == "partial-provider" {
				for i := range 127 {
					p, err := s.WaitStorm(ctx, "provider.test", fmt.Sprintf("model-%d", i), nil)
					if err != nil {
						b.Fatal(err)
					}
					p.Observe(true, "HTTP 503", 0)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				p, err := s.WaitStorm(ctx, "provider.test", "model-0", nil)
				if err != nil {
					b.Fatal(err)
				}
				p.Observe(mode == "partial-provider", "HTTP 503", 0)
			}
		})
	}
}

func TestStormDistinctRequestsAcrossRetries(t *testing.T) {
	opts := stormTestOptions()
	opts.ProviderEnabled = true
	s := newStormTestScheduler(t, opts)
	var observation StormObservation
	for range 2 {
		stormPermit(t, s, "provider.test", "a").Observe(true, "HTTP 503", 0, &observation)
	}
	rows := s.StormSnapshot()
	if len(rows) != 2 || rows[0].Failures != 2 || rows[0].ErrorRequests != 1 || rows[1].ErrorRequests != 1 {
		t.Fatalf("retries counted as separate requests: %+v", rows)
	}
	stormDue(s)
	stormPermit(t, s, "provider.test", "a").Observe(true, "HTTP 503", 0, &observation)
	rows = s.StormSnapshot()
	if rows[0].Failures != 3 || rows[0].ErrorRequests != 1 || rows[1].ErrorRequests != 1 {
		t.Fatalf("probe retry duplicated affected request: %+v", rows)
	}
}

func TestStormDistinctRequestMembershipMovesAndExpires(t *testing.T) {
	window := 64 * time.Second
	at := time.Unix(1000, 0)
	var scope stormScope
	var observation, other StormObservation
	observe := func(now time.Time, request *StormObservation) {
		scope.sample(now, window, true)
		scope.observeRequest(now, window, request, 0)
	}
	observe(at, &observation)
	observe(at.Add(500*time.Millisecond), &observation)
	observe(at.Add(2*time.Second), &observation)
	if scope.cachedErrorRequests != 1 || scope.buckets[stormEpoch(at, window)%stormBucketCount].errorRequests != 0 {
		t.Fatal("distinct request did not move to latest failure bucket")
	}
	observe(at.Add(2*time.Second), &other)
	if scope.cachedErrorRequests != 2 {
		t.Fatal("separate failed request was deduplicated")
	}
	scope.totals(at.Add(65*time.Second), window)
	if scope.cachedErrorRequests != 2 {
		t.Fatal("moving request membership expired before its last failure")
	}
	scope.totals(at.Add(67*time.Second), window)
	if scope.cachedErrorRequests != 0 {
		t.Fatal("expired request membership retained")
	}
	observe(at.Add(68*time.Second), &observation)
	if scope.cachedErrorRequests != 1 {
		t.Fatal("failure after window expiry not counted")
	}
	scope.resetSamples()
	observe(at.Add(68*time.Second), &observation)
	if scope.cachedErrorRequests != 1 {
		t.Fatal("recovery reset reused stale request membership")
	}
	var replacement stormScope
	replacement.sample(at.Add(68*time.Second), window, true)
	replacement.observeRequest(at.Add(68*time.Second), window, &observation, 0)
	if replacement.cachedErrorRequests != 1 {
		t.Fatal("configuration replacement reused stale scope membership")
	}
}

func TestStormDistinctObservationDisabledAllocatesNothing(t *testing.T) {
	s := New(Options{})
	var observation StormObservation
	ctx := context.Background()
	allocations := testing.AllocsPerRun(100, func() {
		p, err := s.WaitStorm(ctx, "provider.test", "a", nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Observe(true, "HTTP 503", 0, &observation)
	})
	if allocations != 0 {
		t.Fatalf("disabled distinct observation allocated %v times", allocations)
	}
}
