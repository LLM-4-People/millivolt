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

// providerGate is the live limiter for one provider. The cap policy
// (Throttle) and occupancy (inFlight, buckets) live together so SetThrottle
// and tryAdmit share one mutex. The map of gates is the single owner of
// provider limits.
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

// blocked reports whether this gate refuses a request estimated at est
// tokens right now, and how long until the blocking dimension could admit
// (0 when the concurrency cap blocks: that waits for a release, not a
// timer). The caller must hold g.mu: the bucket refills it performs are
// the same mutations an admitting caller then relies on. tryAdmit applies
// its reservation only after this returns false.
func (g *providerGate) blocked(est int64) (bool, time.Duration) {
	if !g.throttle.Limit.Active() {
		return false, 0
	}
	now := time.Now()
	g.req.refill(now)
	g.tok.refill(now)
	lim := g.throttle.Limit
	if lim.Concurrency > 0 && g.inFlight >= lim.Concurrency {
		return true, 0 // wait for release, not a timer
	}
	if g.req.enabled && g.req.tokens < 1 {
		return true, g.req.waitFor(1)
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

func (g *providerGate) tryAdmit(est int64) (ok bool, lease admission, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// An inactive limit admits with NO gate bookkeeping at all (nil lease,
	// no occupancy) - distinct from active-and-admitted below, so this fork
	// cannot fold into blocked.
	if !g.throttle.Limit.Active() {
		return true, admission{}, 0
	}
	if blocked, wait := g.blocked(est); blocked {
		return false, admission{}, wait
	}
	// Admitted with a positive estimate: reserve it. The token switch above
	// already let an oversize request out of debt - it exhausts the bucket
	// here. A zero estimate reserves nothing (settle debits actual usage).
	var reserved int64
	if g.tok.enabled && est > 0 {
		reserved = est
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
	// A ramp shape change invalidates every group's epoch: the target and the
	// slope are different, so an old epoch would grant the wrong allowance.
	// Token/request-only edits keep the ramp untouched.
	rampChanged := cur.Limit.Concurrency != t.Limit.Concurrency ||
		cur.Limit.AntiBurst != t.Limit.AntiBurst ||
		cur.Limit.RampDuration != t.Limit.RampDuration
	if g != nil {
		g.wake.stop()
	}
	if !t.Limit.Active() {
		g.throttle = Throttle{}
		g.mu.Unlock()
		delete(s.gates, provider)
		s.publishGatesLocked()
		s.throttleMu.Unlock()
		if rampChanged {
			s.resetProviderRamps(provider)
		}
		s.kickThrottles()
		return true
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now()
	}
	if g == nil {
		if len(s.gates) >= maxProviderGates {
			// Header-driven limits are client-controlled. Beyond the cap a new
			// provider name is ignored instead of growing the gate map forever.
			s.throttleMu.Unlock()
			return false
		}
		g = s.newProviderGate(provider)
		g.mu.Lock()
		if s.gates == nil {
			s.gates = make(map[string]*providerGate)
		}
		s.gates[provider] = g
		s.publishGatesLocked()
	}
	g.throttle = t
	now := time.Now()
	g.resetBuckets(t.Limit, now)
	g.mu.Unlock()
	s.throttleMu.Unlock()
	if rampChanged {
		s.resetProviderRamps(provider)
	}
	s.kickThrottles()
	return true
}

// ThrottlePolicies returns every active provider cap, sorted by provider,
// without live occupancy. Durable persistence stores only policy, so it must
// not pay ListThrottles' scan of every group and queued waiter. Every gate
// policy write holds throttleMu, so reading g.throttle under that lock is
// race-free and never takes a group mutex. Throttle has only value fields, so
// the returned copies cannot alias later gate updates.
func (s *Scheduler) ThrottlePolicies() []Throttle {
	s.throttleMu.Lock()
	out := make([]Throttle, 0, len(s.gates))
	for _, g := range s.gates {
		out = append(out, g.throttle)
	}
	s.throttleMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// ListThrottles returns every active provider cap, sorted by provider,
// with live occupancy. The dashboard GET is this list. Persistence uses the
// policy-only ThrottlePolicies instead.
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

// throttleQueued counts, per provider, every request the scheduler parks for
// that provider: admission waiters plus send-gate waiters. A 429 pacing
// window parks both populations, so "queued" in the limits menu means work
// waiting on the provider, not only work waiting on a configured cap.
func (s *Scheduler) throttleQueued() map[string]int {
	out := map[string]int{}
	for _, g := range s.snapshotGroups() {
		g.mu.Lock()
		for _, w := range g.queue {
			if w.provider != "" {
				out[w.provider]++
			}
		}
		if g.provider != "" {
			out[g.provider] += g.sendWaiters
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
	s.publishGatesLocked()
	s.throttleMu.Unlock()
	s.resetAllRamps()
}

// resetProviderRamps clears the ramp epoch of every group attributed to
// provider. It runs after the policy lock is released and before the caller
// kicks the throttles, so no group mutex is taken under throttleMu or a gate
// mutex. The index pointer stays valid after the scheduler lock is released;
// eviction unlinks groups but never frees the index while it is in use.
func (s *Scheduler) resetProviderRamps(provider string) {
	s.mu.Lock()
	idx := s.providerGroups[provider]
	s.mu.Unlock()
	if idx == nil {
		return
	}
	resetRamps(idx.snapshotAll())
}

// resetAllRamps clears every attributed group's ramp epoch. RestoreThrottles
// replaces the entire policy set, so no installed epoch survives it.
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

// maxProviderGates bounds header-driven provider caps. Configured providers
// are restored explicitly and are not subject to the header-creation path.
const maxProviderGates = 4096

func (s *Scheduler) newProviderGate(provider string) *providerGate {
	g := &providerGate{}
	g.wake.fn = func() { s.drainProviderWake(provider) }
	return g
}

func (s *Scheduler) gate(provider string) *providerGate {
	if provider == "" {
		return nil
	}
	snap := s.gateSnap.Load()
	if snap == nil {
		return nil
	}
	return (*snap)[provider]
}

// publishGatesLocked replaces the immutable read view. Callers hold
// throttleMu; readers only load the pointer, so a hot-path admission never
// waits for a policy writer.
func (s *Scheduler) publishGatesLocked() {
	snap := make(map[string]*providerGate, len(s.gates))
	for p, g := range s.gates {
		snap[p] = g
	}
	s.gateSnap.Store(&snap)
}

func (s *Scheduler) tryAdmit(provider string, est int64) (ok bool, lease admission, wait time.Duration) {
	g := s.gate(provider)
	if g == nil {
		return true, admission{}, 0
	}
	return g.tryAdmit(est)
}

// providerConcurrencyFull reports whether provider's gate has no free
// concurrency slot left. It reads occupancy under the gate lock only: a drain
// that cannot admit anyone must not walk its fair set, and a bucket refill (the
// one gate mutation an occupancy read could trigger) belongs to admission, not
// to a capacity query. A nil gate, or a limit without a concurrency dimension,
// has no cap to saturate.
func (s *Scheduler) providerConcurrencyFull(provider string) bool {
	g := s.gate(provider)
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	limit := g.throttle.Limit
	return limit.Concurrency > 0 && g.inFlight >= limit.Concurrency
}

// settle reconciles the gate captured at admission, never a fresh
// provider-name lookup, and does not drain: the caller owns exactly one drain
// after it has also released the group's slot. Settling first keeps reserved
// tokens, the token bucket and gate in-flight occupancy authoritative before a
// queued waiter can consume the freed capacity. A cleared/reinstalled limit has
// a different owner; traffic admitted without a gate cannot charge a
// subsequently installed cap. Token enable/disable starts a new epoch even when
// concurrency keeps the gate; old reservations cannot debit/refund that new
// bucket. Enabled resizes retain their epoch so outstanding usage still
// reconciles against the same debt. A nil gate is a no-op and the caller still
// drains the group once.
func (a admission) settle(actual int64) {
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
		if g.queued > 0 {
			g.markGateWaitingLocked()
		}
		g.mu.Unlock()
	}
}

// providerRamps reports whether provider currently has an active anti-burst
// ramp, the only timer wake that can unblock a group deliberately left out of
// the fair set. The gate comes from the atomic policy snapshot, so this never
// takes throttleMu.
func (s *Scheduler) providerRamps(provider string) bool {
	gate := s.gate(provider)
	if gate == nil {
		return false
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.throttle.Limit.rampActive()
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
