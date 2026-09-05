package scheduler

import (
	"sort"
	"sync/atomic"
	"time"
)

// Hold is one operator pause. Matching is AND across the dimensions that
// are set: All parks everyone; New parks clients not in KnownAtNew;
// Clients / Providers are exact Record.Client / Record.Provider sets
// (empty means "any" on that dimension). Until is the absolute unpause
// time; zero means until the operator resumes. MaxQueued is how many
// matching waiters this hold will park before Acquire refuses (0 = unlimited).
type Hold struct {
	ID         string
	All        bool
	New        bool
	Clients    []string
	Providers  []string
	KnownAtNew []string
	Until      time.Time
	MaxQueued  int
}

// Policy is the operator hold. SetPolicy replaces the whole set with this
// single hold (compat). Prefer AddHold / RemoveHold / SetHolds for multiple
// non-overlapping pauses.
type Policy struct {
	All        bool
	New        bool
	Clients    []string
	Providers  []string
	KnownAtNew []string
	Until      time.Time
	MaxQueued  int
}

func (p Policy) Active() bool {
	return p.asHold().Active()
}

func (p Policy) asHold() Hold {
	return Hold{
		All: p.All, New: p.New, Clients: p.Clients, Providers: p.Providers,
		KnownAtNew: p.KnownAtNew, Until: p.Until, MaxQueued: p.MaxQueued,
	}
}

func (h Hold) Active() bool {
	if holdExpired(h.Until) {
		return false
	}
	return h.All || h.New || len(h.Clients) > 0 || len(h.Providers) > 0
}

type holdSnap struct {
	id        string
	all       bool
	new       bool
	clients   map[string]struct{}
	providers map[string]struct{}
	knownNew  map[string]struct{}
	until     time.Time
	maxQueued int
}

type policySnap struct {
	holds []*holdSnap
}

var holdSeq atomic.Uint64

func newHoldID() string {
	return "p" + itoa(holdSeq.Add(1))
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func setFrom(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		if s != "" {
			m[s] = struct{}{}
		}
	}
	return m
}

func keysOf(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func snapHold(h Hold) *holdSnap {
	id := h.ID
	if id == "" {
		id = newHoldID()
	}
	return &holdSnap{
		id:        id,
		all:       h.All,
		new:       h.New,
		clients:   setFrom(h.Clients),
		providers: setFrom(h.Providers),
		knownNew:  setFrom(h.KnownAtNew),
		until:     h.Until,
		maxQueued: h.MaxQueued,
	}
}

func (h *holdSnap) hold() Hold {
	if h == nil {
		return Hold{}
	}
	return Hold{
		ID: h.id, All: h.all, New: h.new,
		Clients: keysOf(h.clients), Providers: keysOf(h.providers),
		KnownAtNew: keysOf(h.knownNew), Until: h.until, MaxQueued: h.maxQueued,
	}
}

func holdExpired(until time.Time) bool {
	return !until.IsZero() && !time.Now().Before(until)
}

func (h *holdSnap) active() bool {
	if h == nil || holdExpired(h.until) {
		return false
	}
	return h.all || h.new || len(h.clients) > 0 || len(h.providers) > 0
}

// matches is AND across set dimensions. Empty client-scope (and not New/All)
// means any client; empty provider-scope means any provider. An empty
// provider argument only matches holds that do not filter on provider.
func (h *holdSnap) matches(client, provider string) bool {
	if !h.active() {
		return false
	}
	if h.all {
		return true
	}
	if !h.clientOK(client) {
		return false
	}
	return h.providerOK(provider)
}

func (h *holdSnap) clientOK(client string) bool {
	if h.all {
		return true
	}
	hasNamed := len(h.clients) > 0
	if hasNamed {
		if _, ok := h.clients[client]; ok {
			return true
		}
	}
	if h.new {
		_, known := h.knownNew[client]
		return !known
	}
	if hasNamed {
		return false
	}
	// Provider-only hold: any client.
	return len(h.providers) > 0
}

func (h *holdSnap) providerOK(provider string) bool {
	if h.all || len(h.providers) == 0 {
		return true
	}
	if provider == "" {
		return false
	}
	_, ok := h.providers[provider]
	return ok
}

func (p *policySnap) match(client, provider string) *holdSnap {
	if p == nil {
		return nil
	}
	for _, h := range p.holds {
		if h.matches(client, provider) {
			return h
		}
	}
	return nil
}

func (p *policySnap) active() bool {
	if p == nil {
		return false
	}
	for _, h := range p.holds {
		if h.active() {
			return true
		}
	}
	return false
}

func (p *policySnap) list() []Hold {
	if p == nil || len(p.holds) == 0 {
		return nil
	}
	out := make([]Hold, 0, len(p.holds))
	for _, h := range p.holds {
		if h.active() {
			out = append(out, h.hold())
		}
	}
	return out
}

func snapFromHolds(holds []Hold) *policySnap {
	p := &policySnap{}
	for _, h := range holds {
		if !h.Active() {
			continue
		}
		p.holds = append(p.holds, snapHold(h))
	}
	return p
}

func snapEqual(a, b *policySnap) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.holds) != len(b.holds) {
		return false
	}
	for i := range a.holds {
		if !holdEqual(a.holds[i], b.holds[i]) {
			return false
		}
	}
	return true
}

func holdEqual(a, b *holdSnap) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.id != b.id || a.all != b.all || a.new != b.new || a.maxQueued != b.maxQueued || !a.until.Equal(b.until) {
		return false
	}
	if len(a.clients) != len(b.clients) || len(a.providers) != len(b.providers) || len(a.knownNew) != len(b.knownNew) {
		return false
	}
	for k := range a.clients {
		if _, ok := b.clients[k]; !ok {
			return false
		}
	}
	for k := range a.providers {
		if _, ok := b.providers[k]; !ok {
			return false
		}
	}
	for k := range a.knownNew {
		if _, ok := b.knownNew[k]; !ok {
			return false
		}
	}
	return true
}

// holdScope is the cartesian client×provider set a hold covers. Used to
// reject two pauses that would park the same request.
type holdScope struct {
	allClients   bool
	newClients   bool
	clients      map[string]struct{}
	knownNew     map[string]struct{}
	allProviders bool
	providers    map[string]struct{}
}

func scopeOf(h Hold) holdScope {
	s := holdScope{knownNew: setFrom(h.KnownAtNew)}
	if h.All {
		s.allClients = true
		s.allProviders = true
		return s
	}
	if h.New {
		s.newClients = true
	}
	s.clients = setFrom(h.Clients)
	if len(h.Providers) == 0 {
		s.allProviders = true
	} else {
		s.providers = setFrom(h.Providers)
	}
	if !h.New && len(h.Clients) == 0 {
		s.allClients = true
	}
	return s
}

func namedOverlap(a, b map[string]struct{}) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

func clientsOverlap(a, b holdScope) bool {
	if a.allClients || b.allClients {
		return true
	}
	if namedOverlap(a.clients, b.clients) {
		return true
	}
	if a.newClients && b.newClients {
		return true
	}
	if a.newClients {
		for c := range b.clients {
			if _, known := a.knownNew[c]; !known {
				return true
			}
		}
	}
	if b.newClients {
		for c := range a.clients {
			if _, known := b.knownNew[c]; !known {
				return true
			}
		}
	}
	return false
}

func providersOverlap(a, b holdScope) bool {
	if a.allProviders || b.allProviders {
		return true
	}
	return namedOverlap(a.providers, b.providers)
}

// HoldsOverlap reports whether two active holds would park the same
// (client, provider) pair - even with different timers.
func HoldsOverlap(a, b Hold) bool {
	if !a.Active() || !b.Active() {
		return false
	}
	sa, sb := scopeOf(a), scopeOf(b)
	return clientsOverlap(sa, sb) && providersOverlap(sa, sb)
}

func overlapAny(holds []Hold) bool {
	for i := 0; i < len(holds); i++ {
		if !holds[i].Active() {
			continue
		}
		for j := i + 1; j < len(holds); j++ {
			if HoldsOverlap(holds[i], holds[j]) {
				return true
			}
		}
	}
	return false
}

// SetPaused parks or releases every group. SetPaused(true) is a global hold
// (Policy.All). SetPaused(false) clears the whole set. Prefer SetHolds /
// AddHold for scoped pauses.
func (s *Scheduler) SetPaused(paused bool) {
	if paused {
		s.SetPolicy(Policy{All: true})
		return
	}
	s.SetPolicy(Policy{})
}

// SetPolicy replaces the operator hold with a single policy. In-flight slots
// finish; drain skips held waiters so a paused client does not
// head-of-line-block others on the same key. Time spent held does not count
// toward MaxWait.
func (s *Scheduler) SetPolicy(p Policy) {
	if !p.Active() {
		_ = s.replaceHolds(nil, false)
		return
	}
	_ = s.replaceHolds([]Hold{p.asHold()}, false)
}

// SetHolds replaces the whole pause set. Returns ErrOverlap when two holds
// cover the same targets.
func (s *Scheduler) SetHolds(holds []Hold) error {
	return s.replaceHolds(holds, true)
}

// RestoreHolds loads a persisted set without overlap rejection (already
// accepted) and without dropping hold-queue counts we do not have yet.
func (s *Scheduler) RestoreHolds(holds []Hold) {
	_ = s.replaceHolds(holds, false)
}

// AddHold appends one pause. Returns ErrOverlap when it covers a target
// already parked by another hold. The snapshot, overlap check, and store
// run under pauseMu so concurrent adds cannot drop a hold.
func (s *Scheduler) AddHold(h Hold) error {
	if !h.Active() {
		return nil
	}
	if h.ID == "" {
		h.ID = newHoldID()
	}
	s.pauseMu.Lock()
	cur := s.policy.Load().list()
	for i := range cur {
		if HoldsOverlap(cur[i], h) {
			s.pauseMu.Unlock()
			return ErrOverlap
		}
	}
	nextHolds := make([]Hold, 0, len(cur)+1)
	nextHolds = append(nextHolds, cur...)
	nextHolds = append(nextHolds, h)
	next := snapFromHolds(nextHolds)
	changed := s.commitPolicyLocked(next)
	s.pauseMu.Unlock()
	if !changed {
		return nil
	}
	s.pruneHoldCounts(next)
	s.seedQueuedHolds()
	s.drainAll()
	return nil
}

// ReplaceHold updates one pause in place (same id) so already-queued
// waiters stay parked. Overlap is checked against the other holds only.
func (s *Scheduler) ReplaceHold(h Hold) error {
	if h.ID == "" {
		return ErrHoldNotFound
	}
	if !h.Active() {
		s.RemoveHold(h.ID)
		return nil
	}
	s.pauseMu.Lock()
	cur := s.policy.Load()
	nextHolds := make([]Hold, 0)
	found := false
	if cur != nil {
		for _, sh := range cur.holds {
			hh := sh.hold()
			if hh.ID == h.ID {
				found = true
				continue
			}
			nextHolds = append(nextHolds, hh)
		}
	}
	if !found {
		s.pauseMu.Unlock()
		return ErrHoldNotFound
	}
	for i := range nextHolds {
		if HoldsOverlap(nextHolds[i], h) {
			s.pauseMu.Unlock()
			return ErrOverlap
		}
	}
	nextHolds = append(nextHolds, h)
	next := snapFromHolds(nextHolds)
	changed := s.commitPolicyLocked(next)
	s.pauseMu.Unlock()
	if !changed {
		return nil
	}
	s.pruneHoldCounts(next)
	s.seedQueuedHolds()
	s.drainAll()
	return nil
}

// RemoveHold drops one pause by id. Unknown id is a no-op.
func (s *Scheduler) RemoveHold(id string) {
	if id == "" {
		return
	}
	s.pauseMu.Lock()
	cur := s.policy.Load()
	nextHolds := make([]Hold, 0)
	found := false
	if cur != nil {
		for _, h := range cur.holds {
			hh := h.hold()
			if hh.ID == id {
				found = true
				continue
			}
			nextHolds = append(nextHolds, hh)
		}
	}
	if !found {
		s.pauseMu.Unlock()
		return
	}
	next := snapFromHolds(nextHolds)
	changed := s.commitPolicyLocked(next)
	s.pauseMu.Unlock()
	if !changed {
		return
	}
	s.pruneHoldCounts(next)
	s.drainAll()
}

func (s *Scheduler) replaceHolds(holds []Hold, checkOverlap bool) error {
	if checkOverlap && overlapAny(holds) {
		return ErrOverlap
	}
	next := snapFromHolds(holds)
	s.pauseMu.Lock()
	changed := s.commitPolicyLocked(next)
	s.pauseMu.Unlock()
	if !changed {
		return nil
	}
	s.pruneHoldCounts(next)
	s.seedQueuedHolds()
	s.drainAll()
	return nil
}

func (s *Scheduler) commitPolicyLocked(next *policySnap) bool {
	if next == nil {
		next = &policySnap{}
	}
	if snapEqual(s.policy.Load(), next) {
		return false
	}
	s.policy.Store(next)
	close(s.kick)
	s.kick = make(chan struct{})
	return true
}

func (s *Scheduler) pruneHoldCounts(next *policySnap) {
	keep := make(map[string]struct{})
	if next != nil {
		for _, h := range next.holds {
			keep[h.id] = struct{}{}
		}
	}
	s.holdCountMu.Lock()
	if s.holdCount == nil {
		s.holdCount = make(map[string]int)
	}
	for id := range s.holdCount {
		if _, ok := keep[id]; !ok {
			delete(s.holdCount, id)
		}
	}
	s.holdCountMu.Unlock()
}

// seedQueuedHolds counts already-queued waiters against the holds they now
// match so a new Acquire cannot sneak under MaxQueued before they are
// attributed. The count stamp AND the holding transition both run inside the
// per-group scan, under g.mu + holdCountMu: every departure path needs g.mu
// (removeWaiter's queue removal, drain's grant pop), so a waiter visible in
// g.queue is provably not yet departed - membership proof and attribution are
// atomic. The previous shape staged (waiter, hold) pairs and ran adoptHold
// after the locks dropped; a waiter that departed in that window (client
// cancel, or a grant once the hold expired) was then adopted post-mortem:
// holdCount was re-added with nobody left to decrement it - a permanent leak
// that burns the hold's MaxQueued cap (reserveHold refuses new matching
// Acquires) and overcounts GET /admin/pause "queued" - and OnHold fired for a
// request whose Acquire had already returned, stranding a paused live row.
// Only the OnHold callback is deferred until after the scan drops g.mu (the
// hook never runs under g.mu), and fireSeededHold re-validates liveness for
// that scan→dispatch window, then invokes the hook WHILE STILL HOLDING
// holdCountMu - hold-callback dispatch is serialized with hold-attribution
// transitions under holdCountMu (the leaf), so a departing waiter's dropHold
// cannot interleave between validation and invocation and fire OnUnhold first
// (a stale paused publish). Hooks MUST be non-blocking and never re-enter the
// scheduler - the proxy's lifecycle publication is non-blocking. Socket I/O
// belongs to OnWait, which runs outside these accounting locks.
// The wait loop's own adoptHold and the seeded callback converge on the same
// false→true holding edge under the same mutex, so OnHold still fires exactly
// once per waiter - including for a ReplaceHold that newly matches a cap
// waiter still stuck inside OnWait. The unmatched branch deliberately
// suppresses OnUnhold: the waiter is being re-scoped, not released.
func (s *Scheduler) seedQueuedHolds() {
	type seeded struct {
		w *waiter
		h *holdSnap
	}
	var fire []seeded
	s.mu.Lock()
	groups := make([]*group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	s.mu.Unlock()
	for _, g := range groups {
		g.mu.Lock()
		for _, w := range g.queue {
			if w == nil {
				continue
			}
			h := s.matchingHold(w.client, w.provider)
			s.holdCountMu.Lock()
			if h == nil {
				// ReplaceHold / SetHolds may drop this waiter from the
				// new scope. Uncount in place - do not call dropHold
				// (that would nest holdCountMu / fire OnUnhold from seed).
				s.clearHoldCountLocked(w)
				s.holdCountMu.Unlock()
				continue
			}
			s.moveHoldCountLocked(w, h)
			if !w.holding {
				w.holding = true
				if w.onHold != nil {
					fire = append(fire, seeded{w, h})
				}
			}
			s.holdCountMu.Unlock()
		}
		g.mu.Unlock()
	}
	for _, p := range fire {
		s.fireSeededHold(p.w, p.h)
	}
}

// fireSeededHold invokes a seed-adopted waiter's OnHold. The scan→dispatch
// window is still real (the scan released g.mu; the waiter may have departed
// - client cancel, or a grant - since), so liveness is re-validated: holding
// only returns to false after dropHold uncounted it, and holdID only changes
// when another hold re-attributed the waiter - either way the pause must not
// be published for a request whose Acquire already returned. The invoke runs
// WHILE STILL HOLDING holdCountMu: a concurrent departure's dropHold blocks
// on the same lock until the hook returns, so its OnUnhold can never precede
// this OnHold - the ordering half of the "dispatch is serialized with
// attribution under holdCountMu" invariant (the exactly-once half is the
// false→true holding edge itself). Hooks must be non-blocking and never
// re-enter the scheduler.
func (s *Scheduler) fireSeededHold(w *waiter, h *holdSnap) {
	s.holdCountMu.Lock()
	if w.holding && w.holdID == h.id && w.onHold != nil {
		w.onHold()
	}
	s.holdCountMu.Unlock()
}

// HoldsList returns the active pauses (expired Until omitted).
func (s *Scheduler) HoldsList() []Hold {
	return s.policy.Load().list()
}

// Holds reports whether Acquire for this client is parked when the provider
// is unknown (client-only / all / new holds). Provider-scoped holds need
// HoldsTarget.
func (s *Scheduler) Holds(client string) bool {
	return s.matchingHold(client, "") != nil
}

// HoldsTarget reports whether Acquire for this client+provider is parked.
func (s *Scheduler) HoldsTarget(client, provider string) bool {
	return s.matchingHold(client, provider) != nil
}

func (s *Scheduler) matchingHold(client, provider string) *holdSnap {
	return s.policy.Load().match(client, provider)
}

// Paused reports whether any operator hold is active.
func (s *Scheduler) Paused() bool { return s.policy.Load().active() }

// HoldQueued is the number of waiters currently parked by this hold.
func (s *Scheduler) HoldQueued(id string) int {
	if id == "" {
		return 0
	}
	s.holdCountMu.Lock()
	n := s.holdCount[id]
	s.holdCountMu.Unlock()
	return n
}

// moveHoldCountLocked re-attributes w from its current hold to h: uncounts
// the old hold, counts the new one, stamps w.holdID. Idempotent - a no-op
// when w is already attributed to h - so seed × reserve × adopt × the wait
// loop, however they interleave, attribute one waiter exactly once. The
// caller must hold holdCountMu.
func (s *Scheduler) moveHoldCountLocked(w *waiter, h *holdSnap) {
	if w.holdID == h.id {
		return
	}
	if w.holdID != "" && s.holdCount[w.holdID] > 0 {
		s.holdCount[w.holdID]--
	}
	if s.holdCount == nil {
		s.holdCount = make(map[string]int)
	}
	s.holdCount[h.id]++
	w.holdID = h.id
}

// clearHoldCountLocked drops w's attribution: uncounts its hold and clears
// w.holdID. The caller must hold holdCountMu.
func (s *Scheduler) clearHoldCountLocked(w *waiter) {
	if w.holdID == "" {
		return
	}
	if s.holdCount[w.holdID] > 0 {
		s.holdCount[w.holdID]--
	}
	w.holdID = ""
}

func (s *Scheduler) reserveHold(w *waiter, h *holdSnap) bool {
	if w == nil || h == nil {
		return true
	}
	s.holdCountMu.Lock()
	defer s.holdCountMu.Unlock()
	if s.holdCount == nil {
		s.holdCount = make(map[string]int)
	}
	if w.holdID == h.id {
		return true
	}
	if h.maxQueued > 0 && s.holdCount[h.id] >= h.maxQueued {
		return false
	}
	s.moveHoldCountLocked(w, h)
	return true
}

func (w *waiter) adoptHold(s *Scheduler, h *holdSnap) {
	if s == nil {
		return
	}
	if h == nil {
		w.dropHold(s)
		return
	}
	// holdID + count + holding + the OnHold invoke move under holdCountMu as
	// ONE atomic transition: AddHold's seed pass, the enqueue re-check, and
	// the wait loop all adopt the same waiter concurrently, and exactly one
	// caller wins the false→true edge and fires OnHold. The invoke stays
	// inside the lock so dispatch is serialized with every other hold
	// transition (a concurrent departure's dropHold blocks until the hook
	// returns, making OnUnhold strictly ordered after the OnHold it races).
	// holdCountMu is a leaf whose sections touch only the count map and
	// waiter fields, so an in-lock invoke is cycle-free PROVIDED hooks are
	// non-blocking and never re-enter the scheduler (the proxy's
	// lifecycle publication satisfies this; socket writes belong to OnWait).
	s.holdCountMu.Lock()
	s.moveHoldCountLocked(w, h)
	if !w.holding {
		w.holding = true
		if w.onHold != nil {
			w.onHold()
		}
	}
	s.holdCountMu.Unlock()
}

// syncUnhold drops this waiter's hold count only if they no longer
// match. matchingHold is re-read under holdCountMu so seed cannot
// assign an id that we then decrement. The OnHold/OnUnhold invoke stays inside
// the lock (serialized dispatch - see adoptHold). Returns true if still held.
func (w *waiter) syncUnhold(s *Scheduler) bool {
	if s == nil {
		return false
	}
	s.holdCountMu.Lock()
	h := s.matchingHold(w.client, w.provider)
	if h != nil {
		s.moveHoldCountLocked(w, h)
		if !w.holding {
			w.holding = true
			if w.onHold != nil {
				w.onHold()
			}
		}
		s.holdCountMu.Unlock()
		return true
	}
	s.clearHoldCountLocked(w)
	if w.holding {
		w.holding = false
		if w.onUnhold != nil {
			w.onUnhold()
		}
	}
	s.holdCountMu.Unlock()
	return false
}

func (w *waiter) dropHold(s *Scheduler) {
	if s != nil {
		// holdID + holding + the OnUnhold invoke move under holdCountMu
		// (seed / reserve / adopt): the true→false edge is atomic, so a
		// concurrent adoptHold cannot interleave and OnUnhold fires exactly
		// once. The invoke stays inside the lock - a concurrent OnHold
		// dispatch (seed's fireSeededHold, or an adopt) blocks until the
		// hook returns, so OnHold can never land after this OnUnhold for the
		// waiter being dropped (a stale paused publish). Hooks must be
		// non-blocking and never re-enter the scheduler.
		s.holdCountMu.Lock()
		s.clearHoldCountLocked(w)
		if w.holding {
			w.holding = false
			if w.onUnhold != nil {
				w.onUnhold()
			}
		}
		s.holdCountMu.Unlock()
		return
	}
	if w.holding {
		w.holding = false
		if w.onUnhold != nil {
			w.onUnhold()
		}
	}
}
