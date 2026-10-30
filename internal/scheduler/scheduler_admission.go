package scheduler

import (
	"context"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

// group is a single FIFO queue with concurrency and pacing state.
type group struct {
	sched *Scheduler
	// key is the provider|key identity this group was created for. It is
	// immutable and lets eviction delete the exact map entry without trusting
	// a caller-supplied key.
	key string
	// provider is the Record.Provider this group was first attributed to by
	// AcquireWith. It is written only under mu, together with the
	// providerGroups index link, and is immutable once non-empty: the
	// proxy's group key includes the provider, so one key cannot legitimately
	// switch providers. An empty group can still be created by the internal
	// lookup paths; its first non-empty AcquireWith attributes it.
	provider string
	// providerIdx is the attributed index (nil until attribution). Attribution
	// publishes it under both Scheduler.mu and the group mutex: fair-set
	// marking reads it under g.mu, and eviction clears it under both locks
	// after retiring the group. providerPrev/providerNext/providerListed are
	// the index list links, guarded by providerIdx.mu.
	providerIdx    *providerIndex
	providerPrev   *group
	providerNext   *group
	providerListed bool
	// idlePrev/idleNext order idle groups for LRU eviction and are guarded by
	// Scheduler.idleMu. idleListed is a lock-free fast-path witness for that
	// membership; it changes under idleMu. Activity transitions, which hold
	// g.mu, skip the idle lock when it is false; eviction re-verifies idleness
	// under g.mu. A listed group is idle (inFlight == 0 and queue empty).
	idlePrev   *group
	idleNext   *group
	idleListed atomic.Bool
	// retired is set under mu only after eviction re-verified that no
	// in-flight slot or queued waiter remains. A stale Acquire that looked the
	// group up before eviction must observe it and retry against the map.
	retired        bool
	maxConcurrent  int
	inFlight       int
	nextAllowedAt  time.Time     // do not send before this time (after rate limit)
	backoff        time.Duration // attempt-level (BackoffFor; grows even with a hint)
	requestBackoff time.Duration // consecutive failed requests (FailSend)
	tripped        bool          // a request is retrying; siblings must WaitSend
	probing        bool          // the retrying request holds the send token
	sendWaiters    int           // callers parked in WaitSend's send gate
	sendKick       chan struct{} // closed and replaced when the send gate changes
	pacer          wakeTimer     // one shared pacing wakeup, not one per waiter
	queue          waiterQueue   // parked Acquire callers, oldest first
	// hasQueued mirrors queue.live > 0 for lock-free drainProvider probes.
	// providerQueued is the owning provider index's queued-group counter,
	// installed at first attribution. Both move together, only under mu, at the
	// queue's empty/non-empty transition - syncQueueLocked is the sole owner.
	hasQueued      atomic.Bool
	providerQueued *atomic.Int64
	// gateWaiting is true while this group belongs to its provider index's
	// fair-waiting set: its queued head was refused by the shared provider
	// gate rather than by a per-key predicate. providerFair is that index's
	// lock-free fair-waiting count, installed at attribution under mu. The
	// fast path admits only when the count is zero, so a peer parked on
	// pacing, a hold, the anti-burst ramp or a per-key cap never blocks it.
	// fairIndex is this group's slot in the index's fair slice, guarded by
	// providerIdx.mu. All three are published together under mu.
	gateWaiting  atomic.Bool
	providerFair *atomic.Int64
	fairIndex    int
	// rampStart anchors this group's anti-burst epoch: allowance grows from 1
	// toward the provider's configured Concurrency as time passes. Zero means
	// no epoch yet. idleSince stamps the moment the group became fully idle
	// (inFlight == 0 and queue empty), so a first grant can tell a short gap
	// that continues the epoch from a full RampDuration that resets it. Both
	// fields are guarded by mu and only meaningful while the provider policy
	// is enabled.
	rampStart time.Time
	idleSince time.Time
	mu        sync.Mutex
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
	queue     *waiterQueue
	prev      *waiter
	next      *waiter
}

// waiterQueue is a group's FIFO of parked Acquire callers. It owns membership
// and the live count: a cancelled waiter is unlinked in O(1), so there is no
// tombstone slot a reader could mistake for a live waiter and no compaction
// pass to defer. Every node reachable from head is a live waiter. The zero
// value is empty. All access is guarded by the owning group's mu.
type waiterQueue struct {
	head *waiter
	tail *waiter
	live int
}

// push appends w at the tail.
func (q *waiterQueue) push(w *waiter) {
	w.queue = q
	w.prev = q.tail
	w.next = nil
	if q.tail != nil {
		q.tail.next = w
	} else {
		q.head = w
	}
	q.tail = w
	q.live++
}

// remove unlinks w and reports whether it was still queued. A waiter granted
// by drain is unlinked at grant, so a later cancellation sees false and
// follows the phantom-slot path in removeWaiter.
func (q *waiterQueue) remove(w *waiter) bool {
	if w == nil || w.queue != q {
		return false
	}
	if w.prev != nil {
		w.prev.next = w.next
	} else {
		q.head = w.next
	}
	if w.next != nil {
		w.next.prev = w.prev
	} else {
		q.tail = w.prev
	}
	w.queue, w.prev, w.next = nil, nil, nil
	q.live--
	return true
}

// forEach calls fn for every live waiter in FIFO order. It is the only
// read-only walk of the queue, so consumers never see a removed waiter.
func (q *waiterQueue) forEach(fn func(*waiter)) {
	for w := q.head; w != nil; w = w.next {
		fn(w)
	}
}

// WaiterHooks is optional Acquire context: which client/provider this caller
// is, and live-dashboard callbacks when the caller starts or stops waiting.
// A request is queued whenever the scheduler parks it instead of letting it
// send: the admission queue (concurrency cap, pacing window, provider cap,
// operator hold) or WaitSend's send gate (retry pacing, probe serialization).
// OnWait signals a known wait outside scheduler locks: once per Acquire
// enqueue, and once when WaitSend enters an operator hold. It may commit SSE
// headers; it never fires for a fast Acquire or an already-full queue.
// OnHold/OnUnhold are lifecycle-only: non-blocking, no I/O or scheduler re-entry.
// OnThrottle / OnUnthrottle bookend every queued wait - admission queue or send
// gate - so a parked request never reads as actively sending. They are
// lifecycle-only too: non-blocking, no I/O or scheduler re-entry. Release owns
// usage.
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

// AcquireWith blocks until the group's head-of-queue slot is available,
// respecting concurrency caps, rate-limit pacing, and operator pause
// (AddHold / RestoreHolds). hooks carry the caller's client and provider
// identity plus hold callbacks. It returns a Release that settles usage and
// frees the slot, or an error if the context is cancelled or the wait
// exceeds MaxWait.
func (s *Scheduler) AcquireWith(ctx context.Context, key string, maxConcurrent int, hooks WaiterHooks) (Release, error) {
	for {
		s.mu.Lock()
		if _, exists := s.groups[key]; !exists {
			// A client that rotates keys forever must not grow the group map
			// for the process lifetime. Reclaim the least-recently-idle
			// groups first; a brand-new key is refused only when every
			// retained group has in-flight or queued work.
			s.makeGroupRoomLocked()
			if len(s.groups) >= s.groupCap {
				s.mu.Unlock()
				return nil, ErrGroupLimit
			}
		}
		g := s.groupForLocked(key, maxConcurrent, true)
		s.attributeProviderLocked(g, hooks.Provider)
		opts := s.opts
		s.mu.Unlock()
		release, err := g.acquire(ctx, opts, hooks)
		if err == errGroupRetired {
			// The group was idle and evicted between lookup and admission;
			// retry against whatever the map holds now.
			continue
		}
		return release, err
	}
}

func (g *group) acquire(ctx context.Context, opts Options, hooks WaiterHooks) (Release, error) {
	enqueued := time.Now()
	g.mu.Lock()
	if g.retired {
		g.mu.Unlock()
		return nil, errGroupRetired
	}
	// Provider attribution happened under s.mu in AcquireWith before this
	// call, so the group is already visible to provider-scoped drains.
	// Fast path: queue empty, no other key of this provider waiting on the
	// shared gate, capacity available, past pacing window, not held, provider
	// throttle and anti-burst allowance admit. A non-zero provider fair-waiting
	// count means another key was refused the same provider gate and needs this
	// drain's turn; a fresh key must enqueue and take its turn instead of
	// jumping that work. A peer parked only on its own pacing window, hold,
	// ramp or per-key cap is not in that set: it cannot use either a free
	// provider slot or a provider bucket token, so it must not suppress this
	// fast path. tryAdmit consumes RPM/TPM/concurrency - only call it when
	// every other predicate already passed.
	now := time.Now()
	providerFairIdle := g.providerFair == nil || g.providerFair.Load() == 0
	if g.queue.live == 0 &&
		providerFairIdle &&
		(g.maxConcurrent == 0 || g.inFlight < g.maxConcurrent) &&
		!now.Before(g.nextAllowedAt) &&
		g.sched.matchingHold(hooks.Client, hooks.Provider) == nil {
		ok, lease, wait, _ := g.admitLocked(now, hooks.Provider, hooks.EstTokens)
		if ok {
			g.inFlight++
			g.sched.markGroupBusyLocked(g)
			g.mu.Unlock()
			return g.slotRelease(lease), nil
		}
		if wait > 0 {
			g.sched.armThrottleDrain(hooks.Provider, wait)
		}
	}
	if opts.MaxQueueSize > 0 && g.queue.live >= opts.MaxQueueSize {
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
	g.queue.push(w)
	g.syncQueueLocked()
	g.sched.markGroupBusyLocked(g)
	if w.holdID != "" && opts.MaxWait > 0 {
		// Stamp hold start here so a waiter that hasn't entered the wait
		// loop yet still excludes pause time from MaxWait.
		w.pausedAt = w.enqueued
	}
	g.mu.Unlock()
	if hooks.OnWait != nil {
		hooks.OnWait()
	}
	// The request is queued from this enqueue until drain grants it. Publish
	// that state once for the whole wait: every reason (concurrency cap,
	// pacing window, provider cap, operator hold) parks here, and only the
	// grant or the caller's exit ends it. The adjacent defer owns the lift on
	// every return path.
	defer func() {
		if hooks.OnUnthrottle != nil {
			hooks.OnUnthrottle()
		}
	}()
	if hooks.OnThrottle != nil {
		hooks.OnThrottle()
	}
	// Re-check after OnWait: a stale enqueue-time snapshot can fire OnHold
	// after the operator already lifted the hold during the SSE write.
	if h := g.sched.matchingHold(w.client, w.provider); h != nil {
		w.adoptHold(g.sched, h)
	}
	// If the hold lifted between the fast-path check and enqueue, drain grants
	// now - otherwise the unpause kick already fired and this waiter would
	// wait forever (lost wakeup). The fair provider drain keeps a local
	// enqueue from refilling its own key ahead of other queued keys.
	g.drainAdmission()

	deadline := time.Time{}
	if opts.MaxWait > 0 {
		deadline = w.enqueued.Add(opts.MaxWait)
	}

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
					g.drainAdmission()
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
				g.drainAdmission()
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
		g.drainAdmission()

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

// release frees a slot and grants the next queued waiter through the owning
// provider's fair drain. The caller has already settled the released
// acquisition's gate usage, so token accounting is current before this drain
// admits anyone. Keeping release local made a busy key refill itself ahead of
// other keys waiting on the same provider allowance.
func (g *group) release() {
	g.mu.Lock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.sched.markGroupIdleLocked(g)
	g.mu.Unlock()
	g.drainAdmission()
}

// drainAdmission runs a local admission re-check through the group's provider
// when the group is attributed, and locally otherwise. Reading provider under
// g.mu keeps attribution's publication race-free; the drain itself runs with
// no scheduler, throttle or group lock held. A local trigger cannot cheaply
// prove which predicate blocked a queued waiter, so it optimistically marks the
// group for this one fair drain; the drain clears the mark again when it stops
// on a per-key predicate before any provider refusal. Marking ahead of the
// drain is required: a release that frees a provider slot must find this group,
// while a peer parked on pacing/hold/cap stays out of the fast path's witness.
// markGateWaitingLocked owns the publication predicate, so a group whose own
// pacing window or per-key cap holds it back cannot enter the fair set at all.
func (g *group) drainAdmission() {
	g.mu.Lock()
	provider := g.provider
	if provider != "" {
		g.markGateWaitingLocked()
		g.mu.Unlock()
		g.sched.drainProvider(provider)
		return
	}
	g.mu.Unlock()
	g.drain()
}

// admissionWindowOpenLocked reports whether the group's own pacing window and
// per-key concurrency cap would let a queued head reach the shared provider
// gate now. A group held back by either predicate cannot use a provider slot
// or bucket token until its own timer or release fires. The anti-burst ramp is
// deliberately not consulted here: admitLocked may anchor an epoch and still
// admit, so a mark decision must stay permissive. Callers hold g.mu.
func (g *group) admissionWindowOpenLocked(now time.Time) bool {
	if g.maxConcurrent != 0 && g.inFlight >= g.maxConcurrent {
		return false
	}
	return !now.Before(g.nextAllowedAt)
}

// markGateWaitingLocked publishes g into its provider's fair-waiting set when
// the group can actually use the shared gate now: it has a live waiter and its
// own pacing window and per-key cap are both open. Every caller holds g.mu, so
// the check and the publication are one atomic transition. A closed group must
// stay out of the set because no provider release or bucket refill can serve it
// until its own timer or release fires; publishing it would latch a mark on a
// saturated provider, where no drain runs to withdraw it. The index lock nests
// inside g.mu, and no path may take the reverse order. A group without a
// provider index (unattributed) has no shared gate to wait on, so the call is a
// no-op.
func (g *group) markGateWaitingLocked() {
	if g.providerIdx == nil || g.gateWaiting.Load() {
		return
	}
	if g.queue.live == 0 || !g.admissionWindowOpenLocked(time.Now()) {
		return
	}
	g.providerIdx.markFairLocked(g)
}

// clearGateWaitingLocked withdraws g from its provider's fair-waiting set.
// Callers hold g.mu. The lock-free fast path observes the count drop
// immediately; the slice tombstone is compacted by the next snapshot.
func (g *group) clearGateWaitingLocked() {
	if g.providerIdx == nil || !g.gateWaiting.Load() {
		return
	}
	g.providerIdx.clearFairLocked(g)
}

// drain is the unlimited local form of drainLimited, retained for unattributed
// groups and internal callers that hold no provider-wide queued work.
func (g *group) drain() {
	g.drainLimited(0)
}

// rampPolicy resolves this group's provider anti-burst policy. Callers hold
// g.mu; the gate read nests inside that existing g.mu -> providerGate.mu order.
// enabled is true only when the flag is set and both dimensions can ramp:
// Concurrency >= 2 and RampDuration > 0. The returned target is always the
// configured provider concurrency, never a live allowance.
func (g *group) rampPolicy() (target int, duration time.Duration, enabled bool) {
	if g.provider == "" {
		return 0, 0, false
	}
	gate := g.sched.gate(g.provider)
	if gate == nil {
		return 0, 0, false
	}
	gate.mu.Lock()
	limit := gate.throttle.Limit
	gate.mu.Unlock()
	if !limit.rampActive() {
		return 0, 0, false
	}
	return limit.Concurrency, limit.RampDuration, true
}

// rampSteps returns floor(elapsed*(target-1)/duration). The 128-bit product
// keeps a legal maximum concurrency target and ramp duration from overflowing
// the 64-bit intermediate; elapsed is known to be below duration here.
func rampSteps(elapsed, duration time.Duration, target int) int {
	if target < 2 || duration <= 0 || elapsed <= 0 {
		return 0
	}
	if elapsed >= duration {
		return target - 1
	}
	hi, lo := bits.Mul64(uint64(target-1), uint64(elapsed))
	steps, _ := bits.Div64(hi, lo, uint64(duration))
	return int(steps)
}

// rampStepDuration returns ceil(steps*duration/(target-1)): the earliest
// elapsed time at which the floor-based allowance has grown past steps. It is
// the ceiling so a wake scheduled exactly at a step boundary observes the
// increment instead of re-arming the same boundary forever.
func rampStepDuration(steps, target int, duration time.Duration) time.Duration {
	if steps <= 0 || target < 2 || duration <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(steps), uint64(duration))
	q, r := bits.Div64(hi, lo, uint64(target-1))
	if r != 0 {
		q++
	}
	return time.Duration(q)
}

// rampAllowanceLocked is the group's live anti-burst allowance:
// 1 + floor((target-1) * elapsedSinceEpoch / RampDuration), clamped to
// [1, target]. A group with no epoch is at the floor. Callers hold g.mu.
func (g *group) rampAllowanceLocked(now time.Time, target int, duration time.Duration) int {
	if g.rampStart.IsZero() || target < 2 || duration <= 0 {
		return 1
	}
	allowance := 1 + rampSteps(now.Sub(g.rampStart), duration, target)
	if allowance < 1 {
		return 1
	}
	if allowance > target {
		return target
	}
	return allowance
}

// nextRampStepLocked returns the absolute time of the next allowance increase,
// or zero when the group is already at the target. The deadline is never
// earlier than 1ms from now so a clock edge or a late drain cannot spin the
// shared provider wake. Callers hold g.mu.
func (g *group) nextRampStepLocked(now time.Time, target int, duration time.Duration) time.Time {
	if g.rampStart.IsZero() || target < 2 || duration <= 0 {
		return time.Time{}
	}
	allowance := g.rampAllowanceLocked(now, target, duration)
	if allowance >= target {
		return time.Time{}
	}
	next := g.rampStart.Add(rampStepDuration(allowance, target, duration))
	if next.Before(now.Add(time.Millisecond)) {
		next = now.Add(time.Millisecond)
	}
	return next
}

// startRampLocked advances the epoch on a successful first grant. inFlight is
// still the pre-grant occupancy, so zero means this grant starts or resumes
// the group's epoch: start fresh when no epoch exists or the group has been
// fully idle for at least one ramp duration; otherwise keep the existing epoch
// across a short idle gap. A failed admission never calls this, so a policy or
// provider block cannot start the clock.
func (g *group) startRampLocked(now time.Time, duration time.Duration) {
	if g.inFlight != 0 {
		return
	}
	if g.rampStart.IsZero() || (!g.idleSince.IsZero() && now.Sub(g.idleSince) >= duration) {
		g.rampStart = now
	}
	g.idleSince = time.Time{}
}

// anchorRampLocked back-dates an epoch when the policy is enabled while this
// group already has in-flight work. Anchoring at the existing occupancy makes
// the current work already fit the allowance (occupancy+1, clamped to target)
// instead of stalling it as if the group had just started from zero.
func (g *group) anchorRampLocked(now time.Time, target int, duration time.Duration) {
	if !g.rampStart.IsZero() || g.inFlight <= 0 || target < 2 || duration <= 0 {
		return
	}
	occupancy := min(g.inFlight, target-1)
	g.rampStart = now.Add(-rampStepDuration(occupancy, target, duration))
	g.idleSince = time.Time{}
}

// resetRampLocked clears an epoch whose policy shape is gone or changed.
func (g *group) resetRampLocked() {
	g.rampStart = time.Time{}
	g.idleSince = time.Time{}
}

// armRampStepLocked arms the shared provider wake for the next allowance
// increase when this group still has queued waiters. The one provider timer
// coalesces every key's next step through armThrottleDrain's earliest-deadline
// rule. Callers hold g.mu; arming nests g.mu -> providerGate.mu as tryAdmit
// already does.
func (g *group) armRampStepLocked(now time.Time) {
	if g.queue.live == 0 {
		return
	}
	target, duration, enabled := g.rampPolicy()
	if !enabled {
		return
	}
	if next := g.nextRampStepLocked(now, target, duration); !next.IsZero() {
		g.sched.armThrottleDrain(g.provider, time.Until(next))
	}
}

// admitLocked applies the per-key anti-burst allowance and then the provider
// gate. Callers hold g.mu and have already checked the group's own
// maxConcurrent, pacing window and hold predicate. A disabled policy clears
// stale ramp state. A ramp-blocked call arms the next step and reports no
// wait: the caller stops its FIFO pass until that wake. gateBlocked reports
// that tryAdmit was reached and refused, which is the only case that publishes
// the group into the provider's fair-waiting set; a queued waiter is still
// refused while the gate stays full, so the mark survives further passes. A
// fast-path call with no queued waiter marks nothing: its enqueue drain owns
// that transition. A failed provider admission never starts a fresh epoch;
// only a successful grant does, while the mid-flight anchor reflects
// occupancy that already exists.
func (g *group) admitLocked(now time.Time, provider string, estTokens int64) (ok bool, lease admission, wait time.Duration, gateBlocked bool) {
	target, duration, enabled := g.rampPolicy()
	if !enabled {
		if !g.rampStart.IsZero() || !g.idleSince.IsZero() {
			g.resetRampLocked()
		}
		ok, lease, wait = g.sched.tryAdmit(provider, estTokens)
		if !ok && g.queue.live > 0 {
			g.markGateWaitingLocked()
			gateBlocked = true
		}
		return ok, lease, wait, gateBlocked
	}
	g.anchorRampLocked(now, target, duration)
	if g.inFlight >= g.rampAllowanceLocked(now, target, duration) {
		if next := g.nextRampStepLocked(now, target, duration); !next.IsZero() {
			g.sched.armThrottleDrain(g.provider, time.Until(next))
		}
		return false, admission{}, 0, false
	}
	ok, lease, wait = g.sched.tryAdmit(provider, estTokens)
	if ok {
		g.startRampLocked(now, duration)
	} else if g.queue.live > 0 {
		g.markGateWaitingLocked()
		gateBlocked = true
	}
	return ok, lease, wait, gateBlocked
}

// drainLimited grants Acquire slots to the next un-held waiters while capacity,
// pacing and the anti-burst allowance allow. budget limits how many waiters
// this call may grant; budget <= 0 means unlimited. It returns the grant count,
// which the fair provider drain uses to detect a pass that made no progress.
// Cancelled and held waiters are preserved in FIFO order without consuming the
// budget. It does not consult tripped/probing - WaitSend is the send gate.
// Granting a slot while half-open is required so that waiter can become the
// next probe. matchingHold is lock-free (atomic policy pointer).
//
// Fair-set ownership: a provider refusal marks the group and keeps it marked
// across passes; a stop on pacing, the per-key cap, a hold-only remainder or
// the anti-burst ramp withdraws it, because another key's release or a
// provider timer cannot relieve those predicates. A pass stopped only by its
// one-grant budget keeps the mark so the next fair pass can use free provider
// capacity for the same key.
func (g *group) drainLimited(budget int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	granted := 0
	refused, budgetStop := false, false
	for w := g.queue.head; w != nil; {
		if budget > 0 && granted >= budget {
			budgetStop = true
			break
		}
		if (g.maxConcurrent != 0 && g.inFlight >= g.maxConcurrent) || now.Before(g.nextAllowedAt) {
			break
		}
		next := w.next
		// Leave cancelled waiters queued so their own removeWaiter observes
		// membership. Dropping here made removeWaiter treat the cancellation
		// as a grant and decrement a live in-flight slot.
		if w.ctx != nil && w.ctx.Err() != nil {
			w = next
			continue
		}
		if g.sched.matchingHold(w.client, w.provider) != nil {
			w = next
			continue
		}
		ok, lease, wait, gateBlocked := g.admitLocked(now, w.provider, w.estTokens)
		if !ok {
			refused = gateBlocked
			if wait > 0 {
				g.sched.armThrottleDrain(w.provider, wait)
			}
			break
		}
		w.admission = lease
		g.queue.remove(w)
		g.inFlight++
		granted++
		// Non-blocking send: w.ch is buffered (cap 1) so this always succeeds
		// for a popped waiter. A waiter that was cancelled before reading the
		// grant is reconciled by removeWaiter's phantom-slot decrement.
		select {
		case w.ch <- struct{}{}:
		default:
		}
		w = next
	}
	if granted > 0 {
		g.syncQueueLocked()
		// A budget-limited pass stops with waiters still queued while the ramp
		// allowance or a provider bucket opens later. Arm that future wake so
		// the remaining FIFO does not wait for an unrelated release.
		g.armRampStepLocked(now)
	}
	if !refused && !budgetStop {
		// The pass ended without a provider refusal and without a fair turn cut
		// short by the budget: whatever remains is held, cancelled, paced, over
		// the per-key cap or ramp-blocked, none of which the shared gate can
		// relieve. Withdraw the mark so this key stops taking fair-drain turns.
		g.clearGateWaitingLocked()
	}
	return granted
}

// syncQueueLocked is the sole owner of the queue empty/non-empty witness and
// its provider counters. Callers hold g.mu after mutating queue membership
// (enqueue, drain grant, removeWaiter unlink). No queue reset/close path exists
// today; a future one must route through this same transition. hasQueued
// mirrors queue.live > 0 for lock-free drainProvider skips, and providerQueued
// moves exactly on the edge, so the per-provider count can never go negative or
// stay positive with an empty queue. An empty queue also withdraws the group's
// fair-waiting mark: no waiter remains that the shared gate could refuse.
func (g *group) syncQueueLocked() {
	nonEmpty := g.queue.live > 0
	if !nonEmpty {
		g.clearGateWaitingLocked()
	}
	if g.hasQueued.Load() == nonEmpty {
		return
	}
	g.hasQueued.Store(nonEmpty)
	if g.providerQueued == nil {
		return
	}
	if nonEmpty {
		g.providerQueued.Add(1)
	} else {
		g.providerQueued.Add(-1)
	}
}

func (g *group) removeWaiter(w *waiter) {
	g.mu.Lock()
	headRemoved := g.queue.head == w
	found := g.queue.remove(w)
	if !found {
		headRemoved = false
		// The waiter was already granted a slot (unlinked + signaled) but
		// cancelled before reading the channel. Settle the gate before releasing
		// the phantom group slot so its reserved tokens and in-flight occupancy
		// are reconciled before any drain can admit a replacement; the caller
		// below owns the single drain.
		w.admission.settle(0)
		if g.inFlight > 0 {
			g.inFlight--
		}
	} else {
		g.syncQueueLocked()
	}
	g.sched.markGroupIdleLocked(g)
	g.mu.Unlock()
	w.dropHold(g.sched)
	// Removing a throttled head can expose a smaller reservation that fits now.
	// A non-head removal cannot change which waiter drain reaches first, so
	// skipping the synchronous pass keeps a mass cancel linear in removed
	// waiters.
	if headRemoved || !found {
		g.drainAdmission()
	}
}
