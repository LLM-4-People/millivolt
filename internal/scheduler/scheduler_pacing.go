package scheduler

import (
	"math/rand"
	"sync"
	"time"
)

// maxSchedulerPacingStates bounds the pacing/backoff state retained for groups
// removed by idle eviction. The LRU carries only a recent victim's small
// timing fields, never the group object, queue, provider link or pacer, so it
// cannot recreate the growth the group cap exists to bound.
const maxSchedulerPacingStates = 4_096

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
