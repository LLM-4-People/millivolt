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
	"slices"
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
	// BaseBackoff is the initial backoff when a rate-limit response carries no
	// retry hint.
	BaseBackoff time.Duration
	// MaxBackoff caps adaptive exponential backoff when the provider sends no
	// retry hint. It does not clamp a provider-supplied Retry-After /
	// rate-limit-reset duration - those are honored as-is (see maxRetryHint).
	MaxBackoff time.Duration
}

// Scheduler manages ordered queues per group key.
type Scheduler struct {
	mu     sync.Mutex // guards opts and groups
	opts   Options
	groups map[string]*group

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
}

// New builds a Scheduler. All Options are taken as-is - defaults are owned by
// config.Default(), never re-derived here, so there is exactly one source of
// truth and no conflicting fallback values.
func New(opts Options) *Scheduler {
	s := &Scheduler{opts: opts, groups: make(map[string]*group), kick: make(chan struct{}), holdCount: make(map[string]int), gates: make(map[string]*providerGate)}
	s.policy.Store(&policySnap{})
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
	defer s.mu.Unlock()
	s.opts = opts
}

// group is a single FIFO queue with concurrency and pacing state.
type group struct {
	sched          *Scheduler
	provider       string // Record.Provider; set from WaiterHooks, used to drain throttles
	maxConcurrent  int
	inFlight       int
	nextAllowedAt  time.Time     // do not send before this time (after rate limit)
	backoff        time.Duration // attempt-level (BackoffFor, no hint)
	requestBackoff time.Duration // consecutive failed requests (FailSend)
	tripped        bool          // a request is retrying; siblings must WaitSend
	probing        bool          // the retrying request holds the send token
	sendKick       chan struct{} // closed and replaced when the send gate changes
	pacer          wakeTimer     // one shared pacing wakeup, not one per waiter
	queue          []*waiter
	mu             sync.Mutex
}

type waiter struct {
	ch        chan struct{}
	enqueued  time.Time
	ctx       context.Context // for detecting already-cancelled waiters at grant time
	pausedAt  time.Time       // when the current hold began while this waiter was queued
	client    string
	provider  string
	holdID    string
	holding   bool
	onHold    func()
	onUnhold  func()
	estTokens int64
	admission admission // exact provider/token owners captured at grant
}

// WaiterHooks is optional Acquire context: which client/provider this caller
// is, and live-dashboard callbacks when the operator hold starts or lifts.
// OnWait signals a known wait outside scheduler locks: once per Acquire
// enqueue, and once when WaitSend enters an operator hold. It may commit SSE
// headers; it never fires for a fast Acquire or an already-full queue.
// OnHold/OnUnhold are lifecycle-only: non-blocking, no I/O or scheduler re-entry.
// OnThrottle / OnUnthrottle bookend provider-cap waits. Release owns usage.
type WaiterHooks struct {
	Client       string
	Provider     string
	EstTokens    int64
	OnWait       func()
	OnHold       func()
	OnUnhold     func()
	OnThrottle   func()
	OnUnthrottle func()
}

func (s *Scheduler) groupFor(key string, maxConcurrent int) *group {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groupForLocked(key, maxConcurrent, false)
}

// groupForLocked returns the group for key, creating it if needed. The caller
// must hold s.mu. applyCap is true only on Acquire: that is the request's
// concurrency (config MaxConcurrent or X-Proxy-Max-Concurrency), including
// explicit 0 = unlimited. Internal callers (Trip/FailSend/WaitSend/BackoffFor)
// pass applyCap false and 0 so a lookup cannot clobber a live cap.
// Cap mutation on a live group is under g.mu (lock order s.mu → g.mu).
func (s *Scheduler) groupForLocked(key string, maxConcurrent int, applyCap bool) *group {
	g, ok := s.groups[key]
	if !ok {
		g = &group{sched: s, maxConcurrent: maxConcurrent, sendKick: make(chan struct{})}
		g.pacer.fn = func() {
			g.kickSend()
			g.drain()
		}
		s.groups[key] = g
		return g
	}
	if applyCap && g.maxConcurrent != maxConcurrent {
		g.mu.Lock()
		g.maxConcurrent = maxConcurrent
		g.mu.Unlock()
	}
	return g
}

// Acquire blocks until the group's head-of-queue slot is available, respecting
// concurrency caps, rate-limit pacing, and operator pause (AddHold / SetHolds).
// It returns a Release that settles usage and frees the slot, or an error if
// the context is cancelled or the wait exceeds MaxWait.
func (s *Scheduler) Acquire(ctx context.Context, key string, maxConcurrent int) (Release, error) {
	return s.AcquireWith(ctx, key, maxConcurrent, WaiterHooks{})
}

// AcquireWith is Acquire plus the caller's client identity and hold hooks.
func (s *Scheduler) AcquireWith(ctx context.Context, key string, maxConcurrent int, hooks WaiterHooks) (Release, error) {
	s.mu.Lock()
	g := s.groupForLocked(key, maxConcurrent, true)
	opts := s.opts
	s.mu.Unlock()
	return g.acquire(ctx, opts, hooks)
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
		st.Queued += len(g.queue)
		g.mu.Unlock()
	}
	return st
}

func (s *Scheduler) drainAll() {
	s.mu.Lock()
	groups := make([]*group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	s.mu.Unlock()
	for _, g := range groups {
		g.drain()
	}
}

func (s *Scheduler) kickC() <-chan struct{} {
	s.pauseMu.Lock()
	ch := s.kick
	s.pauseMu.Unlock()
	return ch
}

// SetRateLimit pauses the group for at least d. In-flight requests already
// on the wire are unaffected; Acquire waiters wait. A shorter follow-up
// must not shrink a longer window (fail closed). Does not claim the retry
// send token - use Trip for that.
func (s *Scheduler) SetRateLimit(key string, d time.Duration) {
	s.extendWindow(key, d, false)
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
	// A non-positive duration means "no new wait" - never let it erase an
	// existing pacing window (fail closed). Claiming still trips so
	// siblings queue even when the owner can retry immediately.
	if d > 0 {
		until := time.Now().Add(d)
		if until.After(g.nextAllowedAt) {
			g.nextAllowedAt = until
			g.pacer.arm(until, false)
		}
	}
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
			select {
			case <-kick:
			case <-sendKick:
			case <-ctx.Done():
				return own, ctx.Err()
			}
			continue
		}
		if own || !g.tripped {
			g.mu.Unlock()
			return own, nil
		}
		if g.probing {
			g.mu.Unlock()
			select {
			case <-kick:
			case <-sendKick:
			case <-ctx.Done():
				return own, ctx.Err()
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
// (BackoffFor, no hint) and request-level (FailSend) backoff. "Double"
// is the algorithm, with a fixed jitter band rather than an operator setting.
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
	if d > 0 {
		until := time.Now().Add(d)
		if until.After(g.nextAllowedAt) {
			g.nextAllowedAt = until
			g.pacer.arm(until, false)
		}
	}
	g.tripped = true
	g.kickSendLocked()
	g.mu.Unlock()
	g.drain()
}

// RequestBackoff is the stored consecutive-request backoff (pre-jitter).
func (s *Scheduler) RequestBackoff(key string) time.Duration {
	s.mu.Lock()
	g, ok := s.groups[key]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requestBackoff
}

// Backoff returns the current adaptive backoff for a group.
func (s *Scheduler) Backoff(key string) time.Duration {
	s.mu.Lock()
	g, ok := s.groups[key]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backoff
}

func (g *group) kickC() <-chan struct{} {
	if g.sched == nil {
		return nil
	}
	return g.sched.kickC()
}

func (g *group) acquire(ctx context.Context, opts Options, hooks WaiterHooks) (Release, error) {
	enqueued := time.Now()
	g.mu.Lock()
	if hooks.Provider != "" && g.provider == "" {
		g.provider = hooks.Provider
	}
	// Fast path: queue empty, capacity available, past pacing window, not held,
	// provider throttle admits. tryAdmit consumes RPM/TPM/concurrency - only
	// call it when every other predicate already passed.
	if len(g.queue) == 0 &&
		(g.maxConcurrent == 0 || g.inFlight < g.maxConcurrent) &&
		!time.Now().Before(g.nextAllowedAt) &&
		g.sched.matchingHold(hooks.Client, hooks.Provider) == nil {
		ok, lease, wait := g.sched.tryAdmit(hooks.Provider, hooks.EstTokens)
		if ok {
			g.inFlight++
			g.mu.Unlock()
			return g.slotRelease(lease), nil
		}
		if wait > 0 {
			g.sched.armThrottleDrain(hooks.Provider, wait)
		}
	}
	if opts.MaxQueueSize > 0 && len(g.queue) >= opts.MaxQueueSize {
		g.mu.Unlock()
		return nil, ErrQueueFull
	}
	// Allocate only on the queued path. ch must be buffered: cancellation after
	// a grant relies on drain's non-blocking send and phantom-slot reconciliation.
	w := &waiter{
		ch: make(chan struct{}, 1), enqueued: enqueued, ctx: ctx,
		client: hooks.Client, provider: hooks.Provider,
		onHold: hooks.OnHold, onUnhold: hooks.OnUnhold,
		estTokens: hooks.EstTokens,
	}
	if h := g.sched.matchingHold(w.client, w.provider); h != nil {
		if !g.sched.reserveHold(w, h) {
			g.mu.Unlock()
			return nil, ErrQueueFull
		}
	}
	g.queue = append(g.queue, w)
	if w.holdID != "" && opts.MaxWait > 0 {
		// Stamp hold start here so a waiter that hasn't entered the wait
		// loop yet still excludes pause time from MaxWait.
		w.pausedAt = w.enqueued
	}
	g.mu.Unlock()
	if hooks.OnWait != nil {
		hooks.OnWait()
	}
	// Re-check after OnWait: a stale enqueue-time snapshot can fire OnHold
	// after the operator already lifted the hold during the SSE write.
	if h := g.sched.matchingHold(w.client, w.provider); h != nil {
		w.adoptHold(g.sched, h)
	}
	// If the hold lifted between the fast-path check and enqueue, drain grants
	// now - otherwise the unpause kick already fired and this waiter would
	// wait forever (lost wakeup).
	g.drain()

	deadline := time.Time{}
	if opts.MaxWait > 0 {
		deadline = w.enqueued.Add(opts.MaxWait)
	}

	throttling := false
	defer func() {
		if throttling && hooks.OnUnthrottle != nil {
			hooks.OnUnthrottle()
		}
	}()

	for {
		// Snapshot kick before the hold predicate. Select evaluates channel
		// operands once on entry; AddHold close-and-replaces kick, so a
		// post-predicate kickC() can miss the close (lost wakeup).
		kick := g.kickC()
		sendKick := g.sendKickC()
		h := g.sched.matchingHold(w.client, w.provider)
		if h != nil {
			if !deadline.IsZero() && w.pausedAt.IsZero() {
				w.pausedAt = time.Now()
			}
			w.adoptHold(g.sched, h)
			var untilTimer *time.Timer
			var untilC <-chan time.Time
			if !h.until.IsZero() {
				d := time.Until(h.until)
				if d <= 0 {
					g.drain()
					continue
				}
				untilTimer = time.NewTimer(d)
				untilC = untilTimer.C
			}
			select {
			case <-w.ch:
				if untilTimer != nil {
					untilTimer.Stop()
				}
				w.dropHold(g.sched)
				return g.slotRelease(w.admission), nil
			case <-untilC:
				if untilTimer != nil {
					untilTimer.Stop()
				}
				g.drain()
				continue
			case <-kick:
				if untilTimer != nil {
					untilTimer.Stop()
				}
				continue
			case <-ctx.Done():
				if untilTimer != nil {
					untilTimer.Stop()
				}
				g.removeWaiter(w)
				return nil, ctx.Err()
			}
		}
		// Re-check under holdCountMu: AddHold may have published +
		// seeded after the snapshot above. A bare dropHold would
		// undo that count.
		if w.syncUnhold(g.sched) {
			continue
		}
		if !w.pausedAt.IsZero() {
			deadline = deadline.Add(time.Since(w.pausedAt))
			w.pausedAt = time.Time{}
		}
		// Hold just lifted (Until or kick) - grant ourselves if the group is idle.
		g.drain()

		blocked := g.sched.throttleBlocked(w.provider, w.estTokens)
		if blocked && !throttling {
			throttling = true
			if hooks.OnThrottle != nil {
				hooks.OnThrottle()
			}
		} else if !blocked && throttling {
			throttling = false
			if hooks.OnUnthrottle != nil {
				hooks.OnUnthrottle()
			}
		}

		var maxWait *time.Timer
		var maxWaitC <-chan time.Time
		if !deadline.IsZero() && g.sched.matchingHold(w.client, w.provider) == nil {
			maxWait = time.NewTimer(time.Until(deadline))
			maxWaitC = maxWait.C
		}

		select {
		case <-w.ch:
			if maxWait != nil {
				maxWait.Stop()
			}
			w.dropHold(g.sched)
			return g.slotRelease(w.admission), nil
		case <-sendKick:
			if maxWait != nil {
				maxWait.Stop()
			}
			continue
		case <-maxWaitC:
			g.removeWaiter(w)
			return nil, errMaxWait
		case <-kick:
			if maxWait != nil {
				maxWait.Stop()
			}
			continue
		case <-ctx.Done():
			g.removeWaiter(w)
			if maxWait != nil {
				maxWait.Stop()
			}
			return nil, ctx.Err()
		}
	}
}

// release frees a slot and grants the next queued waiter if possible.
func (g *group) release() {
	g.mu.Lock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.mu.Unlock()
	g.drain()
}

// drain grants Acquire slots to the next un-held waiter while capacity and
// pacing allow. It does not consult tripped/probing - WaitSend is the send
// gate. Granting a slot while half-open is required so that waiter can
// become the next probe. Held waiters stay in place so a paused client
// does not block others on the same key. matchingHold is lock-free
// (atomic policy pointer).
func (g *group) drain() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	read, kept := 0, 0
	for read < len(g.queue) &&
		(g.maxConcurrent == 0 || g.inFlight < g.maxConcurrent) &&
		!now.Before(g.nextAllowedAt) {
		w := g.queue[read]
		// Leave cancelled waiters queued so removeWaiter still finds them.
		// Dropping here made removeWaiter treat them as a grant and
		// decrement a live in-flight slot.
		if w.ctx != nil && w.ctx.Err() != nil {
			g.queue[kept] = w
			kept++
			read++
			continue
		}
		if g.sched.matchingHold(w.client, w.provider) != nil {
			g.queue[kept] = w
			kept++
			read++
			continue
		}
		ok, lease, wait := g.sched.tryAdmit(w.provider, w.estTokens)
		if !ok {
			if wait > 0 {
				g.sched.armThrottleDrain(w.provider, wait)
			}
			break
		}
		w.admission = lease
		read++
		g.inFlight++
		// Non-blocking send: w.ch is buffered (cap 1) so this always succeeds
		// for a popped waiter. A waiter that was cancelled before reading the
		// grant is reconciled by removeWaiter's phantom-slot decrement.
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
	// Compact once, preserving held/cancelled waiters and the untouched FIFO
	// suffix. Removing each grant separately shifted the suffix O(Q²) times.
	kept += copy(g.queue[kept:], g.queue[read:])
	clear(g.queue[kept:])
	g.queue = g.queue[:kept]
}

func (g *group) removeWaiter(w *waiter) {
	g.mu.Lock()
	found := false
	for i, q := range g.queue {
		if q == w {
			g.queue = slices.Delete(g.queue, i, i+1)
			found = true
			break
		}
	}
	if !found {
		// The waiter was already granted a slot (popped + signaled) but cancelled
		// before reading the channel. Release that phantom slot.
		if g.inFlight > 0 {
			g.inFlight--
		}
	}
	g.mu.Unlock()
	w.dropHold(g.sched)
	if !found {
		w.admission.finish(0)
	}
	// Removing a throttled head can expose a smaller reservation that fits now.
	// Re-evaluate synchronously; no detached goroutine is needed for a cancel.
	g.drain()
}

// maxRetryHint is the ceiling on a provider-supplied Retry-After /
// rate-limit-reset duration. Daily quota windows routinely last tens of
// minutes and can run until UTC midnight (~24h). This is a safety guardrail
// only - a hostile or buggy upstream (Retry-After: 99999999) must not stall
// the request and the whole provider+key group forever. It is independent of
// MaxBackoff, which caps adaptive exponential backoff when there is NO hint.
const maxRetryHint = 24 * time.Hour

// Backoff computes the pacing delay for a rate-limited group, preferring
// explicit provider hints (Retry-After, x-ratelimit-reset) over exponential
// backoff. It also updates the group's adaptive backoff state.
func (s *Scheduler) BackoffFor(key string, retryAfter time.Duration) time.Duration {
	s.mu.Lock()
	g := s.groupForLocked(key, 0, false)
	opts := s.opts // read under s.mu (UpdateOptions mutates it)
	s.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if retryAfter > 0 {
		g.backoff = 0 // explicit hint resets adaptive backoff
		// Honor the provider hint. MaxBackoff must not shrink a legitimate
		// daily-limit Retry-After (often 30–60m) down to the adaptive cap
		// (default 2m) - that just re-hammers the provider. Bound only
		// pathological values so a buggy upstream cannot stall the group
		// forever.
		if retryAfter > maxRetryHint {
			retryAfter = maxRetryHint
		}
		return retryAfter
	}
	g.backoff = growBackoff(g.backoff, opts.BaseBackoff, opts.MaxBackoff)
	// Jitter: 0.75x–1.25x to avoid thundering herd. Do not write
	// nextAllowedAt here - only Trip/SetRateLimit trip the group.
	return jitterBackoff(g.backoff)
}

// ResetBackoff clears the adaptive backoff after a success.
func (s *Scheduler) ResetBackoff(key string) {
	s.mu.Lock()
	g, ok := s.groups[key]
	s.mu.Unlock()
	if !ok {
		return
	}
	g.mu.Lock()
	g.backoff = 0
	g.mu.Unlock()
}

var (
	// ErrQueueFull is returned when the group queue or a pause's MaxQueued
	// cap is exhausted. The proxy maps this to HTTP 429.
	ErrQueueFull = errorString("queue full")
	// ErrOverlap is returned when AddHold / SetHolds / ReplaceHold would
	// park a target already covered by another pause.
	ErrOverlap = errorString("pause overlaps existing hold")
	// ErrHoldNotFound is returned when ReplaceHold targets an unknown id.
	ErrHoldNotFound = errorString("pause hold not found")
	errMaxWait      = errorString("queue wait exceeded")
)

type errorString string

func (e errorString) Error() string { return string(e) }
