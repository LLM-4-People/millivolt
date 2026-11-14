package scheduler

import (
	"maps"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
)

// Hold is one operator pause. Matching is AND across the dimensions that
// are set: All parks everyone; New parks clients not in KnownAtNew; every
// other dimension is an exact record-field set, and an empty set means "any"
// on that dimension. Until is the absolute unpause time; zero means until the
// operator resumes. MaxQueued is how many matching waiters this hold will park
// before Acquire refuses (0 = unlimited).
//
// A hold's dimensions are LimitScope's dimensions: the console offers the same
// scope strip for every rule kind, and the scheduler already resolves all five
// facts (WaiterHooks carries Provider, Model, Client, KeyHash and Conversation)
// before it asks whether a hold applies. Matching only client and provider left
// a hold unable to express "this model" or "this key" while a limit could, so
// one section of the console had four fewer choices than another for no reason
// the operator could see. Scope is embedded rather than flattened so the five
// dimensions have one definition and one normalization.
type Hold struct {
	ID         string
	All        bool
	New        bool
	Scope      LimitScope
	KnownAtNew []string
	Until      time.Time
	MaxQueued  int
}

func (h Hold) Active() bool {
	if holdExpired(h.Until) {
		return false
	}
	return h.All || h.New || !h.Scope.Empty()
}

type holdSnap struct {
	id        string
	all       bool
	new       bool
	scope     LimitScope
	knownNew  map[string]struct{}
	until     time.Time
	maxQueued int
}

type policySnap struct {
	holds []*holdSnap
}

var holdSeq atomic.Uint64

func newHoldID() string {
	return "p" + strconv.FormatUint(holdSeq.Add(1), 10)
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
	return slices.Sorted(maps.Keys(m))
}

func snapHold(h Hold) *holdSnap {
	id := h.ID
	if id == "" {
		id = newHoldID()
	}
	return &holdSnap{
		id:  id,
		all: h.All,
		new: h.New,
		// The same normalization a limit gets, so two holds whose selectors
		// differ only in spelling, order or duplicates address one target.
		scope:     NormalizeLimitScope(h.Scope),
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
		ID: h.id, All: h.all, New: h.new, Scope: h.scope,
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
	return h.all || h.new || !h.scope.Empty()
}

// matches is AND across the dimensions that are set. An empty dimension means
// "any value" on that dimension, and All overrides every dimension. New is a
// client-dimension modifier: those requests park only for clients the server
// had not seen when the hold started.
//
// Every dimension is read from the same five-fact identity a limit is matched
// against, so a hold and a limit can no longer disagree about which requests
// they cover. The one asymmetry that remains is deliberate and pre-existing:
// New needs the client, so it is evaluated here rather than in LimitScope.
func (h *holdSnap) matches(t PolicyTarget) bool {
	if !h.active() {
		return false
	}
	if h.all {
		return true
	}
	// The client dimension carries the New modifier, so it is decided here and
	// the remaining four dimensions are then an exact-set AND.
	named := h.scope.Clients
	if len(named) > 0 {
		if !slices.Contains(named, t.Client) {
			// A named hold parks only its named clients. New also admits
			// clients the server had not seen when the hold started, and a
			// named client always wins over New rather than being re-tested.
			if !h.new {
				return false
			}
			if _, known := h.knownNew[t.Client]; known {
				return false
			}
		}
	} else if h.new {
		if _, known := h.knownNew[t.Client]; known {
			return false
		}
	}
	// The client dimension is already decided; check the rest.
	rest := h.scope
	rest.Clients = nil
	return rest.Matches(t.Provider, t.Model, "", t.KeyHash, t.Conversation)
}

func (p *policySnap) match(t PolicyTarget) *holdSnap {
	if p == nil {
		return nil
	}
	for _, h := range p.holds {
		if h.matches(t) {
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
	if !slicesEqual(a.scope.Providers, b.scope.Providers) ||
		!slicesEqual(a.scope.Models, b.scope.Models) ||
		!slicesEqual(a.scope.Clients, b.scope.Clients) ||
		!slicesEqual(a.scope.KeyHashes, b.scope.KeyHashes) ||
		!slicesEqual(a.scope.Conversations, b.scope.Conversations) {
		return false
	}
	return slicesEqual(keysOf(a.knownNew), keysOf(b.knownNew))
}

// slicesEqual reports whether two already-normalized string slices hold the
// same values in the same order. Snapshots are built from
// NormalizeLimitScope, so order is canonical and a direct comparison is the
// whole answer; this exists so the five dimensions are compared by one rule
// instead of a hand-written loop per dimension.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// holdScope is the subset of requests a hold covers, one entry per dimension.
// Used to reject two pauses that would park the same request. A dimension that
// is not set is "any", which is why each dimension records whether it is
// unbounded instead of relying on an empty set to mean both "any" and "none".
type holdScope struct {
	all        bool
	newClients bool
	knownNew   map[string]struct{}
	// dims is keyed by LimitScope dimension (providers, models, clients,
	// keyHashes, conversations). A missing entry means the hold does not
	// narrow that dimension.
	dims map[string]map[string]struct{}
}

// holdScopeDims is the one list of scope dimensions. It is derived from the
// shared scope type rather than restated, so a new LimitScope dimension
// widens holds and limits together.
var holdScopeDims = []string{"providers", "models", "clients", "keyHashes", "conversations"}

func scopeOf(h Hold) holdScope {
	s := holdScope{knownNew: setFrom(h.KnownAtNew), dims: map[string]map[string]struct{}{}}
	if h.All {
		s.all = true
		return s
	}
	if h.New {
		s.newClients = true
	}
	values := map[string][]string{
		"providers":     h.Scope.Providers,
		"models":        h.Scope.Models,
		"clients":       h.Scope.Clients,
		"keyHashes":     h.Scope.KeyHashes,
		"conversations": h.Scope.Conversations,
	}
	for _, dim := range holdScopeDims {
		if set := setFrom(values[dim]); set != nil {
			s.dims[dim] = set
		}
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

// dimOverlap reports whether two holds can select the same value on one
// dimension. A dimension either hold leaves unbounded accepts every value,
// so it always intersects.
func dimOverlap(a, b holdScope, dim string) bool {
	as, bs := a.dims[dim], b.dims[dim]
	if as == nil || bs == nil {
		return true
	}
	return namedOverlap(as, bs)
}

// clientsOverlap is the client dimension plus the New modifier, which no
// other dimension has: a New hold's client set is "everyone not in
// KnownAtNew", so it can intersect a re-created hold's named clients.
func clientsOverlap(a, b holdScope) bool {
	an, bn := a.dims["clients"], b.dims["clients"]
	// A side that neither names clients nor restricts itself to new ones
	// accepts every client, so it intersects whatever the other side covers.
	if (an == nil && !a.newClients) || (bn == nil && !b.newClients) {
		return true
	}
	// Two new-only holds: some client is unknown to both.
	if a.newClients && b.newClients {
		return true
	}
	// New against a named set: they intersect when a named client was unknown
	// when the new-only hold started. An empty named set here cannot happen -
	// a side with no named clients and no New returned above.
	if a.newClients {
		for c := range bn {
			if _, known := a.knownNew[c]; !known {
				return true
			}
		}
		return false
	}
	if b.newClients {
		for c := range an {
			if _, known := b.knownNew[c]; !known {
				return true
			}
		}
		return false
	}
	return namedOverlap(an, bn)
}

// HoldsOverlap reports whether two active holds would park the same request -
// even with different timers. Two holds conflict only when every dimension
// they cover intersects: a provider-only hold and a model-only hold overlap on
// their shared request space, while a hold for one model and a hold for a
// different model do not.
func HoldsOverlap(a, b Hold) bool {
	if !a.Active() || !b.Active() {
		return false
	}
	sa, sb := scopeOf(a), scopeOf(b)
	if sa.all || sb.all {
		return true
	}
	for _, dim := range holdScopeDims {
		if dim == "clients" {
			if !clientsOverlap(sa, sb) {
				return false
			}
			continue
		}
		if !dimOverlap(sa, sb, dim) {
			return false
		}
	}
	return true
}

// RestoreHolds loads a persisted set without overlap rejection (already
// accepted) and without dropping hold-queue counts we do not have yet.
func (s *Scheduler) RestoreHolds(holds []Hold) {
	s.replaceHolds(holds)
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
	s.commitAppendedHolds(cur, h)
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
	nextHolds, found := holdsWithoutID(s.policy.Load(), h.ID)
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
	s.commitAppendedHolds(nextHolds, h)
	return nil
}

// RemoveHold drops one pause by id. Unknown id is a no-op.
func (s *Scheduler) RemoveHold(id string) {
	if id == "" {
		return
	}
	s.pauseMu.Lock()
	nextHolds, found := holdsWithoutID(s.policy.Load(), id)
	if !found {
		s.pauseMu.Unlock()
		return
	}
	next := snapFromHolds(nextHolds)
	s.commitHolds(next, false)
}

// holdsWithoutID projects the loaded policy's holds minus the one with id.
// found reports whether id was present; a nil snapshot projects an empty set.
func holdsWithoutID(cur *policySnap, id string) (next []Hold, found bool) {
	next = make([]Hold, 0)
	if cur != nil {
		for _, sh := range cur.holds {
			hh := sh.hold()
			if hh.ID == id {
				found = true
				continue
			}
			next = append(next, hh)
		}
	}
	return next, found
}

// commitAppendedHolds commits the accepted set with h appended: it snapshots,
// then commits with queued-waiter seeding (both hold-addition paths - a fresh
// add and a same-id replace - seed). The caller holds pauseMu, has already
// overlap-checked, and delegates the rest of the lock discipline to
// commitHolds.
func (s *Scheduler) commitAppendedHolds(nextHolds []Hold, h Hold) {
	nextHolds = append(nextHolds, h)
	next := snapFromHolds(nextHolds)
	s.commitHolds(next, true)
}

// replaceHolds installs a whole hold set as-is (RestoreHolds' mechanism):
// acceptance already happened at the operator boundary, so no overlap check
// runs here.
func (s *Scheduler) replaceHolds(holds []Hold) {
	next := snapFromHolds(holds)
	s.pauseMu.Lock()
	s.commitHolds(next, true)
}

// commitHolds finishes a policy transition: it publishes the prepared
// snapshot, releases pauseMu, and runs the post-commit epilogue (hold-count
// pruning, queued-waiter seeding, drain). The caller must hold pauseMu -
// its reads, overlap checks, and snapshot build run under the same lock so
// a concurrent transition cannot interleave - and must not touch pauseMu
// after this returns. seedQueued is false only for RemoveHold: removal
// drops a scope, it never widens one, so waiters parked by the surviving
// holds stay correctly attributed and a seed pass has nothing to adopt.
func (s *Scheduler) commitHolds(next *policySnap, seedQueued bool) {
	changed := s.commitPolicyLocked(next)
	s.pauseMu.Unlock()
	if !changed {
		return
	}
	s.pruneHoldCounts(next)
	if seedQueued {
		s.seedQueuedHolds()
	}
	s.drainAll()
}

func (s *Scheduler) commitPolicyLocked(next *policySnap) bool {
	if next == nil {
		next = &policySnap{}
	}
	if snapEqual(s.policy.Load(), next) {
		return false
	}
	s.policy.Store(next)
	s.kickPolicyLocked()
	return true
}

// kickPolicyLocked closes and replaces the global policy kick channel so
// every policy-edge waiter (the hold select loops) re-evaluates its
// predicate. The caller must hold pauseMu. This owns the policy kick only:
// the group send gate (sendKick under g.mu) and the storm state kick
// (stormState.kick under its own mu) are separate channels with their own
// wake paths. kick is built in New, so the nil guard is defensive only -
// kept from the two former inline call sites.
func (s *Scheduler) kickPolicyLocked() {
	if s.kick != nil {
		close(s.kick)
	}
	s.kick = make(chan struct{})
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
	for _, g := range s.snapshotGroups() {
		g.mu.Lock()
		g.queue.forEach(func(w *waiter) {
			h := s.matchingHold(w.PolicyTarget())
			s.holdCountMu.Lock()
			if h == nil {
				// ReplaceHold / RestoreHolds may drop this waiter from the
				// new scope. Uncount in place - do not call dropHold
				// (that would nest holdCountMu / fire OnUnhold from seed).
				s.clearHoldCountLocked(w)
				s.holdCountMu.Unlock()
				return
			}
			s.moveHoldCountLocked(w, h)
			if !w.holding {
				w.holding = true
				if w.onHold != nil {
					fire = append(fire, seeded{w, h})
				}
			}
			s.holdCountMu.Unlock()
		})
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

// Holds reports whether a request with only a known client is parked. A hold
// that narrows by provider, model, key or conversation cannot answer without
// those facts, so callers holding the full identity use HoldsTarget.
func (s *Scheduler) Holds(client string) bool {
	return s.matchingHold(PolicyTarget{Client: client}) != nil
}

// HoldsTarget reports whether a request with this identity is parked. It takes
// the same five-fact target a limit is matched against, because a hold can
// narrow by any of them; passing fewer facts is how a hold on one model used
// to be invisible to the check that decides whether a request is parked.
func (s *Scheduler) HoldsTarget(t PolicyTarget) bool {
	return s.matchingHold(t) != nil
}

func (s *Scheduler) matchingHold(t PolicyTarget) *holdSnap {
	return s.policy.Load().match(t)
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

// fireHoldLocked walks the false→true holding edge: sets the flag and
// invokes OnHold exactly once per edge. Callers hold holdCountMu, which makes
// the edge atomic and serializes hook dispatch with every other hold
// transition (the ordering half of adoptHold's contract).
func (w *waiter) fireHoldLocked() {
	if !w.holding {
		w.holding = true
		if w.onHold != nil {
			w.onHold()
		}
	}
}

// fireUnholdLocked walks the true→false holding edge: clears the flag and
// invokes OnUnhold exactly once per edge. Callers hold holdCountMu for the
// same serialized-dispatch contract, except dropHold's nil-scheduler
// fallback: no scheduler means no shared state and no concurrent transition
// to serialize against, so no lock exists to hold.
func (w *waiter) fireUnholdLocked() {
	if w.holding {
		w.holding = false
		if w.onUnhold != nil {
			w.onUnhold()
		}
	}
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
	w.fireHoldLocked()
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
	h := s.matchingHold(w.PolicyTarget())
	if h != nil {
		s.moveHoldCountLocked(w, h)
		w.fireHoldLocked()
		s.holdCountMu.Unlock()
		return true
	}
	s.clearHoldCountLocked(w)
	w.fireUnholdLocked()
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
		w.fireUnholdLocked()
		s.holdCountMu.Unlock()
		return
	}
	// Nil scheduler: no lock exists to take (see fireUnholdLocked).
	w.fireUnholdLocked()
}
