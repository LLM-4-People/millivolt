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

// Limit is the three per-provider caps plus the optional per-key anti-burst
// ramp. Zero on a cap dimension means that dimension is off (unlimited). A
// Limit with every cap dimension zero is inactive - SetThrottle then drops the
// provider from the map; the ramp fields alone never activate a provider.
//
// AntiBurst makes every provider+key group independently grow its concurrency
// from one up to Concurrency instead of allowing a fresh key to fill the whole
// provider allowance at once. It is off when false, and inert unless
// Concurrency >= 2 and RampDuration > 0. RampDuration is the time one group
// needs to reach the configured Concurrency target; zero = off. Both fields
// are policy, not live occupancy: Concurrency always stays the configured
// target, so persistence and the dashboard never observe a mid-ramp value.
type Limit struct {
	Concurrency  int           // in-flight requests across every key; 0 = off
	Requests     int64         // max requests per ReqWindow; 0 = off
	ReqWindow    time.Duration // window for Requests; ignored when Requests == 0
	Tokens       int64         // max tokens per TokWindow; 0 = off
	TokWindow    time.Duration // window for Tokens; ignored when Tokens == 0
	AntiBurst    bool          // per-key concurrency ramp; false = off
	RampDuration time.Duration // time to reach Concurrency on a new key; 0 = off
}

// Active reports whether any dimension is on. Deny-by-default: a zero
// Limit is not a throttle.
func (l Limit) Active() bool {
	return l.Concurrency > 0 ||
		(l.Requests > 0 && l.ReqWindow > 0) ||
		(l.Tokens > 0 && l.TokWindow > 0)
}

// rampActive reports whether the per-key anti-burst ramp is enabled by this
// limit: the flag is set and both dimensions can ramp. A zero or one
// concurrency target makes the ramp indistinguishable from the ordinary
// provider cap, and a zero duration leaves no slope.
func (l Limit) rampActive() bool {
	return l.AntiBurst && l.Concurrency >= 2 && l.RampDuration > 0
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

// Throttle is one operator limit policy: a selector, an optional schedule, a
// set of caps, and who last wrote it. ID is the canonical digest of Scope and
// Schedule; callers never supply or persist an identity of their own.
type Throttle struct {
	ID        string
	Scope     LimitScope
	Schedule  LimitSchedule
	Limit     Limit
	Source    string // ThrottleSourceUI or ThrottleSourceHeader
	UpdatedBy string // client label (header) or "dashboard" (UI)
	UpdatedAt time.Time
}

// ThrottleInfo is Throttle plus live occupancy, for GET /admin/throttle
// and the dashboard. ReqRemaining is the admits still available inside the
// trailing request window (0..capacity); TokRemaining is the token-bucket fill.
type ThrottleInfo struct {
	Throttle
	InFlight     int     `json:"in_flight"`
	Queued       int     `json:"queued"`
	ReqRemaining float64 `json:"requests_remaining"`
	TokRemaining float64 `json:"tokens_remaining"`
	ReqCapacity  float64 `json:"requests_capacity"`
	TokCapacity  float64 `json:"tokens_capacity"`
}

// policyTarget is the request identity matched against every active policy.
// It is built at admission from the resolved record, so a policy can select on
// any fact the proxy already owns without re-reading the request.
type policyTarget struct {
	Provider     string
	Model        string
	Client       string
	KeyHash      string
	Conversation string
}

// policyGate is the live limiter for one policy. The selector and its counters
// live together so install and admission share one mutex. The policy map is
// the single owner of operator limits.
type policyGate struct {
	mu       sync.Mutex
	throttle Throttle
	inFlight int
	reserved int64 // sum of in-flight token reservations (stats only)
	req      strictWindow
	tok      bucket
	tokEpoch uint64 // increments when the token dimension is enabled/disabled
	wake     wakeTimer
}

// admission is the immutable ownership proof captured while one or more gates
// grant. A request that matches several policies holds a lease in each, so
// every policy's occupancy and token debt settle exactly once.
type admission struct {
	leases []gateLease
}

type gateLease struct {
	gate     *policyGate
	reserved int64
	tokEpoch uint64
}

// active reports whether the policy applies to now. A schedule outside its
// window parks the policy without changing its counters: the request window
// prunes stale admits and the token bucket refills, so the next open window
// starts from observed traffic rather than an invented reset.
//
// The caller must hold the gate lock. An edit replaces throttle wholesale under
// that lock, so an unlocked read here races every policy update.
func (p *policyGate) active(now time.Time) bool {
	return p.throttle.Schedule.Active(now)
}

// maxWindowSlots bounds the memory a strictWindow may hold for its admitted
// stamps: one time.Time per slot, so 65536 slots is 1 MB. A configured count
// above this ceiling switches the limiter to even spacing, which is the same
// policy in O(1) memory (see strictWindow). Internal, not a config knob.
const maxWindowSlots = 65536

// strictWindow enforces a request budget as a SLIDING half-open window over
// the trailing Window: at most Count admits in any span (now-Window, now]. No
// window of the configured length ever contains more than Count admits, which
// is what a provider counting requests per rolling minute actually checks; a
// refilling bucket is allowed to emit its whole capacity and then refill
// continuously, so it can exceed Count inside one provider window (capacity in
// the first instant plus Count refilled over the same span).
//
// It keeps the stamps of the admits still inside the trailing window, so an
// idle provider banks nothing and a busy one cannot trade future allowance for
// a present burst. Slots are preallocated at the configured count. A count
// above maxWindowSlots is enforced by even spacing instead - the minimum-gap
// schedule admits one request every Window/Count and, being half-open, also
// never puts more than Count admits in any window of length Window.
//
// The two modes share one API, one policy and one window definition; new()
// picks the representation from the count alone, so no caller branches on it.
type strictWindow struct {
	enabled bool
	count   int64
	window  time.Duration
	// Sliding-log mode (count <= maxWindowSlots): stamps holds admitted
	// instants in arrival order, head is the next write index and filled is
	// how many slots hold a live stamp.
	stamps []time.Time
	head   int
	filled int
	// Pacing mode (count > maxWindowSlots): interval is Window/Count and next
	// is the earliest instant the next admit may use.
	interval time.Duration
	next     time.Time
}

// paced reports whether this budget is enforced by even spacing.
func (w *strictWindow) paced() bool {
	return w.enabled && w.interval > 0
}

func (w *strictWindow) reset(count int64, window time.Duration, now time.Time) {
	if count <= 0 || window <= 0 {
		*w = strictWindow{}
		return
	}
	w.enabled = true
	w.count = count
	w.window = window
	w.stamps = nil
	w.head = 0
	w.filled = 0
	w.interval = 0
	w.next = time.Time{}
	if count > maxWindowSlots {
		// Spacing must satisfy the same half-open invariant the sliding log
		// enforces, and the rule is exact rather than approximate: admits are
		// spaced by at least interval, so a window of length Window holds at
		// most floor(Window/interval)+1 of them. That stays at or below Count
		// exactly when interval > Window/Count, which one extra nanosecond on
		// the integer quotient guarantees unconditionally.
		w.interval = window/time.Duration(count) + 1
		w.next = now
		return
	}
	w.stamps = make([]time.Time, count)
}

// prune drops the stamps that have aged out of the trailing window. It is the
// only place the log forgets, so every reader sees the same window contents.
// The window is half-open, (now-Window, now]: a stamp exactly Window old is
// outside it, which is what keeps Count admits from pairing with a Count+1th
// at the shared boundary.
func (w *strictWindow) prune(now time.Time) {
	if !w.enabled || w.paced() {
		return
	}
	cutoff := now.Add(-w.window)
	for w.filled > 0 {
		oldest := w.stamps[(w.head-w.filled+len(w.stamps))%len(w.stamps)]
		if oldest.After(cutoff) {
			break
		}
		w.filled--
	}
}

// waitFor reports how long until one more admit fits inside the trailing
// window, and whether the budget blocks right now. A zero wait with false
// means admit immediately.
func (w *strictWindow) waitFor(now time.Time) (time.Duration, bool) {
	if !w.enabled {
		return 0, false
	}
	if w.paced() {
		if now.Before(w.next) {
			return w.next.Sub(now), true
		}
		return 0, false
	}
	w.prune(now)
	if int64(w.filled) < w.count {
		return 0, false
	}
	// Full: the next admit becomes legal when the oldest live stamp leaves.
	oldest := w.stamps[(w.head-w.filled+len(w.stamps))%len(w.stamps)]
	d := oldest.Add(w.window).Sub(now)
	if d < 0 {
		d = 0
	}
	return d, true
}

// admit records one admitted request at now. Callers must have checked
// waitFor first; admit never refuses.
func (w *strictWindow) admit(now time.Time) {
	if !w.enabled {
		return
	}
	if w.paced() {
		if w.next.Before(now) {
			w.next = now
		}
		w.next = w.next.Add(w.interval)
		return
	}
	w.prune(now)
	w.stamps[w.head] = now
	w.head = (w.head + 1) % len(w.stamps)
	if w.filled < len(w.stamps) {
		w.filled++
	}
}

// remaining reports the admits still available inside the trailing window.
// Pacing mode holds no stamps, so it reports the whole budget when the next
// slot is already legal and nothing until then; the count there is large
// enough that the figure is a readiness signal rather than a gauge.
func (w *strictWindow) remaining(now time.Time) float64 {
	if !w.enabled {
		return 0
	}
	if w.paced() {
		if now.Before(w.next) {
			return 0
		}
		return float64(w.count)
	}
	w.prune(now)
	return float64(w.count - int64(w.filled))
}

// capacity reports the configured budget for display.
func (w *strictWindow) capacity() float64 {
	if !w.enabled {
		return 0
	}
	return float64(w.count)
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

// maxBucketWait is the ceiling on one bucket waitFor: the exact refill time
// of a deep debt (need/rate) can exceed time.Duration's ~292-year span, and
// the naive float-to-Duration conversion overflows to minInt64 - the
// sub-millisecond floor would then clamp it to ~1ms and armThrottleDrain
// would re-arm about a thousand times per second while the provider stays
// token-blocked. The ceiling mirrors MaxRetryHint, the daily-quota window
// maxLimitWindow is pinned to: a larger debt wakes once per ceiling and
// re-arms, while settles and cap changes re-check admission anyway - the
// timer is only a fallback heartbeat.
const maxBucketWait = MaxRetryHint

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
	sec := need / b.rate
	if sec > maxBucketWait.Seconds() {
		sec = maxBucketWait.Seconds()
	}
	d := time.Duration(sec * float64(time.Second))
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

func (g *policyGate) resetBuckets(l Limit, now time.Time) {
	if g.tok.enabled != (l.Tokens > 0 && l.TokWindow > 0) {
		g.tokEpoch++
	}
	g.req.reset(l.Requests, l.ReqWindow, now)
	g.tok.reset(l.Tokens, l.TokWindow, now)
}

// blockedLocked reports whether this gate refuses a request estimated at est
// tokens right now, and how long until the blocking dimension could admit
// (0 when the concurrency cap blocks: that waits for a release, not a timer).
// The caller must hold g.mu: the window prune and the token bucket refill it
// performs are the same mutations an admitting caller then relies on.
func (g *policyGate) blockedLocked(est int64) (bool, time.Duration) {
	if !g.throttle.Limit.Active() {
		return false, 0
	}
	now := time.Now()
	g.tok.refill(now)
	lim := g.throttle.Limit
	if lim.Concurrency > 0 && g.inFlight >= lim.Concurrency {
		return true, 0 // wait for release, not a timer
	}
	if wait, blocked := g.req.waitFor(now); blocked {
		return true, wait
	}
	if g.tok.enabled {
		need := float64(est)
		switch {
		case need <= 0:
			// No estimate: don't reserve. Block only when already empty
			// (settle will debit actual usage after the fact).
			if g.tok.tokens <= 0 {
				return true, g.tok.waitFor(1)
			}
		case need > g.tok.capacity:
			// One request larger than the window cannot be fragmented.
			// Allow it if we are not already in debt; it exhausts the bucket.
			if g.tok.tokens <= 0 {
				return true, g.tok.waitFor(1)
			}
		case g.tok.tokens < need:
			return true, g.tok.waitFor(need)
		}
	}
	return false, 0
}

// admitLocked commits one admission after blockedLocked returned false. The
// caller holds g.mu and owns the matching check, so an inactive or
// out-of-schedule policy is never charged.
func (g *policyGate) admitLocked(est int64) gateLease {
	var reserved int64
	if g.tok.enabled && est > 0 {
		reserved = est
	}
	g.inFlight++
	g.req.admit(time.Now())
	if reserved > 0 {
		g.tok.tokens -= float64(reserved)
		g.reserved += reserved
	}
	return gateLease{gate: g, reserved: reserved, tokEpoch: g.tokEpoch}
}

func (g *policyGate) snapshot(queued int) ThrottleInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.tok.refill(now)
	info := ThrottleInfo{Throttle: g.throttle, InFlight: g.inFlight, Queued: queued}
	info.ReqRemaining = g.req.remaining(now)
	info.ReqCapacity = g.req.capacity()
	if g.tok.enabled {
		info.TokRemaining = g.tok.tokens
		info.TokCapacity = g.tok.capacity
	}
	return info
}

// SetPolicy installs one complete policy and reports whether anything changed.
// It is the write path used by restore and by a caller that already owns the
// full document. Id is derived from the scope and schedule.
func (s *Scheduler) SetPolicy(t Throttle) bool {
	id := PolicyID(t.Scope, t.Schedule)
	changed, _ := s.UpdatePolicy(id, func(Throttle) Throttle { return t })
	return changed
}

// UpdatePolicy atomically reads, merges and installs one policy. update runs
// under the policy lock and must not block or call the scheduler. The returned
// policy carries the final scope and schedule, so the update function may
// narrow, widen or re-schedule the selector. A changed identity replaces the
// old policy rather than carrying its counters under a new name, unless the new
// identity already belongs to another policy: that is a conflict, reported
// without mutating either policy, so an edit can never silently delete a
// neighbour. An inactive Limit removes the policy. Equal effective policies are
// a true no-op: no bucket reset, metadata update, timer cancellation or global
// kick.
func (s *Scheduler) UpdatePolicy(id string, update func(Throttle) Throttle) (changed bool, conflict bool) {
	s.throttleMu.Lock()
	var old *policyGate
	if id != "" {
		old = s.gates[id]
	}
	var cur Throttle
	if old != nil {
		old.mu.Lock()
		cur = old.throttle
	}
	t := update(cur)
	t.Scope = NormalizeLimitScope(t.Scope)
	t.Schedule = NormalizeLimitSchedule(t.Schedule)
	t.Limit = t.Limit.normalized()
	t.ID = PolicyID(t.Scope, t.Schedule)
	if !t.Limit.Active() || t.ID == "" {
		if old == nil {
			s.throttleMu.Unlock()
			return false, false
		}
		old.wake.stop()
		old.throttle = Throttle{}
		old.mu.Unlock()
		delete(s.gates, id)
		s.publishPolicyLocked()
		s.throttleMu.Unlock()
		s.resetAllRamps()
		s.kickThrottles()
		return true, false
	}
	if old != nil && cur.Limit == t.Limit && cur.ID == t.ID {
		old.mu.Unlock()
		s.throttleMu.Unlock()
		return false, false
	}
	// An edit that resolves to another policy's identity would replace that
	// policy and lose its counters and metadata. Refuse it instead: the caller
	// must remove or re-scope the other rule explicitly.
	if t.ID != id {
		if existing := s.gates[t.ID]; existing != nil && existing != old {
			if old != nil {
				old.mu.Unlock()
			}
			s.throttleMu.Unlock()
			return false, true
		}
	}
	// A ramp shape change invalidates every group's epoch: the target and the
	// slope are different, so an old epoch would grant the wrong allowance.
	// Token/request-only edits keep the ramp untouched.
	rampChanged := cur.Limit.Concurrency != t.Limit.Concurrency ||
		cur.Limit.AntiBurst != t.Limit.AntiBurst ||
		cur.Limit.RampDuration != t.Limit.RampDuration
	identityChanged := old != nil && cur.ID != t.ID
	if old != nil {
		old.wake.stop()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now()
	}
	if old != nil && !identityChanged {
		old.throttle = t
		old.resetBuckets(t.Limit, time.Now())
		old.mu.Unlock()
		s.publishPolicyLocked()
		s.throttleMu.Unlock()
		if rampChanged {
			s.resetAllRamps()
		}
		s.kickThrottles()
		return true, false
	}
	if len(s.gates) >= maxProviderGates {
		// Header-driven limits are client-controlled. Beyond the cap a new
		// selector is ignored instead of growing the policy map forever.
		if old != nil {
			old.mu.Unlock()
		}
		s.throttleMu.Unlock()
		return false, false
	}
	if old != nil {
		old.throttle = Throttle{}
		old.mu.Unlock()
		delete(s.gates, id)
	}
	g := s.newPolicyGate(t)
	g.throttle = t
	g.resetBuckets(t.Limit, time.Now())
	if s.gates == nil {
		s.gates = make(map[string]*policyGate)
	}
	s.gates[t.ID] = g
	s.publishPolicyLocked()
	s.throttleMu.Unlock()
	s.resetAllRamps()
	s.kickThrottles()
	return true, false
}

// RemovePolicy deletes one policy by id.
func (s *Scheduler) RemovePolicy(id string) bool {
	changed, _ := s.UpdatePolicy(id, func(Throttle) Throttle { return Throttle{} })
	return changed
}

// PolicyByID returns one policy's current definition.
func (s *Scheduler) PolicyByID(id string) (Throttle, bool) {
	s.throttleMu.Lock()
	defer s.throttleMu.Unlock()
	g := s.gates[id]
	if g == nil {
		return Throttle{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.throttle, true
}

// ThrottlePolicies returns every active policy without live occupancy, sorted
// by identity. Durable persistence stores only policy, so it must not pay
// ListThrottles' scan of every group and queued waiter. Every policy write
// holds throttleMu, so reading g.throttle under that lock is race-free and
// never takes a group mutex. Throttle has only value fields, so the returned
// copies cannot alias later gate updates.
func (s *Scheduler) ThrottlePolicies() []Throttle {
	s.throttleMu.Lock()
	out := make([]Throttle, 0, len(s.gates))
	for _, g := range s.gates {
		out = append(out, g.throttle)
	}
	s.throttleMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ListThrottles returns every active policy with live occupancy. The dashboard
// GET is this list. Persistence uses the policy-only ThrottlePolicies instead.
func (s *Scheduler) ListThrottles() []ThrottleInfo {
	queued := s.policyQueued()
	s.throttleMu.Lock()
	out := make([]ThrottleInfo, 0, len(s.gates))
	for id, g := range s.gates {
		out = append(out, g.snapshot(queued[id]))
	}
	s.throttleMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// policyQueued counts every parked request a policy covers: admission waiters
// plus send-gate waiters. A waiter can be covered by several policies, so each
// matching policy reports it; the console labels the number as the policy's
// own queue, not a partition of the scheduler's total.
func (s *Scheduler) policyQueued() map[string]int {
	out := map[string]int{}
	snap := s.policySnap.Load()
	if snap == nil || len(snap.all) == 0 && len(snap.byProvider) == 0 {
		return out
	}
	for _, g := range s.snapshotGroups() {
		g.mu.Lock()
		count := func(provider, model, client, keyHash, conversation string) {
			snap.each(provider, func(entry *policyEntry) {
				if entry.matches(provider, model, client, keyHash, conversation) {
					out[entry.id]++
				}
			})
		}
		g.queue.forEach(func(w *waiter) {
			count(w.provider, w.model, w.client, w.keyHash, w.conversation)
		})
		if g.provider != "" && g.sendWaiters > 0 {
			// Send-gate waiters are not enumerated here; attribute the count to
			// every policy that can select the group's provider.
			snap.each(g.provider, func(entry *policyEntry) {
				out[entry.id] += g.sendWaiters
			})
		}
		g.mu.Unlock()
	}
	return out
}

// RestoreThrottles replaces the whole set (startup from SQLite). Does not
// kick waiters - there are none yet at attach time. Ramp epochs are reset
// after installation: at startup no group exists yet, and a live restore in a
// test or reload must not continue an epoch owned by the previous policy.
func (s *Scheduler) RestoreThrottles(ts []Throttle) {
	s.throttleMu.Lock()
	for _, g := range s.gates {
		g.wake.stop()
	}
	s.gates = make(map[string]*policyGate, len(ts))
	now := time.Now()
	for _, t := range ts {
		t.Scope = NormalizeLimitScope(t.Scope)
		t.Schedule = NormalizeLimitSchedule(t.Schedule)
		t.Limit = t.Limit.normalized()
		if !t.Limit.Active() {
			continue
		}
		t.ID = PolicyID(t.Scope, t.Schedule)
		if t.UpdatedAt.IsZero() {
			t.UpdatedAt = now
		}
		g := s.newPolicyGate(t)
		g.throttle = t
		g.resetBuckets(t.Limit, now)
		s.gates[t.ID] = g
	}
	s.publishPolicyLocked()
	s.throttleMu.Unlock()
	s.resetAllRamps()
}

// resetAllRamps clears every attributed group's ramp epoch. RestoreThrottles
// replaces the entire policy set and a shaped edit can change which groups a
// selector covers, so no installed epoch survives either.
func (s *Scheduler) resetAllRamps() {
	s.mu.Lock()
	idxs := make([]*providerIndex, 0, len(s.providerGroups))
	for _, idx := range s.providerGroups {
		idxs = append(idxs, idx)
	}
	s.mu.Unlock()
	for _, idx := range idxs {
		resetRamps(idx.snapshotAll())
	}
}

func resetRamps(groups []*group) {
	for _, g := range groups {
		g.mu.Lock()
		g.resetRampLocked()
		g.mu.Unlock()
	}
}

// maxProviderGates bounds header-driven policies. Restored policies are
// loaded explicitly and are not subject to the header-creation path.
const maxProviderGates = 4096

// newPolicyGate binds the shared provider wake to the policy's selector. A
// policy that names providers wakes only those providers' fair sets; a global
// policy wakes every group because it can block any of them.
func (s *Scheduler) newPolicyGate(t Throttle) *policyGate {
	g := &policyGate{}
	providers := append([]string(nil), t.Scope.Providers...)
	g.wake.fn = func() {
		if len(providers) == 0 {
			s.drainAll()
			return
		}
		for _, provider := range providers {
			s.drainProviderWake(provider)
		}
	}
	return g
}

// policyIndex is the immutable read view of the policy map: a list for
// provider-independent policies and one list per named provider. Matching a
// request is a map lookup plus a short scan, never a scan of every policy.
//
// Each entry carries the selector and schedule it was published with. Those
// two are immutable between publications, while the gate's throttle document
// is replaced under its own lock on every edit, so a reader that tested
// g.throttle.Scope here would race every policy update.
type policyIndex struct {
	all        []*policyEntry
	byProvider map[string][]*policyEntry
}

// policyEntry is one published policy: the gate plus the selector and
// schedule that decide whether it applies. Published entries are never
// mutated; an edit builds a new index.
type policyEntry struct {
	gate     *policyGate
	id       string
	scope    LimitScope
	schedule LimitSchedule
}

// matches reports whether this published selector covers a request. It reads
// only immutable snapshot fields.
func (e *policyEntry) matches(provider, model, client, keyHash, conversation string) bool {
	return e.scope.Matches(provider, model, client, keyHash, conversation)
}

// providerOnly reports whether the policy names only providers, with no other
// constraint and no schedule. It reads only immutable snapshot fields.
func (e *policyEntry) providerOnly() bool {
	return e.scope.ProviderOnly() && e.schedule.Empty()
}

// activeFor reports whether the published schedule is open at now.
func (e *policyEntry) activeFor(now time.Time) bool {
	return e.schedule.Active(now)
}

// each calls fn for every policy that can select provider.
func (idx *policyIndex) each(provider string, fn func(*policyEntry)) {
	if idx == nil {
		return
	}
	for _, entry := range idx.all {
		fn(entry)
	}
	if provider == "" {
		return
	}
	for _, entry := range idx.byProvider[provider] {
		fn(entry)
	}
}

// publishPolicyLocked replaces the immutable read view. Callers hold
// throttleMu; readers only load the pointer, so a hot-path admission never
// waits for a policy writer.
func (s *Scheduler) publishPolicyLocked() {
	idx := &policyIndex{byProvider: make(map[string][]*policyEntry)}
	ids := make([]string, 0, len(s.gates))
	for id := range s.gates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := s.gates[id]
		entry := &policyEntry{
			gate: g, id: g.throttle.ID,
			scope: g.throttle.Scope, schedule: g.throttle.Schedule,
		}
		if len(entry.scope.Providers) == 0 {
			idx.all = append(idx.all, entry)
			continue
		}
		for _, provider := range entry.scope.Providers {
			idx.byProvider[provider] = append(idx.byProvider[provider], entry)
		}
	}
	s.policySnap.Store(idx)
}

// tryAdmit evaluates every policy that covers target. All matching policies
// must admit: the request takes one lease in each, so every cap is charged and
// settled exactly once. Gates are locked in identity order, and no other path
// holds two policy locks, so the set lock cannot deadlock. A refusal releases
// every lock without charging anything; wait is the earliest timer that could
// reopen a refusing dimension (0 when only a release can).
func (s *Scheduler) tryAdmit(target policyTarget, est int64) (ok bool, lease admission, wait time.Duration) {
	idx := s.policySnap.Load()
	if idx == nil {
		return true, admission{}, 0
	}
	var entries []*policyEntry
	idx.each(target.Provider, func(entry *policyEntry) {
		if entry.matches(target.Provider, target.Model, target.Client, target.KeyHash, target.Conversation) {
			entries = append(entries, entry)
		}
	})
	if len(entries) == 0 {
		return true, admission{}, 0
	}
	// Lock in published identity order. The ids come from the immutable
	// snapshot, so the sort never reads a gate's mutable document.
	if len(entries) > 1 {
		sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	}
	gates := make([]*policyGate, 0, len(entries))
	for _, entry := range entries {
		gates = append(gates, entry.gate)
	}
	now := time.Now()
	for _, g := range gates {
		g.mu.Lock()
	}
	// A policy outside its schedule is skipped entirely: it neither blocks nor
	// charges, and its counters resume from real traffic when the window opens.
	active := make([]*policyGate, 0, len(gates))
	for _, entry := range entries {
		if entry.activeFor(now) && entry.gate.throttle.Limit.Active() {
			active = append(active, entry.gate)
		}
	}
	if len(active) == 0 {
		for _, g := range gates {
			g.mu.Unlock()
		}
		return true, admission{}, 0
	}
	blocked := false
	minWait := time.Duration(0)
	for _, g := range active {
		if isBlocked, gateWait := g.blockedLocked(est); isBlocked {
			blocked = true
			if gateWait > 0 && (minWait <= 0 || gateWait < minWait) {
				minWait = gateWait
			}
		}
	}
	if blocked {
		for _, g := range gates {
			g.mu.Unlock()
		}
		return false, admission{}, minWait
	}
	leases := make([]gateLease, 0, len(active))
	for _, g := range active {
		leases = append(leases, g.admitLocked(est))
	}
	for _, g := range gates {
		g.mu.Unlock()
	}
	return true, admission{leases: leases}, 0
}

// providerConcurrencyFull reports whether a provider-wide policy has no free
// concurrency slot left. Only a policy with no model, client, key or
// conversation constraint can prove that no request to this provider can be
// admitted; a scoped policy never suppresses the drain, which then re-checks
// the full selector set. The read takes one gate lock at a time and never
// mutates a bucket.
func (s *Scheduler) providerConcurrencyFull(provider string) bool {
	idx := s.policySnap.Load()
	if idx == nil {
		return false
	}
	now := time.Now()
	full := false
	idx.each(provider, func(entry *policyEntry) {
		if full || !entry.providerOnly() || !entry.activeFor(now) {
			return
		}
		// Only occupancy and the cap belong to the gate; the selector and the
		// schedule came from the immutable snapshot.
		entry.gate.mu.Lock()
		limit := entry.gate.throttle.Limit
		full = limit.Concurrency > 0 && entry.gate.inFlight >= limit.Concurrency
		entry.gate.mu.Unlock()
	})
	return full
}

// settle reconciles every lease captured at admission, never a fresh policy
// lookup, and does not drain: the caller owns exactly one drain after it has
// also released the group's slot. Settling first keeps reserved tokens, the
// token bucket and gate in-flight occupancy authoritative before a queued
// waiter can consume the freed capacity. A cleared/reinstalled policy has a
// different owner; traffic admitted without a gate cannot charge a
// subsequently installed cap. Token enable/disable starts a new epoch even when
// concurrency keeps the policy; old reservations cannot debit/refund that new
// bucket. Enabled resizes retain their epoch so outstanding usage still
// reconciles against the same debt. An empty lease set is a no-op and the
// caller still drains the group once.
func (a admission) settle(actual int64) {
	for _, lease := range a.leases {
		g := lease.gate
		if g == nil {
			continue
		}
		g.mu.Lock()
		if lease.reserved != 0 {
			g.reserved -= lease.reserved
			if g.reserved < 0 {
				g.reserved = 0
			}
		}
		if g.tok.enabled && g.tokEpoch == lease.tokEpoch {
			g.tok.refill(time.Now())
			// Negative counts are unavailable, including an invalid caller value.
			// The original reservation remains charged without a debit or refund.
			if actual >= 0 {
				g.tok.tokens -= float64(actual - lease.reserved)
			}
		}
		if g.inFlight > 0 {
			g.inFlight--
		}
		g.mu.Unlock()
	}
}

// armThrottleDrain arms every policy that can select provider. A request
// admitted to this provider may be blocked by any matching selector, so every
// such policy owns a timer wake. Arming an earlier deadline than a particular
// policy needs is harmless: the wake re-runs the provider's fair drain, which
// re-checks the full selector set.
func (s *Scheduler) armThrottleDrain(provider string, d time.Duration) {
	if d < time.Millisecond {
		d = time.Millisecond
	}
	idx := s.policySnap.Load()
	if idx == nil {
		return
	}
	at := time.Now().Add(d)
	idx.each(provider, func(entry *policyEntry) {
		entry.gate.mu.Lock()
		if entry.gate.throttle.Limit.Active() {
			entry.gate.wake.arm(at, true)
		}
		entry.gate.mu.Unlock()
	})
}

// drainProvider wakes only the groups that wait on provider's shared gate. It
// reads the index's lock-free fair-waiting count under mu, releases mu before
// touching any group, and returns immediately when no group is marked. The
// snapshot copies only fair-waiting groups, so a release allocates and walks
// work that the shared gate actually refused instead of every queued key: a
// peer parked on its own pacing window, hold, anti-burst ramp or per-key cap is
// not in this set and costs nothing. Preserving the s.mu -> g.mu order every
// other path uses: no path takes g.mu and then waits for s.mu; the index lock
// is released before any group drain, and marking takes g.mu -> providerIndex.mu
// only.
//
// Admission is round-robin across the fair-waiting groups, not group-by-group
// to exhaustion. The snapshot starts after the group the last productive pass
// served, and each pass grants at most one waiter per group before wrapping. A
// single freed provider slot therefore goes to the next waiting group instead
// of forever to the group whose key was attributed first. A group that stops on
// a per-key predicate withdraws its mark during the pass and is skipped by the
// remaining passes; a group still refused by the gate keeps its mark. Held and
// cancelled waiters keep their FIFO place without consuming the one-grant
// budget.
//
// The walk is bounded by the provider's concurrency cap, not by the backlog. A
// cap that is already full before the pass cannot grant anyone, so the drain
// returns without taking a single group mutex. Once a grant refills the cap the
// whole drain stops and the cursor moves past the granting group, because the
// remaining fair-waiting keys would only be refused: their marks stay for the
// release or bucket refill that opens the next slot. A provider without a
// concurrency cap keeps walking until a pass grants nothing, and a drain with
// several free slots keeps granting across the set until the last one is
// refilled.
//
// A release cannot lose a wake to a group created concurrently with it: the
// new group is linked, its provider and index pointers published under mu
// before its Acquire attempts admission or enqueues, and a refused admission
// marks it under g.mu. A drain that misses it therefore ran before that
// admission attempt; if the attempt then blocks, either its own enqueue drain
// runs after the mark, its in-flight slot holders release later, or a
// token/request bucket arms the provider timer that re-runs this drain. A
// waiter that enqueues after this drain's count check is covered by its own
// post-enqueue fair drain (which marks it first) or by the later release that
// observes the count the mark published. Marking publishes the flag and the
// count under g.mu + providerIndex.mu before the caller drains, so the fast
// path either observes the peer in the count or ran before the mark existed; a
// cleared group cannot retain a positive count.
//
// Eviction cannot remove a group whose queue holds a live waiter, so a marked
// group stays linked across this snapshot.
//
// Empty provider identity has no gate of its own (gate("") is nil); callers
// that do reach here with an empty provider keep the previous full-drain
// fallback instead of silently skipping every group. An unknown non-empty
// provider has no attributed groups and correctly drains nothing.
func (s *Scheduler) drainProvider(provider string) {
	if provider == "" {
		s.drainAll()
		return
	}
	s.mu.Lock()
	idx := s.providerGroups[provider]
	if idx == nil || idx.fairCount.Load() == 0 {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	// No admission can fit while the cap is full, so the fair set is walked
	// only when a slot is free. A release settles its own admission before
	// calling here, so the drain that hands its freed slot on is never skipped;
	// a drain that is skipped leaves every mark in place for that next release
	// or for the bucket timer.
	if s.providerConcurrencyFull(provider) {
		return
	}
	groups := idx.fairSnapshot()
	if len(groups) == 0 {
		return
	}
	// Passes repeat only while they make progress and the cap still has room. A
	// pass that grants nothing means every fair-waiting group is blocked by the
	// provider gate or by its own cap, ramp or hold; another pass would observe
	// the same state. A grant that refills the concurrency cap ends the whole
	// drain. New enqueues own their enqueue drain.
	served := -1
	for {
		granted := 0
		last := -1
		full := false
		for i, g := range groups {
			if !g.gateWaiting.Load() || !g.hasQueued.Load() {
				continue
			}
			if g.drainLimited(1) > 0 {
				granted++
				last = i
				if s.providerConcurrencyFull(provider) {
					// The freed slot is reassigned. Every remaining fair
					// waiter needs a concurrency slot too, so this pass cannot
					// admit another one: stop here, keep their marks, and let
					// the cursor start the next drain after this group.
					served = i
					full = true
					break
				}
			}
		}
		if full || granted == 0 {
			break
		}
		served = last
	}
	if served >= 0 {
		// Rotate only after real service, so a drain that only withdrew stale
		// marks cannot bias the next start.
		idx.advanceFair(groups[served])
	}
}

// drainProviderWake is the shared provider timer's wake. A bucket refill or an
// anti-burst step is not tied to one group, so it cannot prove which queued key
// it unblocks. A ramp-blocked group is deliberately absent from the fair set
// (the ramp is per-key, and its presence must not suppress another key's fast
// path), so before draining, every queued group is optimistically marked for
// this one pass; the drain withdraws every mark that still stops on a per-key
// predicate. Providers without a ramp keep relying on their already-marked
// gate waiters.
func (s *Scheduler) drainProviderWake(provider string) {
	if s.providerRamps(provider) {
		s.markQueuedProviderGroups(provider)
	}
	s.drainProvider(provider)
}

// markQueuedProviderGroups marks every group of provider that holds a live
// waiter, for one fair drain. It snapshots the list before taking any group
// mutex, so the providerIndex lock is never held while waiting for g.mu.
func (s *Scheduler) markQueuedProviderGroups(provider string) {
	s.mu.Lock()
	idx := s.providerGroups[provider]
	s.mu.Unlock()
	if idx == nil || idx.queued.Load() == 0 {
		return
	}
	for _, g := range idx.snapshotAll() {
		if !g.hasQueued.Load() {
			continue
		}
		g.mu.Lock()
		if g.queue.live > 0 {
			g.markGateWaitingLocked()
		}
		g.mu.Unlock()
	}
}

// providerRamps reports whether any policy selecting provider has an active
// anti-burst ramp, the only timer wake that can unblock a group deliberately
// left out of the fair set. The gates come from the atomic policy snapshot, so
// this never takes throttleMu.
func (s *Scheduler) providerRamps(provider string) bool {
	idx := s.policySnap.Load()
	if idx == nil {
		return false
	}
	ramped := false
	idx.each(provider, func(entry *policyEntry) {
		if ramped {
			return
		}
		entry.gate.mu.Lock()
		ramped = entry.gate.throttle.Limit.rampActive()
		entry.gate.mu.Unlock()
	})
	return ramped
}

func (s *Scheduler) kickThrottles() {
	s.pauseMu.Lock()
	s.kickPolicyLocked()
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
			// Settle the gate before freeing the group slot: the drain that
			// release() runs then observes correct token and concurrency
			// accounting, and it is the only drain for this release.
			a.settle(actual)
			g.release()
		})
	}
}
