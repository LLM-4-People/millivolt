package scheduler

import (
	"context"
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
	// providerIdx is the attributed index (nil until attribution), guarded by
	// Scheduler.mu. providerPrev/providerNext/providerListed are the index
	// list links, guarded by providerIdx.mu.
	providerIdx    *providerIndex
	providerPrev   *group
	providerNext   *group
	providerListed bool
	// idlePrev/idleNext order idle groups for LRU eviction and are guarded by
	// Scheduler.idleMu. idleListed is a lock-free fast-path witness for that
	// membership; it changes under idleMu. Activity transitions, which hold
	// g.mu, skip the idle lock when it is false; eviction re-verifies idleness
	// under g.mu. A listed group is idle (inFlight == 0 and queued == 0).
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
	sendKick       chan struct{} // closed and replaced when the send gate changes
	pacer          wakeTimer     // one shared pacing wakeup, not one per waiter
	queue          []*waiter     // live waiters; cancelled entries are nil tombstones
	queued         int           // live waiter count (excludes tombstones)
	tombstones     int           // nil entries awaiting drain's compaction pass
	// hasQueued mirrors queued > 0 for lock-free drainProvider probes.
	// providerQueued is the owning provider index's queued-group counter,
	// installed at first attribution. Both move together, only under mu, at the
	// queue's empty/non-empty transition - syncQueueLocked is the sole owner.
	hasQueued      atomic.Bool
	providerQueued *atomic.Int64
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
	// Fast path: queue empty, capacity available, past pacing window, not held,
	// provider throttle admits. tryAdmit consumes RPM/TPM/concurrency - only
	// call it when every other predicate already passed.
	if g.queued == 0 &&
		(g.maxConcurrent == 0 || g.inFlight < g.maxConcurrent) &&
		!time.Now().Before(g.nextAllowedAt) &&
		g.sched.matchingHold(hooks.Client, hooks.Provider) == nil {
		ok, lease, wait := g.sched.tryAdmit(hooks.Provider, hooks.EstTokens)
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
	if opts.MaxQueueSize > 0 && g.queued >= opts.MaxQueueSize {
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
	g.queued++
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
	g.sched.markGroupIdleLocked(g)
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
	granted := false
	for read < len(g.queue) &&
		(g.maxConcurrent == 0 || g.inFlight < g.maxConcurrent) &&
		!now.Before(g.nextAllowedAt) {
		w := g.queue[read]
		if w == nil {
			// removeWaiter tombstoned a cancelled waiter; drop it in the one
			// compaction pass instead of shifting the suffix per cancel.
			read++
			continue
		}
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
		g.queued--
		g.inFlight++
		granted = true
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
	if g.tombstones > 0 {
		// The grant loop stops at the first blocked waiter, so tombstones below
		// it survive the one-pass compaction above.
		g.compactLocked()
	}
	if granted {
		g.syncQueueLocked()
	}
}

// compactLocked drops tombstoned waiters in one sweep. removeWaiter tombstones
// instead of shifting the suffix per cancel, so a mass cancel costs O(Q) in
// total rather than O(Q²).
func (g *group) compactLocked() {
	live := g.queue[:0]
	for _, w := range g.queue {
		if w != nil {
			live = append(live, w)
		}
	}
	clear(g.queue[len(live):])
	g.queue = live
	g.tombstones = 0
}

// syncQueueLocked is the sole owner of the queue empty/non-empty witness and
// its provider counter transition. Callers hold g.mu after mutating g.queued
// (enqueue, drain grant, removeWaiter tombstone); queue compaction only drops
// nil tombstones and never changes that live count, so it needs no separate
// transition. No queue reset/close path exists today; a future one must route
// through this same transition. hasQueued mirrors queued > 0 for lock-free
// drainProvider skips, and providerQueued moves exactly on the edge, so the
// per-provider count can never go negative or stay positive with an empty
// queue.
func (g *group) syncQueueLocked() {
	nonEmpty := g.queued > 0
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
	found, headRemoved := false, false
	for i, q := range g.queue {
		if q == w {
			// Tombstone instead of slices.Delete: a mass cancel must not shift
			// the queue suffix once per waiter (O(Q²)); drain's single
			// compaction pass drops the nil slots immediately below.
			g.queue[i] = nil
			g.queued--
			g.tombstones++
			found = true
			headRemoved = i == 0
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
	// Bound tombstone retention without paying a full shift per cancel. Decide
	// under the lock: the threshold reads queue state that drain mutates.
	needCompact := found && !headRemoved && g.tombstones >= max(1, len(g.queue)/8)
	if found {
		g.syncQueueLocked()
	}
	g.sched.markGroupIdleLocked(g)
	g.mu.Unlock()
	w.dropHold(g.sched)
	if !found {
		w.admission.finish(0)
	}
	// Removing a throttled head can expose a smaller reservation that fits now.
	// A non-head removal cannot change which waiter drain reaches first, and its
	// tombstone is compacted on the next drain; skipping the synchronous pass
	// keeps a mass cancel from re-walking the queue per waiter.
	if headRemoved || !found {
		g.drain()
	} else if needCompact {
		// compactLocked is idempotent; a concurrent drain may have already done
		// the sweep between the unlock and this relock.
		g.mu.Lock()
		g.compactLocked()
		g.mu.Unlock()
	}
}
