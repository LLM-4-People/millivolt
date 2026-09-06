package scheduler

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// StormOptions configures attempt-based outage detection and send admission.
// Defaults and validation belong to config; zero options disable this feature.
type StormOptions struct {
	// PolicyKey invalidates samples when proxy-owned failure classification changes.
	PolicyKey                                           string
	Enabled, ProviderEnabled, ModelEnabled              bool
	Window                                              time.Duration
	MinSamples, ErrorPercent                            int
	InitialBackoff, MaxBackoff                          time.Duration
	BackoffMultiplier, JitterPercent, RecoverySuccesses int
	MaxQueue, MaxScopes                                 int
	MaxWait                                             time.Duration
}

// Fixed buckets bound work and memory independently of traffic volume. Windows
// have a resolution of approximately Window/64 and exclude the oldest partial
// bucket. Label bounds prevent arbitrary request metadata retaining huge keys.
const (
	stormBucketCount    = 64
	maxStormLabelBytes  = 512
	maxStormReasonBytes = 96
)

var (
	ErrStormQueueFull = errorString("error storm queue full")
	ErrStormMaxWait   = errorString("error storm queue wait exceeded")
	ErrStormCapacity  = errorString("error storm scope capacity exceeded")
)

type stormKey struct {
	provider   string
	model      string
	modelScope bool
}

type stormBucket struct {
	epoch             int64
	samples, failures int64
	errorRequests     int64
}

type stormScope struct {
	key                           stormKey
	buckets                       [stormBucketCount]stormBucket
	cachedEpoch                   int64
	cachedSamples, cachedFailures int64
	cachedErrorRequests           int64
	reset                         uint64
	lastActivity                  time.Time
	active, probing               bool
	epoch                         uint64
	backoff                       time.Duration
	retryAt                       time.Time
	recovered, queued, refs       int
	reason                        string
	timer                         wakeTimer
}

type stormState struct {
	mu         sync.Mutex
	opts       StormOptions
	scopes     map[stormKey]*stormScope
	kick       chan struct{}
	generation uint64
	queued     int
}

func (s *stormState) configure(opts StormOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kick != nil && s.opts == opts {
		return
	}
	for _, scope := range s.scopes {
		scope.timer.stop()
	}
	s.opts = opts
	s.scopes = make(map[stormKey]*stormScope)
	s.generation++
	s.wakeLocked()
}

func (s *stormState) wakeLocked() {
	if s.kick != nil {
		close(s.kick)
	}
	s.kick = make(chan struct{})
}

func (s *stormState) wake() {
	s.mu.Lock()
	s.wakeLocked()
	s.mu.Unlock()
}

// scopesFor reserves both rolling observations together. Active storms and
// referenced scopes cannot be evicted to silently bypass a live admission gate.
func (s *stormState) scopesFor(provider, model string, now time.Time) ([]*stormScope, error) {
	if len(provider) > maxStormLabelBytes || len(model) > maxStormLabelBytes {
		return nil, ErrStormCapacity
	}
	keys := []stormKey{{provider: provider, model: model, modelScope: true}}
	if s.opts.ProviderEnabled {
		keys = append(keys, stormKey{provider: provider})
	}
	needed := 0
	for _, key := range keys {
		if s.scopes[key] == nil {
			needed++
		}
	}
	if len(s.scopes)+needed > s.opts.MaxScopes {
		for key, scope := range s.scopes {
			if !scope.active && scope.refs == 0 && scope.queued == 0 && now.Sub(scope.lastActivity) >= s.opts.Window {
				// Keep the requested scopes even when they have no observations.
				requested := false
				for _, target := range keys {
					requested = requested || key == target
				}
				if !requested {
					scope.timer.stop()
					delete(s.scopes, key)
				}
			}
		}
		if len(s.scopes)+needed > s.opts.MaxScopes {
			return nil, ErrStormCapacity
		}
	}
	scopes := make([]*stormScope, 0, len(keys))
	for _, key := range keys {
		scope := s.scopes[key]
		if scope == nil {
			// A short substring must not retain an arbitrarily large request
			// or URL backing string for the lifetime of this scope.
			key.provider = strings.Clone(key.provider)
			key.model = strings.Clone(key.model)
			scope = &stormScope{key: key, lastActivity: now}
			scope.timer.fn = s.wake
			s.scopes[key] = scope
		}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

func (s *stormState) gates(scope *stormScope) bool {
	return !scope.key.modelScope || s.opts.ModelEnabled
}

type stormClaim struct {
	scope *stormScope
	epoch uint64
	probe bool
}

type stormObservationSlot struct {
	scope *stormScope
	reset uint64
	epoch int64
}

// StormObservation belongs to one logical client request on one Scheduler.
// Reuse it across that request's retry attempts to count distinct affected
// requests without retaining request identifiers or an unbounded membership map.
// The scheduler mutex protects its fixed provider/model membership slots.
type StormObservation struct {
	slots [2]stormObservationSlot
}

// StormPermit owns one pending upstream attempt, including any half-open probe.
// Observe or Cancel must settle it. Both methods are idempotent and concurrent
// safe. Old permits cannot recover a storm opened or reconfigured after grant.
type StormPermit struct {
	state      *stormState
	generation uint64
	claims     [2]stormClaim
	claimCount int
	once       sync.Once
	active     bool
}

var disabledStormPermit StormPermit

// WaitStorm holds the caller before a new upstream attempt, across credentials.
// It claims every relevant due probe atomically, so overlapping provider/model
// storms cannot each admit a competing probe. The caller's context remains the
// lifetime owner; per-send upstream deadlines must start after admission.
func (s *Scheduler) WaitStorm(ctx context.Context, provider, model string, onWait func()) (*StormPermit, error) {
	return s.waitStorm(ctx, provider, model, onWait, true)
}

// WaitStormReady holds new work before ordinary key admission without owning a
// probe. Reserving a probe before that admission would deadlock if its key slot
// were held by a retry waiting for the same probe. Actual sends must WaitStorm
// again after acquiring their ordinary key slot and send token.
func (s *Scheduler) WaitStormReady(ctx context.Context, provider, model string, onWait func()) error {
	_, err := s.waitStorm(ctx, provider, model, onWait, false)
	return err
}

func (s *Scheduler) waitStorm(ctx context.Context, provider, model string, onWait func(), reserve bool) (*StormPermit, error) {
	state := &s.storms
	var queued []*stormScope
	var timer *time.Timer
	var timeout <-chan time.Time
	waitStarted := time.Time{}
	var activityGeneration uint64
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		if queued != nil {
			state.mu.Lock()
			state.unqueue(queued)
			state.mu.Unlock()
		}
	}()
	for {
		state.mu.Lock()
		if queued != nil {
			state.unqueue(queued)
			queued = nil
		}
		if err := ctx.Err(); err != nil {
			state.mu.Unlock()
			return nil, err
		}
		opts := state.opts
		if !opts.Enabled || (!opts.ProviderEnabled && !opts.ModelEnabled) {
			state.mu.Unlock()
			return &disabledStormPermit, nil
		}
		now := time.Now()
		if !waitStarted.IsZero() && opts.MaxWait > 0 && now.Sub(waitStarted) >= opts.MaxWait {
			state.mu.Unlock()
			return nil, ErrStormMaxWait
		}
		scopes, err := state.scopesFor(provider, model, now)
		if err != nil {
			state.mu.Unlock()
			return nil, err
		}
		if activityGeneration != state.generation {
			for _, scope := range scopes {
				scope.lastActivity = now
			}
			activityGeneration = state.generation
		}
		blocked := false
		for _, scope := range scopes {
			if state.gates(scope) && scope.active && (scope.probing || now.Before(scope.retryAt)) {
				blocked = true
			}
		}
		if !blocked {
			if !reserve {
				state.mu.Unlock()
				return nil, nil
			}
			permit := &StormPermit{state: state, generation: state.generation}
			for _, scope := range scopes {
				probe := state.gates(scope) && scope.active
				if probe {
					scope.probing = true
				}
				scope.refs++
				permit.claims[permit.claimCount] = stormClaim{scope, scope.epoch, probe}
				permit.claimCount++
			}
			state.mu.Unlock()
			return permit, nil
		}
		if state.queued >= opts.MaxQueue {
			state.mu.Unlock()
			return nil, ErrStormQueueFull
		}
		state.queued++
		queued = scopes
		for _, scope := range scopes {
			scope.queued++
		}
		kick := state.kick
		firstWait := waitStarted.IsZero()
		if firstWait {
			waitStarted = now
		}
		if timer != nil {
			timer.Stop()
		}
		if opts.MaxWait > 0 {
			timer = time.NewTimer(time.Until(waitStarted.Add(opts.MaxWait)))
			timeout = timer.C
		} else {
			timeout = nil
		}
		state.mu.Unlock()
		if firstWait && onWait != nil {
			onWait()
		}
		select {
		case <-kick:
		case <-timeout:
			return nil, ErrStormMaxWait
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *stormState) unqueue(scopes []*stormScope) {
	s.queued--
	for _, scope := range scopes {
		scope.queued--
	}
}

func stormEpoch(now time.Time, window time.Duration) int64 {
	width := window / stormBucketCount
	if width < time.Nanosecond {
		width = time.Nanosecond
	}
	return now.UnixNano() / int64(width)
}

func stormAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func (s *stormScope) totals(now time.Time, window time.Duration) (samples, failures int64) {
	epoch := stormEpoch(now, window)
	if s.cachedEpoch == epoch {
		return s.cachedSamples, s.cachedFailures
	}
	var errorRequests int64
	for _, bucket := range s.buckets {
		if bucket.epoch > epoch-stormBucketCount && bucket.epoch <= epoch {
			samples = stormAdd(samples, bucket.samples)
			failures = stormAdd(failures, bucket.failures)
			errorRequests = stormAdd(errorRequests, bucket.errorRequests)
		}
	}
	s.cachedEpoch, s.cachedSamples, s.cachedFailures = epoch, samples, failures
	s.cachedErrorRequests = errorRequests
	return
}

func (s *stormScope) observeRequest(now time.Time, window time.Duration, observation *StormObservation, index int) {
	epoch := stormEpoch(now, window)
	if observation != nil {
		slot := &observation.slots[index]
		if slot.scope == s && slot.reset == s.reset {
			if slot.epoch == epoch {
				return
			}
			previous := &s.buckets[slot.epoch%stormBucketCount]
			if slot.epoch > epoch-stormBucketCount && previous.epoch == slot.epoch && previous.errorRequests > 0 {
				previous.errorRequests--
				if s.cachedErrorRequests > 0 {
					s.cachedErrorRequests--
				}
			}
		}
		*slot = stormObservationSlot{scope: s, reset: s.reset, epoch: epoch}
	}
	bucket := &s.buckets[epoch%stormBucketCount]
	bucket.errorRequests = stormAdd(bucket.errorRequests, 1)
	s.cachedErrorRequests = stormAdd(s.cachedErrorRequests, 1)
}

func (s *stormScope) resetSamples() {
	s.buckets = [stormBucketCount]stormBucket{}
	s.cachedEpoch, s.cachedSamples, s.cachedFailures, s.cachedErrorRequests = 0, 0, 0, 0
	s.reset++
}

func (s *stormScope) sample(now time.Time, window time.Duration, failed bool) {
	// Expire buckets once per time slice. Provider-wide checks can then test
	// active model thresholds without scanning 64 buckets per model/send.
	s.totals(now, window)
	epoch := stormEpoch(now, window)
	bucket := &s.buckets[epoch%stormBucketCount]
	if bucket.epoch != epoch {
		*bucket = stormBucket{epoch: epoch}
	}
	bucket.samples = stormAdd(bucket.samples, 1)
	s.cachedSamples = stormAdd(s.cachedSamples, 1)
	if failed {
		bucket.failures = stormAdd(bucket.failures, 1)
		s.cachedFailures = stormAdd(s.cachedFailures, 1)
	}
	s.lastActivity = now
}

func (s *stormScope) threshold(now time.Time, opts StormOptions) bool {
	samples, failures := s.totals(now, opts.Window)
	return samples >= int64(opts.MinSamples) && samples > 0 && failures > 0 && 100*float64(failures)/float64(samples) >= float64(opts.ErrorPercent)
}

func (s *stormState) affectedModels(provider string, now time.Time) (active, affected int) {
	for key, scope := range s.scopes {
		if key.modelScope && key.provider == provider && now.Sub(scope.lastActivity) < s.opts.Window {
			active++
			if scope.threshold(now, s.opts) {
				affected++
			}
		}
	}
	return
}

func (s *stormState) allActiveModelsAffected(provider string, now time.Time) bool {
	active, affected := s.affectedModels(provider, now)
	return active > 0 && active == affected
}

func (s *stormState) delay(scope *stormScope, now time.Time, retryAfter time.Duration, grow bool) {
	opts := s.opts
	if !grow || scope.backoff <= 0 {
		scope.backoff = opts.InitialBackoff
	} else if opts.BackoffMultiplier > 0 && scope.backoff <= opts.MaxBackoff/time.Duration(opts.BackoffMultiplier) {
		scope.backoff *= time.Duration(opts.BackoffMultiplier)
	} else {
		scope.backoff = opts.MaxBackoff
	}
	if scope.backoff > opts.MaxBackoff {
		scope.backoff = opts.MaxBackoff
	}
	delay := scope.backoff
	if opts.JitterPercent > 0 {
		factor := 1 + (2*rand.Float64()-1)*float64(opts.JitterPercent)/100
		value := float64(delay) * factor
		if value >= float64(opts.MaxBackoff) {
			delay = opts.MaxBackoff
		} else if value > 0 {
			delay = time.Duration(value)
		} else {
			delay = 0
		}
	}
	if retryAfter > maxRetryHint {
		retryAfter = maxRetryHint
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	until := now.Add(delay)
	if until.After(scope.retryAt) {
		scope.retryAt = until
	}
	scope.timer.arm(scope.retryAt, false)
}

// Observe records exactly one eligible attempt. failed=false is a clean
// success, not cancellation, invalid input, or permanent account failure.
// The return value says whether either relevant gate remains active.
func (p *StormPermit) Observe(failed bool, reason string, retryAfter time.Duration, observations ...*StormObservation) bool {
	if p == nil || p.state == nil {
		return false
	}
	var observation *StormObservation
	if len(observations) > 0 {
		observation = observations[0]
	}
	p.once.Do(func() { p.settle(true, failed, reason, retryAfter, observation) })
	return p.active
}

// Cancel releases a probe without a health sample or a recovery success.
func (p *StormPermit) Cancel() {
	if p != nil && p.state != nil {
		p.once.Do(func() { p.settle(false, false, "", 0, nil) })
	}
}

func (p *StormPermit) settle(observe, failed bool, reason string, retryAfter time.Duration, observation *StormObservation) {
	if p.state == nil {
		return
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, claim := range p.claims[:p.claimCount] {
		claim.scope.refs--
	}
	if p.generation != s.generation {
		return
	}
	now := time.Now()
	changed := false
	reason = strings.TrimSpace(reason)
	if len(reason) > maxStormReasonBytes {
		reason = reason[:maxStormReasonBytes]
	}
	if retryAfter > maxRetryHint {
		retryAfter = maxRetryHint
	}
	// First record both samples. Provider promotion must see the current
	// model's observation, independent of the order in which gates settle.
	if observe {
		for i, claim := range p.claims[:p.claimCount] {
			claim.scope.sample(now, s.opts.Window, failed)
			if failed {
				claim.scope.observeRequest(now, s.opts.Window, observation, i)
			}
		}
	}
	for _, claim := range p.claims[:p.claimCount] {
		scope := claim.scope
		if !s.gates(scope) {
			continue
		}
		if claim.probe && claim.epoch == scope.epoch && scope.active {
			scope.probing = false
			changed = true
			if observe && failed {
				scope.recovered = 0
				scope.reason = strings.Clone(reason)
				s.delay(scope, now, retryAfter, true)
			} else if observe {
				scope.recovered++
				if scope.recovered >= s.opts.RecoverySuccesses {
					scope.active = false
					scope.backoff = 0
					scope.retryAt = time.Time{}
					scope.resetSamples()
					scope.timer.stop()
				} else {
					// Recovery remains paced until enough clean probes confirm
					// health; partial recovery must not release a burst.
					s.delay(scope, now, 0, false)
				}
			}
		} else if observe && failed && !scope.active && scope.threshold(now, s.opts) && (scope.key.modelScope || s.allActiveModelsAffected(scope.key.provider, now)) {
			scope.active = true
			scope.epoch++
			scope.recovered = 0
			scope.reason = strings.Clone(reason)
			s.delay(scope, now, retryAfter, false)
			changed = true
		} else if observe && failed && scope.active && retryAfter > 0 && now.Add(retryAfter).After(scope.retryAt) {
			// Sibling sends already in flight may announce a longer window.
			// Preserve probe ownership while extending their shared hint.
			scope.retryAt = now.Add(retryAfter)
			scope.reason = strings.Clone(reason)
			scope.timer.arm(scope.retryAt, false)
			changed = true
		}
		p.active = p.active || scope.active
	}
	if changed {
		s.wakeLocked()
	}
}

// StormStatus describes an active gate with attempt-based rolling statistics.
type StormStatus struct {
	Provider             string    `json:"provider"`
	Model                string    `json:"model"`
	Scope                string    `json:"scope"`
	State                string    `json:"state"`
	Reason               string    `json:"reason"`
	ErrorPercent         float64   `json:"error_percent"`
	Failures             int64     `json:"failures"`
	ErrorRequests        int64     `json:"error_requests"`
	Samples              int64     `json:"samples"`
	WindowMs             int64     `json:"window_ms"`
	Queued               int       `json:"queued"`
	RetryAt              time.Time `json:"retry_at"`
	RecoverySuccesses    int       `json:"recovery_successes"`
	RecoveryRequired     int       `json:"recovery_required"`
	ActiveModels         int       `json:"active_models"`
	AffectedModels       int       `json:"affected_models"`
	AffectedModelPercent float64   `json:"affected_model_percent"`
}

func (s *Scheduler) StormSnapshot() []StormStatus {
	state := &s.storms
	state.mu.Lock()
	defer state.mu.Unlock()
	result := make([]StormStatus, 0)
	now := time.Now()
	// Snapshot assembly visits model evidence once. Scanning every scope for
	// every provider would make an administrative poll quadratic under lock.
	type modelCounts struct{ active, affected int }
	counts := make(map[string]modelCounts)
	for key, scope := range state.scopes {
		if key.modelScope && now.Sub(scope.lastActivity) < state.opts.Window {
			count := counts[key.provider]
			count.active++
			if scope.threshold(now, state.opts) {
				count.affected++
			}
			counts[key.provider] = count
		}
	}
	for _, scope := range state.scopes {
		if !scope.active || !state.gates(scope) {
			continue
		}
		samples, failures := scope.totals(now, state.opts.Window)
		row := StormStatus{Provider: scope.key.provider, Model: scope.key.model, Scope: "provider", State: "open", Reason: scope.reason,
			Failures: failures, ErrorRequests: scope.cachedErrorRequests, Samples: samples, WindowMs: state.opts.Window.Milliseconds(), Queued: scope.queued,
			RetryAt: scope.retryAt, RecoverySuccesses: scope.recovered, RecoveryRequired: state.opts.RecoverySuccesses}
		if scope.key.modelScope {
			row.Scope = "model"
		} else {
			count := counts[scope.key.provider]
			row.ActiveModels, row.AffectedModels = count.active, count.affected
			if row.ActiveModels > 0 {
				row.AffectedModelPercent = 100 * float64(row.AffectedModels) / float64(row.ActiveModels)
			}
		}
		if scope.probing || !now.Before(scope.retryAt) {
			row.State = "half_open"
		}
		if samples > 0 {
			row.ErrorPercent = 100 * float64(failures) / float64(samples)
		}
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		if result[i].Scope != result[j].Scope {
			return result[i].Scope == "provider"
		}
		return result[i].Model < result[j].Model
	})
	return result
}
