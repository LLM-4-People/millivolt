// Package scheduler provides a provider-agnostic, FIFO-ordered request queue
// with per-group concurrency caps and adaptive rate-limit pacing. Requests are
// queued transparently when a provider returns 429/503 and retried when the
// provider signals it is ready (Retry-After / rate-limit-reset headers), so
// clients never see a rate-limit failure. While any request is retrying
// (429, 5xx, or transport), WaitSend lets only that owner send. After it
// succeeds or exhausts, exactly one sibling is the probe: a clean
// first-attempt success restores full concurrency; another retry keeps
// the group at one-at-a-time. Consecutive failed requests (exhausted
// retryable or transport) double a request-level backoff (base_backoff,
// cap max_backoff) so the next probe waits; a success resets it.
package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures the scheduler.
type Options struct {
	// MaxConcurrent is the default concurrency cap per group when the provider
	// does not advertise one. 0 means unlimited.
	MaxConcurrent int
	// MaxQueueSize is the maximum number of queued requests per group. 0 means
	// unlimited.
	MaxQueueSize int
	// MaxWait is the maximum time a request may wait in the queue before being
	// rejected with an error. 0 means unlimited.
	MaxWait time.Duration
	// BaseBackoff is the initial adaptive delay. It doubles each attempt
	// whether or not the provider sent a retry hint.
	BaseBackoff time.Duration
	// MaxBackoff caps adaptive exponential backoff. It does not clamp a
	// provider-supplied Retry-After / rate-limit-reset duration - a long
	// hint is still waited in full (see MaxRetryHint). A short hint is a
	// floor on the wait, not a replacement for this cap's doubling.
	MaxBackoff time.Duration
	Storm      StormOptions
}

// maxSchedulerPacingStates bounds the pacing/backoff state retained for groups
// removed by idle eviction. The LRU carries only a recent victim's small
// timing fields, never the group object, queue, provider link or pacer, so it
// cannot recreate the growth the group cap exists to bound.
const maxSchedulerPacingStates = 4_096

// Scheduler manages ordered queues per group key.
//
// Lock order: s.mu -> g.mu, s.mu -> idleMu, s.mu -> providerIndex.mu,
// s.mu -> pacingLRU.mu, and g.mu -> idleMu. A path holding idleMu,
// providerIndex.mu or pacingLRU.mu never waits for s.mu or g.mu, and
// drainProvider releases providerIndex.mu before touching a group.
type Scheduler struct {
	mu     sync.Mutex // guards opts, groups, groupCap, providerGroups and group retirement
	opts   Options
	groups map[string]*group
	// groupCap bounds len(groups). It is maxSchedulerGroups in production;
	// package tests lower it to exercise eviction without creating 100k
	// groups. Guarded by mu.
	groupCap int
	// providerGroups indexes groups by their attributed provider, so a
	// provider throttle release drains only that provider's groups instead of
	// scanning every group. Guarded by mu. A group is linked exactly once, on
	// its first non-empty attribution, and eviction unlinks it under the same
	// lock and retires that group's counter pointer with the entry.
	providerGroups map[string]*providerIndex
	storms         stormState
	// idleMu guards the idle-group LRU (idleHead/idleTail and each group's
	// idle links). A group with no in-flight slot and no queued waiter sits on
	// this list; the head is the oldest eviction candidate. Transition owners
	// add and remove a group while holding its g.mu, and eviction re-verifies
	// idleness under that same g.mu before removing it.
	idleMu   sync.Mutex
	idleHead *group
	idleTail *group
	// pacing carries the pacing/backoff fields of recently evicted idle
	// groups, bounded by maxSchedulerPacingStates.
	pacing pacingLRU

	// policy is the operator hold set (all / new-unseen / named
	// clients / named providers). Read via atomic pointer so drain can
	// skip held waiters without taking pauseMu.
	policy      atomic.Pointer[policySnap]
	pauseMu     sync.Mutex
	kick        chan struct{} // closed and replaced on every policy edge
	holdCountMu sync.Mutex
	holdCount   map[string]int // waiters currently parked, keyed by hold id

	// gates is the per-provider throttle map (concurrency / requests-per-window /
	// tokens-per-window). Keyed by Record.Provider, NOT provider|key - one
	// client setting a header applies across every key of that provider.
	throttleMu sync.Mutex
	gates      map[string]*providerGate
	// gateSnap is the lock-free read view of gates. gate() must never take
	// throttleMu: admission calls it while holding a group's mutex, and the
	// throttle writers take throttleMu -> gate/group mutexes. Reading through
	// an immutable replacement removes that inversion entirely.
	gateSnap atomic.Pointer[map[string]*providerGate]
}

// New builds a Scheduler. All Options are taken as-is - defaults are owned by
// config.Default(), never re-derived here, so there is exactly one source of
// truth and no conflicting fallback values.
func New(opts Options) *Scheduler {
	s := &Scheduler{opts: opts, groups: make(map[string]*group), groupCap: maxSchedulerGroups, providerGroups: make(map[string]*providerIndex), kick: make(chan struct{}), holdCount: make(map[string]int), gates: make(map[string]*providerGate)}
	s.policy.Store(&policySnap{})
	s.publishGatesLocked()
	s.storms.configure(opts.Storm)
	return s
}

// Options returns the scheduler options.
func (s *Scheduler) Options() Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

// UpdateOptions hot-applies new options on a live scheduler (config reload).
// It updates the shared tunables (queue size/wait, backoff bounds) that
// acquire/drain read from s.opts. It deliberately does NOT touch live groups'
// maxConcurrent: that cap is also set per-request via X-Proxy-Max-Concurrency
// (a client override), and the next Acquire for a group passes the fresh config
// value anyway, so resetting here would only clobber a deliberate override.
func (s *Scheduler) UpdateOptions(opts Options) {
	s.mu.Lock()
	s.opts = opts
	s.storms.configure(opts.Storm)
	s.mu.Unlock()
}

// pacingState is the small subset of a group's timing state carried across an
// idle eviction. It never references the group, its queue, its provider or its
// pacer, so retained victims stay cheap and bounded.
type pacingState struct {
	key            string
	nextAllowedAt  time.Time
	backoff        time.Duration
	requestBackoff time.Duration
	tripped        bool
	prev, next     *pacingState
}

// pacingLRU is a fixed-capacity least-recently-used map of pacingState values.
// mu guards every field. It is touched only at group creation and idle
// eviction, never on the admission hot path.
type pacingLRU struct {
	mu     sync.Mutex
	states map[string]*pacingState
	head   *pacingState
	tail   *pacingState
}

// put inserts or replaces key's pacing state and evicts the oldest state past
// the bound.
func (p *pacingLRU) put(key string, st pacingState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states == nil {
		p.states = make(map[string]*pacingState, maxSchedulerPacingStates)
	}
	if old := p.states[key]; old != nil {
		p.unlinkLocked(old)
		delete(p.states, key)
	}
	node := &pacingState{
		key:            key,
		nextAllowedAt:  st.nextAllowedAt,
		backoff:        st.backoff,
		requestBackoff: st.requestBackoff,
		tripped:        st.tripped,
		prev:           p.tail,
	}
	if p.tail != nil {
		p.tail.next = node
	} else {
		p.head = node
	}
	p.tail = node
	p.states[key] = node
	for len(p.states) > maxSchedulerPacingStates && p.head != nil {
		oldest := p.head
		p.unlinkLocked(oldest)
		delete(p.states, oldest.key)
	}
}

// take removes and returns key's pacing state.
func (p *pacingLRU) take(key string) (pacingState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	node := p.states[key]
	if node == nil {
		return pacingState{}, false
	}
	p.unlinkLocked(node)
	delete(p.states, key)
	return pacingState{
		nextAllowedAt:  node.nextAllowedAt,
		backoff:        node.backoff,
		requestBackoff: node.requestBackoff,
		tripped:        node.tripped,
	}, true
}

func (p *pacingLRU) unlinkLocked(n *pacingState) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		p.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		p.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

// adoptPacingLocked copies a recently evicted group's pacing state into a
// freshly created group and restores its pacer deadline. The group is not
// published yet, so no group lock is needed. Callers hold s.mu.
func (s *Scheduler) adoptPacingLocked(g *group) {
	st, ok := s.pacing.take(g.key)
	if !ok {
		return
	}
	g.nextAllowedAt = st.nextAllowedAt
	g.backoff = st.backoff
	g.requestBackoff = st.requestBackoff
	g.tripped = st.tripped
	if g.nextAllowedAt.After(time.Now()) {
		g.pacer.arm(g.nextAllowedAt, false)
	}
}

// Stats is a point-in-time snapshot of scheduler occupancy.
type Stats struct {
	Paused   bool `json:"paused"`
	InFlight int  `json:"in_flight"`
	Queued   int  `json:"queued"`
}

// Stats sums in-flight slots and queued waiters across every group.
func (s *Scheduler) Stats() Stats {
	st := Stats{Paused: s.Paused()}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		g.mu.Lock()
		st.InFlight += g.inFlight
		st.Queued += g.queued
		g.mu.Unlock()
	}
	return st
}

// snapshotGroups copies the current group set under s.mu and releases the
// lock before the caller walks it, so per-group work never nests the
// scheduler lock. Stats deliberately keeps its own nested-lock protocol: it
// sums in-flight and queue counters inside s.mu.
func (s *Scheduler) snapshotGroups() []*group {
	s.mu.Lock()
	groups := make([]*group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	s.mu.Unlock()
	return groups
}

func (s *Scheduler) drainAll() {
	for _, g := range s.snapshotGroups() {
		g.drain()
	}
}

func (s *Scheduler) kickC() <-chan struct{} {
	s.pauseMu.Lock()
	ch := s.kick
	s.pauseMu.Unlock()
	return ch
}

// Trip records a retryable failure (429, 5xx, or transport): extends the
// pacing window and claims the send token if nobody else has it. The
// caller who gets true owns every subsequent send until EndSend - siblings
// WaitSend until that request succeeds or exhausts.
func (s *Scheduler) Trip(key string, d time.Duration) bool {
	return s.extendWindow(key, d, true)
}

func (s *Scheduler) extendWindow(key string, d time.Duration, claim bool) bool {
	s.mu.Lock()
	g := s.groupForLocked(key, 0, false)
	g.mu.Lock()
	owned := false
	// A non-positive duration still trips below so siblings queue even when
	// the owner can retry immediately; the pacing window itself is guarded
	// by armPacingLaterLocked (fail closed).
	g.armPacingLaterLocked(d)
	if d > 0 || claim {
		g.tripped = true
		g.kickSendLocked()
	}
	if claim && !g.probing {
		g.probing = true
		owned = true
	}
	s.mu.Unlock()
	g.mu.Unlock()
	return owned
}

func (g *group) sendKickC() <-chan struct{} {
	g.mu.Lock()
	ch := g.sendKick
	g.mu.Unlock()
	return ch
}

func (g *group) kickSend() {
	g.mu.Lock()
	g.kickSendLocked()
	g.mu.Unlock()
}

func (g *group) kickSendLocked() {
	if g.sendKick == nil {
		g.sendKick = make(chan struct{})
		return
	}
	close(g.sendKick)
	g.sendKick = make(chan struct{})
}

// armPacingLaterLocked pushes the group's pacing window out by d when that
// lands after the current allowance, arming the pacer; an earlier existing
// window is never shrunk. A non-positive d is a no-op: it must never erase an
// existing pacing window (fail closed). Callers hold g.mu.
func (g *group) armPacingLaterLocked(d time.Duration) {
	if d > 0 {
		until := time.Now().Add(d)
		if until.After(g.nextAllowedAt) {
			g.nextAllowedAt = until
			g.pacer.arm(until, false)
		}
	}
}

// waitKickOrDone parks a WaitSend caller on its freshly snapshotted wake
// channels until a group kick, a send kick, or context cancellation; the
// cancellation is returned so the caller can return its retry token.
func waitKickOrDone(ctx context.Context, kick, sendKick <-chan struct{}) error {
	select {
	case <-kick:
	case <-sendKick:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// WaitSend blocks until this caller may hit upstream. honorHold parks on an
// operator pause (retries - a new send). First-attempt callers pass false so
// an already-acquired in-flight request still finishes. own is the token
// from Trip: the retrying request keeps sending; everyone else waits until
// EndSend. The bool is the token to pass back in (true if this caller now
// owns the retry). nextAllowedAt is absolute wall clock - pause does not
// reset it; after unpause the remaining time is waited.
func (s *Scheduler) WaitSend(ctx context.Context, key string, hooks WaiterHooks, honorHold, own bool) (bool, error) {
	g := s.groupFor(key, 0)
	holding := false
	defer func() {
		if holding && hooks.OnUnhold != nil {
			hooks.OnUnhold()
		}
	}()
	for {
		// Snapshot both kick channels before the predicates (lost-wakeup).
		kick := s.kickC()
		sendKick := g.sendKickC()

		if honorHold {
			if h := s.matchingHold(hooks.Client, hooks.Provider); h != nil {
				if !holding {
					if hooks.OnWait != nil {
						hooks.OnWait()
					}
					if err := ctx.Err(); err != nil {
						return own, err
					}
					// A slow client write may outlive the hold. Do not publish
					// a stale paused edge after the operator resumed it.
					if s.matchingHold(hooks.Client, hooks.Provider) == nil {
						continue
					}
					holding = true
					if hooks.OnHold != nil {
						hooks.OnHold()
					}
				}
				var untilTimer *time.Timer
				var untilC <-chan time.Time
				if !h.until.IsZero() {
					d := time.Until(h.until)
					if d <= 0 {
						continue
					}
					untilTimer = time.NewTimer(d)
					untilC = untilTimer.C
				}
				select {
				case <-untilC:
				case <-kick:
				case <-sendKick:
				case <-ctx.Done():
					if untilTimer != nil {
						untilTimer.Stop()
					}
					return own, ctx.Err()
				}
				if untilTimer != nil {
					untilTimer.Stop()
				}
				continue
			}
		}
		if holding {
			holding = false
			if hooks.OnUnhold != nil {
				hooks.OnUnhold()
			}
		}

		g.mu.Lock()
		now := time.Now()
		if now.Before(g.nextAllowedAt) {
			g.mu.Unlock()
			if err := waitKickOrDone(ctx, kick, sendKick); err != nil {
				return own, err
			}
			continue
		}
		if own || !g.tripped {
			g.mu.Unlock()
			return own, nil
		}
		if g.probing {
			g.mu.Unlock()
			if err := waitKickOrDone(ctx, kick, sendKick); err != nil {
				return own, err
			}
			continue
		}
		g.probing = true
		g.mu.Unlock()
		return true, nil
	}
}

// EndSend releases the retry token after a non-failure outcome (success
// or client abort). keepSerial (the request retried) leaves the group
// half-open: exactly one waiter may send next. A clean first-attempt
// success passes keepSerial=false and restores full concurrency.
// Consecutive-request backoff is reset so the next probe is immediate.
func (s *Scheduler) EndSend(key string, keepSerial bool) {
	s.mu.Lock()
	g, ok := s.groups[key]
	s.mu.Unlock()
	if !ok {
		return
	}
	g.mu.Lock()
	g.probing = false
	g.backoff = 0
	g.requestBackoff = 0
	g.nextAllowedAt = time.Time{}
	g.pacer.stop()
	if keepSerial {
		g.tripped = true
	} else {
		g.tripped = false
	}
	g.kickSendLocked()
	g.mu.Unlock()
	g.drain()
}

// backoffMultiplier is the exponential factor for both attempt-level
// (BackoffFor) and request-level (FailSend) backoff. "Double" is the
// algorithm, with a fixed jitter band rather than an operator setting.
const (
	backoffMultiplier   = 2
	backoffJitterSpread = 0.5  // width of the jitter band
	backoffJitterFloor  = 0.75 // 0.75x–1.25x
)

func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (rand.Float64()*backoffJitterSpread + backoffJitterFloor))
}

func growBackoff(cur, base, cap time.Duration) time.Duration {
	if cur <= 0 {
		cur = base
	} else {
		cur *= backoffMultiplier
	}
	if cap > 0 && cur > cap {
		cur = cap
	}
	return cur
}

// FailSend records that this request's final outcome was a retryable
// failure (exhausted 429/5xx or retryable transport). It doubles the
// consecutive-request backoff - start at base_backoff, double each
// failed request, cap at max_backoff (same config pair as attempt
// backoff) - and paces the next probe. Retry-After already on the
// group is not shrunk. own releases the send token; concurrency
// stays at one.
func (s *Scheduler) FailSend(key string, own bool) {
	s.mu.Lock()
	g := s.groupForLocked(key, 0, false)
	opts := s.opts
	s.mu.Unlock()
	g.mu.Lock()
	if own {
		g.probing = false
		// Only the owner is done. A sibling first-attempt exhaust must not
		// wipe an in-flight retry's attempt streak (g.backoff is shared).
		g.backoff = 0
	}
	g.requestBackoff = growBackoff(g.requestBackoff, opts.BaseBackoff, opts.MaxBackoff)
	d := jitterBackoff(g.requestBackoff)
	g.armPacingLaterLocked(d)
	g.tripped = true
	g.kickSendLocked()
	g.mu.Unlock()
	g.drain()
}

func (g *group) kickC() <-chan struct{} {
	if g.sched == nil {
		return nil
	}
	return g.sched.kickC()
}

// MaxRetryHint is the ceiling on a provider-supplied Retry-After /
// rate-limit-reset duration. Daily quota windows routinely last tens of
// minutes and can run until UTC midnight (~24h). This is a safety guardrail
// only - a hostile or buggy upstream (Retry-After: 99999999) must not stall
// the request and the whole provider+key group forever. It is independent of
// MaxBackoff, which caps adaptive exponential backoff. A long hint may
// outlast that cap; a short hint never replaces it. The proxy's
// parseRetryAfter bounds every stored hint to this ceiling, so a wrapped or
// non-finite upstream value can never reach record fields or the scheduler.
const MaxRetryHint = 24 * time.Hour

// ClampRetryHint bounds a retry hint to [0, MaxRetryHint]: a negative (past
// HTTP-date or negative duration) means "retry now", and a value above the
// ceiling is pathological. It is the single clamp owner for every stored or
// compared hint - the proxy's parseRetryAfter decode paths, BackoffFor's
// floor compare, and the storm delay/settle paths, which must clamp BEFORE
// their sibling-window compare so a hostile or overflowed hint can never
// extend a storm window past the ceiling.
func ClampRetryHint(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d > MaxRetryHint:
		return MaxRetryHint
	default:
		return d
	}
}

// BackoffFor computes the pacing delay after a retryable failure. Adaptive
// exponential backoff always grows (base_backoff, cap max_backoff). A
// provider Retry-After / rate-limit-reset is a floor: the wait is the
// longer of the hint and the grown adaptive delay, so a 1s "retry shortly"
// cannot reset the doubling and re-hammer the upstream, while a 35m daily
// limit is still waited in full. Pathological hints are clamped to
// MaxRetryHint. Jitter applies to the adaptive delay only.
func (s *Scheduler) BackoffFor(key string, retryAfter time.Duration) time.Duration {
	s.mu.Lock()
	g := s.groupForLocked(key, 0, false)
	opts := s.opts // read under s.mu (UpdateOptions mutates it)
	s.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.backoff = growBackoff(g.backoff, opts.BaseBackoff, opts.MaxBackoff)
	// Jitter: 0.75x–1.25x to avoid thundering herd. Do not write
	// nextAllowedAt here - only Trip/FailSend trip the group.
	adaptive := jitterBackoff(g.backoff)
	retryAfter = ClampRetryHint(retryAfter)
	if retryAfter > adaptive {
		return retryAfter
	}
	return adaptive
}

var (
	// ErrQueueFull is returned when the group queue or a pause's MaxQueued
	// cap is exhausted. The proxy maps this to HTTP 429.
	ErrQueueFull = errorString("queue full")
	// ErrGroupLimit is returned when a brand-new group would exceed the
	// process-wide group bound. Existing keys are unaffected.
	ErrGroupLimit = errorString("scheduler group limit reached")
	// ErrOverlap is returned when AddHold / ReplaceHold would
	// park a target already covered by another pause.
	ErrOverlap = errorString("pause overlaps existing hold")
	// ErrHoldNotFound is returned when ReplaceHold targets an unknown id.
	ErrHoldNotFound = errorString("pause hold not found")
	errMaxWait      = errorString("queue wait exceeded")
	// errGroupRetired tells AcquireWith that its looked-up group was evicted
	// as idle before admission; the acquisition retries against the map.
	errGroupRetired = errorString("scheduler group retired")
)

type errorString string

func (e errorString) Error() string { return string(e) }
