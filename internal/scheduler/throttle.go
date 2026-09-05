package scheduler

import (
	"sort"
	"sync"
	"time"
)

// Throttle sources. The dashboard and the X-Proxy-Limit-* headers share
// SetThrottle; Source is what the UI shows for "who set this".
const (
	ThrottleSourceUI     = "ui"
	ThrottleSourceHeader = "header"
	// TokensUnavailable is a settlement state, not a token count. Retain the
	// admitted reservation when actual usage is unavailable; still free the
	// lease's concurrency slot. No replacement count is invented.
	TokensUnavailable int64 = -1
)

// Limit is the three per-provider caps. Zero on a dimension means that
// dimension is off (unlimited). A Limit with every dimension zero is
// inactive - SetThrottle then drops the provider from the map.
type Limit struct {
	Concurrency int           // in-flight requests across every key; 0 = off
	Requests    int64         // max requests per ReqWindow; 0 = off
	ReqWindow   time.Duration // window for Requests; ignored when Requests == 0
	Tokens      int64         // max tokens per TokWindow; 0 = off
	TokWindow   time.Duration // window for Tokens; ignored when Tokens == 0
}

// Active reports whether any dimension is on. Deny-by-default: a zero
// Limit is not a throttle.
func (l Limit) Active() bool {
	return l.Concurrency > 0 ||
		(l.Requests > 0 && l.ReqWindow > 0) ||
		(l.Tokens > 0 && l.TokWindow > 0)
}

func (l Limit) normalized() Limit {
	if l.Requests == 0 {
		l.ReqWindow = 0
	}
	if l.Tokens == 0 {
		l.TokWindow = 0
	}
	return l
}

// Throttle is one provider's persisted cap, plus who last wrote it.
type Throttle struct {
	Provider  string
	Limit     Limit
	Source    string // ThrottleSourceUI or ThrottleSourceHeader
	UpdatedBy string // client label (header) or "dashboard" (UI)
	UpdatedAt time.Time
}

// ThrottleInfo is Throttle plus live occupancy, for GET /admin/throttle
// and the dashboard. Remaining values are the token-bucket fill (0..cap).
type ThrottleInfo struct {
	Throttle
	InFlight     int     `json:"in_flight"`
	Queued       int     `json:"queued"`
	ReqRemaining float64 `json:"requests_remaining"`
	TokRemaining float64 `json:"tokens_remaining"`
	ReqCapacity  float64 `json:"requests_capacity"`
	TokCapacity  float64 `json:"tokens_capacity"`
}

// bucket is a token bucket (capacity = limit, refill = limit/window).
// tokens may go negative (debt) when a single request is larger than the
// remaining fill - that request is allowed; subsequent admits wait until
// refill brings the bucket back above zero. Internal, not a config knob.
type bucket struct {
	enabled  bool
	rate     float64 // units per second
	capacity float64
	tokens   float64
	last     time.Time
}

func (b *bucket) refill(now time.Time) {
	if !b.enabled || b.rate <= 0 {
		return
	}
	if b.last.IsZero() {
		b.last = now
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
}

func (b *bucket) reset(count int64, window time.Duration, now time.Time) {
	if count <= 0 || window <= 0 {
		b.enabled = false
		b.rate = 0
		b.capacity = 0
		b.tokens = 0
		b.last = time.Time{}
		return
	}
	cap := float64(count)
	rate := cap / window.Seconds()
	if b.enabled {
		b.refill(now)
		if cap > b.capacity {
			b.tokens += cap - b.capacity
			if b.tokens > cap {
				b.tokens = cap
			}
		} else if b.tokens > cap {
			b.tokens = cap // tighten: fail closed on the new burst
		}
	} else {
		b.tokens = cap // newly enabled: start full (don't punish past traffic)
	}
	b.enabled = true
	b.capacity = cap
	b.rate = rate
	b.last = now
}

func (b *bucket) waitFor(n float64) time.Duration {
	if !b.enabled || b.rate <= 0 {
		return 0
	}
	now := time.Now()
	b.refill(now)
	if b.tokens >= n {
		return 0
	}
	need := n - b.tokens
	d := time.Duration(need / b.rate * float64(time.Second))
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// providerGate is the live limiter for one provider. Policy (Throttle) and
// occupancy (inFlight, buckets) live together so SetThrottle and tryAdmit
// share one mutex. The map of gates is the single owner of provider limits.
type providerGate struct {
	mu       sync.Mutex
	throttle Throttle
	inFlight int
	reserved int64 // sum of in-flight token reservations (stats only)
	req      bucket
	tok      bucket
	tokEpoch uint64 // increments when the token dimension is enabled/disabled
	wake     wakeTimer
}

// admission is the immutable ownership proof captured while a gate grants.
// The gate owns concurrency; the token epoch identifies the bucket incarnation.
type admission struct {
	gate     *providerGate
	reserved int64
	tokEpoch uint64
}

func (g *providerGate) resetBuckets(l Limit, now time.Time) {
	if g.tok.enabled != (l.Tokens > 0 && l.TokWindow > 0) {
		g.tokEpoch++
	}
	g.req.reset(l.Requests, l.ReqWindow, now)
	g.tok.reset(l.Tokens, l.TokWindow, now)
}

func (g *providerGate) tryAdmit(est int64) (ok bool, lease admission, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.throttle.Limit.Active() {
		return true, admission{}, 0
	}
	now := time.Now()
	g.req.refill(now)
	g.tok.refill(now)
	lim := g.throttle.Limit
	if lim.Concurrency > 0 && g.inFlight >= lim.Concurrency {
		return false, admission{}, 0 // wait for release, not a timer
	}
	if g.req.enabled && g.req.tokens < 1 {
		return false, admission{}, g.req.waitFor(1)
	}
	var reserved int64
	if g.tok.enabled {
		need := float64(est)
		switch {
		case need <= 0:
			// No estimate: don't reserve. Block only when already empty
			// (settle will debit actual usage after the fact).
			if g.tok.tokens <= 0 {
				return false, admission{}, g.tok.waitFor(1)
			}
		case need > g.tok.capacity:
			// One request larger than the window cannot be fragmented.
			// Allow it if we are not already in debt; it exhausts the bucket.
			if g.tok.tokens <= 0 {
				return false, admission{}, g.tok.waitFor(1)
			}
			reserved = est
		case g.tok.tokens < need:
			return false, admission{}, g.tok.waitFor(need)
		default:
			reserved = est
		}
	}
	g.inFlight++
	if g.req.enabled {
		g.req.tokens--
	}
	if reserved > 0 {
		g.tok.tokens -= float64(reserved)
		g.reserved += reserved
	}
	return true, admission{gate: g, reserved: reserved, tokEpoch: g.tokEpoch}, 0
}

func (g *providerGate) snapshot(queued int) ThrottleInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.req.refill(now)
	g.tok.refill(now)
	info := ThrottleInfo{Throttle: g.throttle, InFlight: g.inFlight, Queued: queued}
	if g.req.enabled {
		info.ReqRemaining = g.req.tokens
		info.ReqCapacity = g.req.capacity
	}
	if g.tok.enabled {
		info.TokRemaining = g.tok.tokens
		info.TokCapacity = g.tok.capacity
	}
	return info
}

// SetThrottle replaces a provider cap, reporting whether its effective Limit
// changed. Source, UpdatedBy and UpdatedAt describe the last policy change,
// not the last request that repeated the same cap. Explicit installation
// timestamps are preserved; absent timestamps are filled only on a change.
func (s *Scheduler) SetThrottle(t Throttle) bool {
	return s.UpdateThrottle(t.Provider, func(Throttle) Throttle { return t })
}

// UpdateThrottle atomically reads, merges and installs a provider policy.
// update runs under the policy lock and must not block or call the scheduler.
// An inactive Limit clears the provider. Equal effective limits are a true
// no-op: no bucket reset, metadata update, timer cancellation or global kick.
func (s *Scheduler) UpdateThrottle(provider string, update func(Throttle) Throttle) bool {
	if provider == "" {
		return false
	}
	s.throttleMu.Lock()
	g := s.gates[provider]
	var cur Throttle
	if g != nil {
		g.mu.Lock()
		cur = g.throttle
	}
	t := update(cur)
	t.Provider = provider
	t.Limit = t.Limit.normalized()
	if cur.Limit == t.Limit || g == nil && !t.Limit.Active() {
		if g != nil {
			g.mu.Unlock()
		}
		s.throttleMu.Unlock()
		return false
	}
	if g != nil {
		g.wake.stop()
	}
	if !t.Limit.Active() {
		g.throttle = Throttle{}
		g.mu.Unlock()
		delete(s.gates, provider)
		s.throttleMu.Unlock()
		s.kickThrottles()
		return true
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now()
	}
	if g == nil {
		g = s.newProviderGate(provider)
		g.mu.Lock()
		if s.gates == nil {
			s.gates = make(map[string]*providerGate)
		}
		s.gates[provider] = g
	}
	g.throttle = t
	now := time.Now()
	g.resetBuckets(t.Limit, now)
	g.mu.Unlock()
	s.throttleMu.Unlock()
	s.kickThrottles()
	return true
}

// ClearThrottle drops the cap for provider (same as SetThrottle with a
// zero Limit). Unknown providers are a no-op.
func (s *Scheduler) ClearThrottle(provider string) {
	s.SetThrottle(Throttle{Provider: provider})
}

// ThrottleFor returns a copy of the current cap, or a zero Throttle.
func (s *Scheduler) ThrottleFor(provider string) Throttle {
	s.throttleMu.Lock()
	g := s.gates[provider]
	s.throttleMu.Unlock()
	if g == nil {
		return Throttle{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.throttle
}

// ListThrottles returns every active provider cap, sorted by provider,
// with live occupancy. The dashboard GET is this list.
func (s *Scheduler) ListThrottles() []ThrottleInfo {
	queued := s.throttleQueued()
	s.throttleMu.Lock()
	out := make([]ThrottleInfo, 0, len(s.gates))
	for p, g := range s.gates {
		out = append(out, g.snapshot(queued[p]))
	}
	s.throttleMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func (s *Scheduler) throttleQueued() map[string]int {
	out := map[string]int{}
	s.mu.Lock()
	groups := make([]*group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	s.mu.Unlock()
	for _, g := range groups {
		g.mu.Lock()
		for _, w := range g.queue {
			if w.provider != "" {
				out[w.provider]++
			}
		}
		g.mu.Unlock()
	}
	return out
}

// RestoreThrottles replaces the whole set (startup from SQLite). Does not
// kick waiters - there are none yet at attach time.
func (s *Scheduler) RestoreThrottles(ts []Throttle) {
	s.throttleMu.Lock()
	for _, g := range s.gates {
		g.wake.stop()
	}
	s.gates = make(map[string]*providerGate, len(ts))
	now := time.Now()
	for _, t := range ts {
		t.Limit = t.Limit.normalized()
		if t.Provider == "" || !t.Limit.Active() {
			continue
		}
		if t.UpdatedAt.IsZero() {
			t.UpdatedAt = now
		}
		g := s.newProviderGate(t.Provider)
		g.throttle = t
		g.resetBuckets(t.Limit, now)
		s.gates[t.Provider] = g
	}
	s.throttleMu.Unlock()
}

func (s *Scheduler) newProviderGate(provider string) *providerGate {
	g := &providerGate{}
	g.wake.fn = func() { s.drainProvider(provider) }
	return g
}

func (s *Scheduler) gate(provider string) *providerGate {
	if provider == "" {
		return nil
	}
	s.throttleMu.Lock()
	g := s.gates[provider]
	s.throttleMu.Unlock()
	return g
}

func (s *Scheduler) tryAdmit(provider string, est int64) (ok bool, lease admission, wait time.Duration) {
	g := s.gate(provider)
	if g == nil {
		return true, admission{}, 0
	}
	return g.tryAdmit(est)
}

func (s *Scheduler) throttleBlocked(provider string, est int64) bool {
	g := s.gate(provider)
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.throttle.Limit.Active() {
		return false
	}
	now := time.Now()
	g.req.refill(now)
	g.tok.refill(now)
	if g.throttle.Limit.Concurrency > 0 && g.inFlight >= g.throttle.Limit.Concurrency {
		return true
	}
	if g.req.enabled && g.req.tokens < 1 {
		return true
	}
	if g.tok.enabled {
		need := float64(est)
		switch {
		case need <= 0:
			if g.tok.tokens <= 0 {
				return true
			}
		case need > g.tok.capacity:
			if g.tok.tokens <= 0 {
				return true
			}
		case g.tok.tokens < need:
			return true
		}
	}
	return false
}

// finish settles and frees the gate captured at admission, never a fresh
// provider-name lookup. A cleared/reinstalled limit has a different owner;
// traffic admitted without a gate cannot charge a subsequently installed cap.
// Token enable/disable starts a new epoch even when concurrency keeps the gate;
// old reservations cannot debit/refund that new bucket. Enabled resizes retain
// their epoch so outstanding usage still reconciles against the same debt.
func (a admission) finish(actual int64) {
	g := a.gate
	if g == nil {
		return
	}
	g.mu.Lock()
	if a.reserved != 0 {
		g.reserved -= a.reserved
		if g.reserved < 0 {
			g.reserved = 0
		}
	}
	if g.tok.enabled && g.tokEpoch == a.tokEpoch {
		g.tok.refill(time.Now())
		// Negative counts are unavailable, including an invalid caller value.
		// The original reservation remains charged without a debit or refund.
		if actual >= 0 {
			g.tok.tokens -= float64(actual - a.reserved)
		}
	}
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.mu.Unlock()
	g.wake.fn() // shared provider drain, outside the gate lock
}

func (s *Scheduler) armThrottleDrain(provider string, d time.Duration) {
	g := s.gate(provider)
	if g == nil {
		return
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	g.mu.Lock()
	if g.throttle.Limit.Active() {
		g.wake.arm(time.Now().Add(d), true)
	}
	g.mu.Unlock()
}

func (s *Scheduler) drainProvider(provider string) {
	s.mu.Lock()
	groups := make([]*group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	s.mu.Unlock()
	for _, g := range groups {
		g.mu.Lock()
		match := provider == "" || g.provider == provider
		g.mu.Unlock()
		if match {
			g.drain()
		}
	}
}

func (s *Scheduler) kickThrottles() {
	s.pauseMu.Lock()
	if s.kick != nil {
		close(s.kick)
		s.kick = make(chan struct{})
	}
	s.pauseMu.Unlock()
	s.drainAll()
}

// Release settles actual usage and frees an acquisition exactly once, even
// when called concurrently. Pass zero for cancellation/unused reservations,
// or TokensUnavailable to retain the original reservation without settling
// a fabricated actual count.
// Every acquisition must be released, including one admitted without a cap.
type Release func(actualTokens int64)

func (g *group) slotRelease(a admission) Release {
	var once sync.Once
	return func(actual int64) {
		once.Do(func() {
			a.finish(actual)
			g.release()
		})
	}
}
