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
// O(1), and a provider drain snapshots only groups whose queue is non-empty.
// cursor is the group a fair provider drain starts from next: queuedSnapshot
// rotates its result to begin there, and drainProvider advances it after a pass
// that served work, so repeated drains do not favor the first-attributed key.
// queued counts the indexed groups whose queue currently holds at least one
// live waiter. A provider drain reads it under Scheduler.mu as a witness that
// returns before walking (and locking) an idle provider's groups. Each group
// stores a pointer to this counter at its first attribution and adjusts it from
// the single queue-transition owner under the group's own mutex, so maintaining
// the count never needs Scheduler.mu.
type providerIndex struct {
	mu     sync.Mutex
	head   *group
	tail   *group
	live   int
	cursor *group // next fair-drain start; nil = list head. Guarded by mu.
	queued atomic.Int64
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
		// An idle group chosen as the next rotation start is going away. Begin
		// again at the head; it cannot hold queued work, so no turn is lost.
		idx.cursor = nil
	}
	idx.live--
}

// queuedSnapshot returns the groups whose lock-free witness says a live waiter
// is queued, rotated so the fair drain starts at idx.cursor. It allocates only
// for those groups, so a provider with many idle keys does not pay an
// allocation per drain. The cursor itself is advanced by drainProvider after a
// pass that actually granted work (advanceCursor), never by a no-op drain: a
// release fires both the gate wake and the group drain, and advancing on the
// redundant second call would rotate past the key that was just served and
// restore the old first-key bias. A cursor whose group was evicted or has
// emptied falls back to the next queued group after its list position, so
// rotation continues across idle gaps. Callers must not hold idx.mu.
func (idx *providerIndex) queuedSnapshot() []*group {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	cursor := idx.cursor
	passed := cursor == nil
	found := cursor == nil
	var after, before []*group
	for g := idx.head; g != nil; g = g.providerNext {
		if !passed {
			if g != cursor {
				continue
			}
			passed, found = true, true
		}
		if g.hasQueued.Load() {
			after = append(after, g)
		}
	}
	if cursor != nil && found {
		// Queued groups before the cursor follow it in the circular order.
		for g := idx.head; g != nil && g != cursor; g = g.providerNext {
			if g.hasQueued.Load() {
				before = append(before, g)
			}
		}
	} else if !found {
		// The cursor was evicted or detached. Rebuild in list order; the next
		// successful drain resumes rotation from whatever this one serves.
		after = after[:0]
		for g := idx.head; g != nil; g = g.providerNext {
			if g.hasQueued.Load() {
				after = append(after, g)
			}
		}
	}
	return append(after, before...)
}

// advanceCursor starts the next fair drain after the last group this snapshot
// served. served indexes the same snapshot the caller walked; the next group in
// rotation wraps to the snapshot start. Only a drain that granted work calls
// this, so empty or fully blocked drains leave the rotation untouched.
func (idx *providerIndex) advanceCursor(groups []*group, served int) {
	if len(groups) == 0 || served < 0 || served >= len(groups) {
		return
	}
	next := groups[(served+1)%len(groups)]
	idx.mu.Lock()
	idx.cursor = next
	idx.mu.Unlock()
}

// snapshotAll returns every attributed group, queued or idle, in list order.
// Policy changes use it to reset ramp epochs without walking the whole
// scheduler map; the caller must not hold idx.mu.
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
// The group also adopts the index's queued-group counter here, under g.mu (the
// established s.mu -> g.mu order). A group can already hold waiters when it is
// first attributed - an empty-provider Acquire can queue it behind an operator
// hold first - so count a pre-existing non-empty queue exactly once: its
// enqueue transition ran while providerQueued was nil, and every later
// transition is owned by syncQueueLocked.
func (s *Scheduler) attributeProviderLocked(g *group, provider string) {
	if provider == "" || g.provider != "" {
		return
	}
	idx := s.providerGroups[provider]
	if idx == nil {
		idx = &providerIndex{}
		s.providerGroups[provider] = idx
	}
	g.providerIdx = idx
	g.mu.Lock()
	// provider is read by admit/drain under g.mu, and attribution can race a
	// concurrent drain or ListThrottles for an already-listed group; publish
	// it under the group lock together with its counter pointer.
	g.provider = provider
	g.providerQueued = &idx.queued
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
	idx.mu.Lock()
	idx.unlinkLocked(g)
	empty := idx.live == 0
	idx.mu.Unlock()
	if empty && s.providerGroups[g.provider] == idx {
		delete(s.providerGroups, g.provider)
	}
}
