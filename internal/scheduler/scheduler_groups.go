package scheduler

import (
	"sync"
	"sync/atomic"
	"time"
)

// maxSchedulerGroups bounds distinct per-provider+key groups retained at once.
// A hostile client rotating keys can otherwise grow the map without limit; the
// bound is generous for real deployments (100k keys). At
// the bound a new key evicts the least-recently-idle group (no in-flight slot
// and no queued waiter); a group with active work is never evicted, so a table
// full of active work still refuses new keys with ErrGroupLimit.
const maxSchedulerGroups = 100_000

// providerIndex is the scheduler's provider-scoped group index. The attributed
// groups form a doubly linked list guarded by mu; eviction unlinks one group in
// O(1). fair is the provider's fair-waiting set: groups with at least one live
// waiter that the shared provider gate (concurrency or request/token bucket)
// refused, and that therefore need the fair drain's next turn. A group's
// gateWaiting flag is the membership authority; unmarking leaves a nil
// tombstone that the next snapshot, or the dead-entry threshold, compacts away.
// fairCount is the lock-free witness: a new key may take the fast path only
// when it is zero, so a peer parked on its own pacing window, hold, anti-burst
// ramp or per-key cap never suppresses an unrelated key's grant. cursor is the
// group a productive drain served last; fairSnapshot starts after it, so
// repeated drains do not favor the first-attributed key.
//
// queued counts the indexed groups whose queue currently holds at least one
// live waiter. It is the provider's total queued-group accounting (a strict
// superset of the fair set) and moves from the single queue-transition owner
// under the group's own mutex, so maintaining it never needs Scheduler.mu.
type providerIndex struct {
	mu        sync.Mutex
	head      *group
	tail      *group
	live      int
	fair      []*group // fair-waiting groups plus nil tombstones; guarded by mu
	fairDead  int      // tombstone count in fair; guarded by mu
	cursor    *group   // last group served by a productive drain; nil = snapshot head
	fairCount atomic.Int64
	queued    atomic.Int64
}

// linkLocked appends g at the provider's tail. Callers hold idx.mu.
func (idx *providerIndex) linkLocked(g *group) {
	if g.providerListed {
		return
	}
	g.providerListed = true
	g.providerPrev = idx.tail
	g.providerNext = nil
	if idx.tail != nil {
		idx.tail.providerNext = g
	} else {
		idx.head = g
	}
	idx.tail = g
	idx.live++
}

// unlinkLocked removes g from the provider's list in O(1). Callers hold idx.mu.
func (idx *providerIndex) unlinkLocked(g *group) {
	if !g.providerListed {
		return
	}
	if g.providerPrev != nil {
		g.providerPrev.providerNext = g.providerNext
	} else {
		idx.head = g.providerNext
	}
	if g.providerNext != nil {
		g.providerNext.providerPrev = g.providerPrev
	} else {
		idx.tail = g.providerPrev
	}
	g.providerPrev, g.providerNext = nil, nil
	g.providerListed = false
	if idx.cursor == g {
		// The last group served by a productive drain is going away. The next
		// snapshot starts at the slice head; eviction only removes idle groups,
		// so a group with no queued waiter had no fair turn left to lose.
		idx.cursor = nil
	}
	idx.live--
}

// markFairLocked adds g to the provider's fair-waiting set. Callers hold g.mu
// and must have verified gateWaiting is false, so the counter can never count a
// group twice; the index lock nests inside g.mu, never the reverse. The fast
// path reads fairCount without taking either lock.
func (idx *providerIndex) markFairLocked(g *group) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	g.gateWaiting.Store(true)
	g.fairIndex = len(idx.fair)
	idx.fair = append(idx.fair, g)
	idx.fairCount.Add(1)
}

// clearFairLocked removes g from the provider's fair-waiting set and compacts
// tombstones once they reach half the slice, so an unmarked set releases its
// entries instead of retaining every group that ever waited. Callers hold g.mu.
func (idx *providerIndex) clearFairLocked(g *group) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !g.gateWaiting.Load() {
		return
	}
	g.gateWaiting.Store(false)
	idx.fairCount.Add(-1)
	if g.fairIndex >= 0 && g.fairIndex < len(idx.fair) && idx.fair[g.fairIndex] == g {
		idx.fair[g.fairIndex] = nil
		idx.fairDead++
	}
	if idx.fairDead > 0 && idx.fairDead >= len(idx.fair)/2 {
		idx.compactFairLocked()
	}
}

// compactFairLocked drops tombstoned entries and restores every live member's
// slice index. Callers hold idx.mu; index writes are guarded by that lock.
func (idx *providerIndex) compactFairLocked() {
	live := idx.fair[:0]
	for _, g := range idx.fair {
		if g == nil {
			continue
		}
		g.fairIndex = len(live)
		live = append(live, g)
	}
	clear(idx.fair[len(live):])
	idx.fair = live
	idx.fairDead = 0
}

// fairSnapshot returns the fair-waiting groups, rotated so the next drain
// starts after the group the last productive pass served. Only a group whose
// queued head the shared gate refused enters this set, so a release allocates
// and walks work that actually waits on that gate instead of every queued key.
// Tombstones are compacted here; callers must not hold idx.mu, and the group
// mutexes are only taken later by drainProvider after this snapshot is
// complete.
func (idx *providerIndex) fairSnapshot() []*group {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.fairDead > 0 {
		idx.compactFairLocked()
	}
	n := len(idx.fair)
	if n == 0 {
		idx.cursor = nil
		return nil
	}
	start := 0
	found := idx.cursor == nil
	if idx.cursor != nil {
		for i, g := range idx.fair {
			if g == idx.cursor {
				start = (i + 1) % n
				found = true
				break
			}
		}
	}
	if !found {
		// The last served group emptied or was detached; begin at the slice
		// head instead of leaving a stale anchor behind.
		idx.cursor = nil
	}
	out := make([]*group, 0, n)
	out = append(out, idx.fair[start:]...)
	out = append(out, idx.fair[:start]...)
	return out
}

// advanceFair publishes the last group a productive drain served. The next
// snapshot begins after it, so successive drains rotate through the fair set
// instead of favoring the first-attributed key. A drain that granted nothing
// never calls this.
func (idx *providerIndex) advanceFair(last *group) {
	idx.mu.Lock()
	idx.cursor = last
	idx.mu.Unlock()
}

// snapshotAll returns every attributed group, queued or idle, in list order.
// Policy changes use it to reset ramp epochs, and the anti-burst timer wake
// uses it to re-mark queued keys for one drain; the caller must not hold
// idx.mu.
func (idx *providerIndex) snapshotAll() []*group {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := make([]*group, 0, idx.live)
	for g := idx.head; g != nil; g = g.providerNext {
		out = append(out, g)
	}
	return out
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
		g = &group{sched: s, key: key, maxConcurrent: maxConcurrent, sendKick: make(chan struct{})}
		g.pacer.fn = func() {
			g.kickSend()
			g.drainAdmission()
		}
		s.adoptPacingLocked(g)
		s.groups[key] = g
		s.idleMu.Lock()
		s.idlePushLocked(g)
		s.idleMu.Unlock()
		return g
	}
	if applyCap && g.maxConcurrent != maxConcurrent {
		g.mu.Lock()
		g.maxConcurrent = maxConcurrent
		g.mu.Unlock()
	}
	return g
}

// attributeProviderLocked links g into the provider index on its first
// non-empty attribution. The caller must hold s.mu. Provider identity is
// written here and nowhere else, under s.mu (and under g.mu for admission and
// observer readers), so a concurrent drain either sees the group in the index
// or observes the group before this Acquire's admission attempt (see
// drainProvider's lost-wake argument). Internal group lookups
// (Trip/FailSend/WaitSend/BackoffFor) never attribute a provider; only an
// AcquireWith with hooks.Provider does.
//
// The group also adopts the index's queued-group counter and fair-waiting
// counter here, under g.mu (the established s.mu -> g.mu order). A group can
// already hold waiters when it is first attributed - an empty-provider Acquire
// can queue it behind an operator hold first - so count a pre-existing
// non-empty queue exactly once: its enqueue transition ran while providerQueued
// was nil, and every later transition is owned by syncQueueLocked.
func (s *Scheduler) attributeProviderLocked(g *group, provider string) {
	if provider == "" || g.provider != "" {
		return
	}
	idx := s.providerGroups[provider]
	if idx == nil {
		idx = &providerIndex{}
		s.providerGroups[provider] = idx
	}
	g.mu.Lock()
	// provider is read by admit/drain under g.mu, and attribution can race a
	// concurrent drain or ListThrottles for an already-listed group; publish
	// it under the group lock together with its index and counter pointers.
	g.provider = provider
	g.providerIdx = idx
	g.providerQueued = &idx.queued
	g.providerFair = &idx.fairCount
	if g.hasQueued.Load() {
		idx.queued.Add(1)
	}
	g.mu.Unlock()
	idx.mu.Lock()
	idx.linkLocked(g)
	idx.mu.Unlock()
}

// idlePushLocked appends g as the most recently idle group. Callers hold
// idleMu.
func (s *Scheduler) idlePushLocked(g *group) {
	if g.idleListed.Load() {
		return
	}
	g.idlePrev = s.idleTail
	g.idleNext = nil
	if s.idleTail != nil {
		s.idleTail.idleNext = g
	} else {
		s.idleHead = g
	}
	s.idleTail = g
	g.idleListed.Store(true)
}

// idleUnlinkLocked removes g from the idle order in O(1). Callers hold idleMu.
func (s *Scheduler) idleUnlinkLocked(g *group) bool {
	if !g.idleListed.Load() {
		return false
	}
	if g.idlePrev != nil {
		g.idlePrev.idleNext = g.idleNext
	} else {
		s.idleHead = g.idleNext
	}
	if g.idleNext != nil {
		g.idleNext.idlePrev = g.idlePrev
	} else {
		s.idleTail = g.idlePrev
	}
	g.idlePrev, g.idleNext = nil, nil
	g.idleListed.Store(false)
	return true
}

// markGroupBusyLocked removes g from the idle order when its first in-flight
// slot or queued waiter appears. Callers hold g.mu.
func (s *Scheduler) markGroupBusyLocked(g *group) {
	if !g.idleListed.Load() {
		return
	}
	s.idleMu.Lock()
	s.idleUnlinkLocked(g)
	s.idleMu.Unlock()
}

// markGroupIdleLocked appends g when its last live slot or waiter is gone and
// stamps the fully-idle instant the anti-burst ramp uses to decide whether the
// next first grant starts a new epoch. The stamp exists only while an epoch is
// live, so an inert or disabled policy adds no state to the group. A group
// with a remaining backlog is never idle, so a ramp epoch survives a busy
// drain. Callers hold g.mu.
func (s *Scheduler) markGroupIdleLocked(g *group) {
	if g.retired || g.inFlight != 0 || g.queued != 0 {
		return
	}
	if !g.rampStart.IsZero() && g.idleSince.IsZero() {
		g.idleSince = time.Now()
	}
	s.idleMu.Lock()
	s.idlePushLocked(g)
	s.idleMu.Unlock()
}

// popIdleGroupLocked removes and returns the least-recently-idle group, or nil
// when no group is idle.
func (s *Scheduler) popIdleGroupLocked() *group {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	g := s.idleHead
	if g == nil {
		return nil
	}
	s.idleUnlinkLocked(g)
	return g
}

// makeGroupRoomLocked evicts least-recently-idle groups until the group map has
// room for one new key or no idle candidate remains. Callers hold s.mu.
func (s *Scheduler) makeGroupRoomLocked() {
	for len(s.groups) >= s.groupCap {
		g := s.popIdleGroupLocked()
		if g == nil {
			return
		}
		if !s.evictIdleGroupLocked(g) {
			// The candidate acquired work after it was listed; it is no
			// longer idle, so continue with the next witness.
			continue
		}
	}
}

// evictIdleGroupLocked removes g from the group map and provider index after
// re-checking under g.mu that it has no in-flight slot and no queued waiter.
// The re-check is the authority: a stale Acquire that looked g up before this
// critical section either publishes work first (so eviction refuses) or sees
// the retired flag and retries against the map. Callers hold s.mu.
func (s *Scheduler) evictIdleGroupLocked(g *group) bool {
	g.mu.Lock()
	if g.retired || g.inFlight != 0 || g.queued != 0 {
		g.mu.Unlock()
		return false
	}
	g.retired = true
	// A retired group is idle by construction (queued == 0 already cleared its
	// fair mark through syncQueueLocked); clear defensively so a stale mark can
	// never keep an evicted group in the provider's fair set.
	g.clearGateWaitingLocked()
	// The group pacer's timer closure retains g; stop it so eviction cannot
	// leave one timer per evicted group alive for the pacing window.
	g.pacer.stop()
	pacing := pacingState{
		nextAllowedAt:  g.nextAllowedAt,
		backoff:        g.backoff,
		requestBackoff: g.requestBackoff,
		tripped:        g.tripped,
	}
	g.mu.Unlock()
	// A release that observed the last in-flight slot may have re-added g to
	// the idle order after the pop that selected it. Remove any such entry;
	// markGroupIdleLocked now sees retired and cannot add another.
	s.idleMu.Lock()
	s.idleUnlinkLocked(g)
	s.idleMu.Unlock()
	if s.groups[g.key] != g {
		// Defensive: a group no longer owning the map entry is unreachable.
		return false
	}
	delete(s.groups, g.key)
	s.detachProviderLocked(g)
	if !pacing.nextAllowedAt.IsZero() || pacing.backoff != 0 || pacing.requestBackoff != 0 || pacing.tripped {
		s.pacing.put(g.key, pacing)
	}
	return true
}

// detachProviderLocked unlinks g from its provider index and drops an index
// that has no live groups left, so a client rotating providers cannot retain
// one empty entry per provider. Callers hold s.mu.
func (s *Scheduler) detachProviderLocked(g *group) {
	idx := g.providerIdx
	if idx == nil {
		return
	}
	g.providerIdx = nil
	g.providerFair = nil
	idx.mu.Lock()
	idx.unlinkLocked(g)
	empty := idx.live == 0
	idx.mu.Unlock()
	if empty && s.providerGroups[g.provider] == idx {
		delete(s.providerGroups, g.provider)
	}
}
