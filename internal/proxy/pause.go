package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// pausePersist is the durable operator-pause snapshot (SQLite meta).
// Missing store = memory-only (documented).
type pausePersist interface {
	SavePause(ctx context.Context, raw []byte) error
	LoadPause(ctx context.Context) ([]byte, error)
	SaveThrottle(ctx context.Context, raw []byte) error
	LoadThrottle(ctx context.Context) ([]byte, error)
	ListClients(ctx context.Context) ([]string, error)
	ListProviders(ctx context.Context) ([]string, error)
	ListModels(ctx context.Context) ([]string, error)
	SaveDebugSessions(ctx context.Context, raw []byte) error
	LoadDebugSessions(ctx context.Context) ([]byte, error)
	SaveDebugCapture(ctx context.Context, id, sessionID string, capturedAt, expiresAt int64, payload []byte) error
	LoadDebugCapture(ctx context.Context, id string) ([]byte, error)
	ExpireDebugCaptures(ctx context.Context, beforeMs int64) (int64, error)
	CountDebugSession(ctx context.Context, sessionID string) (int64, error)
}

// persistedPause is one reboot-surviving hold. Until is absolute so a
// restart mid-window keeps the remaining time. Reason is the system label
// of an auto-created hold (the durable quota/billing token); operator edits
// preserve it and it is never accepted from the HTTP surface.
type persistedPause struct {
	ID         string    `json:"id,omitempty"`
	All        bool      `json:"all,omitempty"`
	New        bool      `json:"new,omitempty"`
	Clients    []string  `json:"clients,omitempty"`
	Providers  []string  `json:"providers,omitempty"`
	KnownAtNew []string  `json:"known_at_new,omitempty"`
	Duration   string    `json:"duration,omitempty"`
	Until      time.Time `json:"until,omitempty"`
	MaxQueued  int       `json:"max_queued,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type persistedDoc struct {
	Holds []persistedPause `json:"holds,omitempty"`
}

// A present null list is not an omitted scope: treating it as nil would
// promote {paused:true,clients:null} to a global hold.
type pauseNames []string

func (p *pauseNames) UnmarshalJSON(raw []byte) error {
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return err
	}
	if names == nil {
		return fmt.Errorf("pause scope must be an array")
	}
	*p = names
	return nil
}

type pauseBool struct {
	set   bool
	value bool
}

func (p *pauseBool) UnmarshalJSON(raw []byte) error {
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value == nil {
		return fmt.Errorf("pause scope flag must be a boolean")
	}
	p.set, p.value = true, *value
	return nil
}

// pauseDurations is the allowlist (deny by default). Empty = until resume.
// Keys are FormatDuration of the values (except "" for 0).
var pauseDurations = map[string]time.Duration{
	"":                                      0,
	config.FormatDuration(15 * time.Minute): 15 * time.Minute,
	config.FormatDuration(time.Hour):        time.Hour,
	config.FormatDuration(6 * time.Hour):    6 * time.Hour,
	config.FormatDuration(12 * time.Hour):   12 * time.Hour,
	config.FormatDuration(24 * time.Hour):   24 * time.Hour,
}

func pauseDurationError() error {
	keys := make([]string, 0, len(pauseDurations))
	for k := range pauseDurations {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return pauseDurations[keys[i]] < pauseDurations[keys[j]]
	})
	return fmt.Errorf("duration must be %s, or empty", strings.Join(keys, ", "))
}

// resolveHoldDuration resolves an operator-hold duration: the request's
// optional body override (whitespace-trimmed), else the previous hold's
// duration, then the pauseDurations allowlist lookup. ok reports whether
// dur is an allowlist member; the caller maps the rejection to its own
// pauseDurationError return shape.
func resolveHoldDuration(body *string, hasPrev bool, prevDur string) (string, time.Duration, bool) {
	dur := ""
	if body != nil {
		dur = strings.TrimSpace(*body)
	} else if hasPrev {
		dur = prevDur
	}
	wait, ok := pauseDurations[dur]
	return dur, wait, ok
}

// mergeUntil owns the deadline merge shared by the pause and debug edit
// transactions: an edit that omits duration keeps the previous deadline
// untouched; a stated duration re-anchors a fresh window now, except the same
// duration restated while the previous window is still live keeps it (an
// extend-in-place edit must not reset the clock). Zero wait with no carried
// deadline means no deadline (until the operator resumes).
func mergeUntil(hasPrev, durationOmitted bool, dur string, wait time.Duration, prevDur string, prevUntil time.Time) time.Time {
	if hasPrev && durationOmitted {
		return prevUntil
	}
	if wait > 0 {
		if hasPrev && prevDur == dur && !prevUntil.IsZero() && time.Now().Before(prevUntil) {
			return prevUntil
		}
		return time.Now().Add(wait)
	}
	return time.Time{}
}

type pauseRuntime struct {
	persist    pausePersist
	clients    nameSet
	providers  nameSet
	models     nameSet
	mu         sync.Mutex
	holds      []persistedPause
	timers     map[string]*time.Timer
	persistGen uint64
}

// nameSet is one operator-known dimension (clients, providers, canonical
// models): the names an operator can scope a future hold or debug session
// to. Empty names are never recorded; the model set stores canonical names,
// so callers canonicalize before add/seed.
//
// Two classes are tracked, and the distinction is load-bearing rather than
// bookkeeping. Observed names come from live traffic and are evictable; client
// labels and model ids are chosen by the caller, so an unauthenticated peer can
// submit thousands of distinct ones. Pinned names are the scopes of the operator
// documents that currently exist (a hold or a debug session) and are never
// evicted by traffic: operator intent outranks whatever arrives later.
//
// Pins are derived, not accumulated. syncNamePins recomputes them from the live
// document set on every change, so removing a hold releases its names. An
// append-only pin set would let retired documents pin forever, and a saturated
// set would then freeze the vocabulary - which is the same "operator's own
// names are unlistable" failure this design exists to prevent.
type nameSet struct {
	mu     sync.Mutex
	names  map[string]struct{}
	pinned map[string]struct{}
	// seq is each stored name's current generation, and order lists observed
	// names oldest-first by last observation. An entry whose generation no
	// longer matches is stale (re-observed, pinned, or evicted and re-added) and
	// is skipped as the head advances, so the slice stays proportional to the
	// stored set.
	// pins holds each document family's scoped names, so the pinned set is a
	// derived union rather than an append-only record of every scope ever seen.
	pins     map[string]map[string]struct{}
	seq      map[string]uint64
	order    []nameEntry
	next     uint64
	revision uint64
	sorted   []string
	// sortedRevision is the revision the cached sorted view was built from.
	// Zero matches the empty set, whose list is nil (preserving the former
	// slices.Sorted result for nil and empty maps).
	sortedRevision uint64
}

type nameEntry struct {
	seq  uint64
	name string
}

// nameSetMax bounds one operator-known vocabulary. These lists exist for
// operator convenience; model names are client-controlled, so an unbounded set
// is a process-lifetime memory leak. Observed names past the cap displace the
// least recently observed name rather than being dropped, so the vocabulary
// tracks what is actually in use instead of freezing on whatever arrived first.
const nameSetMax = 4096

// add records one observed (evictable) name. A repeat observation refreshes its
// position, so a continuously active client outlives a flood of one-shot labels
// that arrived after it. revision advances only when the stored set changes.
func (n *nameSet) add(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.observeLocked(name)
}

// seed bulk-records durable history. It records observed names with the same
// evicting policy as add: storage returns its lists in name order, so a
// first-wins seed would make boot-time vocabulary an incidental property of a
// SQL ORDER BY instead of what is in use.
func (n *nameSet) seed(names []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, name := range names {
		n.observeLocked(name)
	}
}

// ensureMapsLocked lazily creates the three maps so a zero nameSet is usable.
func (n *nameSet) ensureMapsLocked() {
	if n.names == nil {
		n.names = make(map[string]struct{})
	}
	if n.pinned == nil {
		n.pinned = make(map[string]struct{})
	}
	if n.seq == nil {
		n.seq = make(map[string]uint64)
	}
}

// setPins replaces one document family's contribution to the pinned
// vocabulary: "pause" for every hold's scope, "debug" for every session's.
// Each family owns one entry, so removing a hold releases its names on the next
// sync without a second release path, and a retired document can never pin
// forever. The names come from the live documents, never accumulated.
//
// Names that cannot be stored because the whole vocabulary is already pinned are
// returned so the caller can log them; silent truncation is what made the
// original defect invisible.
func (n *nameSet) setPins(source string, names []string) (dropped []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureMapsLocked()
	clean := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name != "" {
			clean[name] = struct{}{}
		}
	}
	if n.pins == nil {
		n.pins = make(map[string]map[string]struct{})
	}
	if len(clean) == 0 {
		delete(n.pins, source)
	} else {
		n.pins[source] = clean
	}
	want := make(map[string]struct{})
	for _, set := range n.pins {
		for name := range set {
			want[name] = struct{}{}
		}
	}
	// Release first, so a name that only changed families becomes available.
	for name := range n.pinned {
		if _, keep := want[name]; keep {
			continue
		}
		delete(n.pinned, name)
		n.bumpLocked(name)
		n.order = append(n.order, nameEntry{seq: n.seq[name], name: name})
	}
	// Acquire in sorted order so an over-cap operator scope truncates
	// deterministically instead of by map iteration.
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if _, isPinned := n.pinned[name]; isPinned {
			continue
		}
		if _, stored := n.names[name]; !stored {
			if len(n.names) >= nameSetMax && !n.evictLocked() {
				dropped = append(dropped, name)
				continue
			}
			n.names[name] = struct{}{}
			n.revision++
		}
		n.bumpLocked(name)
		n.pinned[name] = struct{}{}
	}
	n.compactLocked()
	return dropped
}

// observeLocked records name as observed traffic, refreshing an existing
// observed name's position and an absent one at the newest position.
func (n *nameSet) observeLocked(name string) {
	if name == "" {
		return
	}
	if _, stored := n.names[name]; stored {
		if _, isPinned := n.pinned[name]; isPinned {
			return // pinned names stay pinned; their position is irrelevant
		}
		n.bumpLocked(name)
		n.order = append(n.order, nameEntry{seq: n.seq[name], name: name})
		n.compactLocked()
		return
	}
	n.ensureMapsLocked()
	if len(n.names) >= nameSetMax && !n.evictLocked() {
		return
	}
	n.names[name] = struct{}{}
	n.revision++
	n.bumpLocked(name)
	n.order = append(n.order, nameEntry{seq: n.seq[name], name: name})
}

// bumpLocked assigns name a new generation, invalidating any older order entry
// that still names it.
func (n *nameSet) bumpLocked(name string) {
	n.next++
	n.seq[name] = n.next
}

// evictLocked drops the least recently observed name and reports whether one was
// free. Pinned names have no live order entry, so they can never be chosen.
func (n *nameSet) evictLocked() bool {
	for len(n.order) > 0 {
		entry := n.order[0]
		n.order = n.order[1:]
		if n.seq[entry.name] != entry.seq {
			continue // stale: re-observed, pinned, or already evicted
		}
		delete(n.names, entry.name)
		delete(n.seq, entry.name)
		n.revision++
		return true
	}
	return false
}

// compactLocked rebuilds the order list when stale entries outnumber the live
// ones, so a hot name that keeps being re-observed cannot grow it without
// bound.
func (n *nameSet) compactLocked() {
	if len(n.order) <= 2*len(n.names)+64 {
		return
	}
	entries := make([]nameEntry, 0, len(n.names))
	for name := range n.names {
		if _, isPinned := n.pinned[name]; isPinned {
			continue
		}
		entries = append(entries, nameEntry{seq: n.seq[name], name: name})
	}
	slices.SortFunc(entries, func(a, b nameEntry) int {
		switch {
		case a.seq < b.seq:
			return -1
		case a.seq > b.seq:
			return 1
		default:
			return 0
		}
	})
	n.order = entries
}

// list returns the known names in lexicographic order. The sorted view is
// rebuilt only when revision changes, and callers get a clone so appending to
// or mutating the result cannot corrupt the cache.
func (n *nameSet) list() []string {
	n.mu.Lock()
	if n.sortedRevision != n.revision {
		n.sorted = slices.Sorted(maps.Keys(n.names))
		n.sortedRevision = n.revision
	}
	out := slices.Clone(n.sorted)
	n.mu.Unlock()
	return out
}

func (s *Server) initPause() {
	s.pause.timers = make(map[string]*time.Timer)
	s.initDebug()
}

func newPauseID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "p" + time.Now().UTC().Format("150405.000000")
	}
	return hex.EncodeToString(b[:])
}

func decodePause(raw []byte) []persistedPause {
	if len(raw) == 0 {
		return nil
	}
	var doc persistedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("pause: decode: %v", err)
		return nil
	}
	if len(doc.Holds) == 0 {
		return nil
	}
	return doc.Holds
}

// AttachPausePersist restores the hold from durable storage and seeds the
// known-client / known-provider lists. Call once after New when a store is open.
func (s *Server) AttachPausePersist(p pausePersist) {
	s.pause.persist = p
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if cs, err := p.ListClients(ctx); err != nil {
		log.Printf("pause: list clients: %v", err)
	} else {
		s.pause.clients.seed(cs)
	}
	if ps, err := p.ListProviders(ctx); err != nil {
		log.Printf("pause: list providers: %v", err)
	} else {
		s.pause.providers.seed(ps)
	}
	if ms, err := p.ListModels(ctx); err != nil {
		log.Printf("pause: list models: %v", err)
	} else {
		s.pause.models.seed(canonicalModelList(ms))
	}
	raw, err := p.LoadPause(ctx)
	if err != nil {
		log.Printf("pause: load: %v", err)
	} else {
		holds := decodePause(raw)
		kept := holds[:0]
		for _, h := range holds {
			// KnownAtNew is the observed vocabulary snapshot the hold compared
			// against, so it seeds like history. The hold's own scope is derived
			// as a pin by syncNamePins once the documents are installed.
			s.pause.clients.seed(h.KnownAtNew)
			if !toSchedHold(h).Active() {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			if len(holds) > 0 {
				s.applyHolds(nil)
			}
		} else {
			s.applyHolds(kept)
			log.Printf("pause restored (%s)", pauseDescribeAll(kept))
		}
	}
	s.attachThrottlePersist(p)
	s.attachDebugPersist(p)
}

// defaultPauseCap is the default MaxQueued of a new hold: how many matching
// waiters the hold parks before Acquire refuses. The semantically matching
// setting is the per-group queue cap, not the per-group concurrency cap.
func (s *Server) defaultPauseCap() int {
	if s == nil {
		return 0
	}
	return s.cfg().MaxQueueSize
}

// HandlePause is GET/POST /admin/pause. GET returns current state; POST
// applies {"paused": bool} (required - deny by default). paused:true adds
// one hold (all/new/clients/providers/duration/max_queued). paused:true
// with id updates that hold in place (queued waiters stay parked).
// A replace that omits scope / duration keeps the previous values -
// {"paused":true,"id"} is not a global hold. Compat {"paused":true}
// with no id and no scope still is. paused:false clears everything,
// or just {"id"} when set. In-flight requests are never cancelled.
func (s *Server) HandlePause(w http.ResponseWriter, r *http.Request) {
	if !s.operatorStateGet(w, r, s.PauseSnapshot) {
		return
	}
	var body struct {
		Paused    *bool           `json:"paused"`
		All       pauseBool       `json:"all"`
		New       pauseBool       `json:"new"`
		Clients   pauseNames      `json:"clients"`
		Providers pauseNames      `json:"providers"`
		Duration  *string         `json:"duration"`
		MaxQueued *int            `json:"max_queued"`
		ID        json.RawMessage `json:"id"`
	}
	// Surface the strict decoder's cause; io.EOF is the empty-body
	// command, which falls through to the requirement message below.
	if err := adminjson.Decode(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Paused == nil {
		adminjson.WriteError(w, http.StatusBadRequest, "paused boolean required")
		return
	}
	id, err := adminjson.OptionalID(body.ID)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !*body.Paused {
		if id != "" {
			s.resetOperatorState(w, s.PauseSnapshot, func() error { return s.removePause(id, time.Time{}) },
				"proxy resumed hold %s", id)
		} else {
			s.resetOperatorState(w, s.PauseSnapshot, func() error { return s.applyHolds(nil) },
				"proxy resumed (in-flight finish, queued drain)")
		}
		return
	}
	hasPrev := id != ""
	var snap persistedPause
	err = s.editPersisted(id, func(prev persistedPause) (persistedPause, error) {
		dur, wait, ok := resolveHoldDuration(body.Duration, hasPrev, prev.Duration)
		if !ok {
			return prev, pauseDurationError()
		}
		if body.MaxQueued != nil && *body.MaxQueued < 0 {
			return prev, fmt.Errorf("max_queued must be >= 0")
		}
		// Scope presence is per dimension: an omitted dimension preserves the
		// edited hold's value, while an explicitly empty array clears it. A
		// create starts with every omitted dimension empty ("any").
		clients, providers := prev.Clients, prev.Providers
		if !hasPrev {
			clients, providers = nil, nil
		}
		if body.Clients != nil {
			clients = sanitizeNameList(body.Clients)
		}
		if body.Providers != nil {
			providers = sanitizeNameList(body.Providers)
		}
		newc := body.New.value
		scopeOmitted := !body.All.set && !body.New.set && body.Clients == nil && body.Providers == nil
		var all bool
		if hasPrev && scopeOmitted {
			// Replace without restated scope keeps the hold. Compat
			// {"paused":true} → All is only for a new hold.
			all, newc = prev.All, prev.New
		} else {
			all = scopeOmitted || body.All.value
			// An explicit empty / whitespace-only scope is not a
			// global hold - deny by default.
			if !all && !newc && len(clients) == 0 && len(providers) == 0 {
				return prev, fmt.Errorf("all, new, clients, or providers required")
			}
			if all {
				newc = false
				clients = nil
				providers = nil
			}
		}
		capn := s.defaultPauseCap()
		if body.MaxQueued != nil {
			capn = *body.MaxQueued
		} else if hasPrev {
			capn = prev.MaxQueued
		}
		snap = persistedPause{
			ID: newPauseID(), All: all, New: newc, Clients: clients,
			Providers: providers, Duration: dur, MaxQueued: capn,
			// An edit re-states scope, deadline and cap; the system origin
			// of an auto-created hold is never restated or cleared.
			Reason: prev.Reason,
		}
		if hasPrev {
			snap.ID = id
		}
		if newc {
			if hasPrev && prev.New {
				snap.KnownAtNew = prev.KnownAtNew
			} else {
				snap.KnownAtNew = s.pause.clients.list()
			}
		}
		snap.Until = mergeUntil(hasPrev, body.Duration == nil, dur, wait, prev.Duration, prev.Until)
		return snap, nil
	})
	if err != nil {
		s.writeOperatorEditError(w, err, scheduler.ErrOverlap, scheduler.ErrHoldNotFound,
			"pause overlaps existing hold", "pause hold not found", s.PauseSnapshot)
		return
	}
	if hasPrev {
		log.Printf("proxy pause updated %s (%s)", id, pauseDescribe(snap))
	} else {
		log.Printf("proxy paused (%s)", pauseDescribe(snap))
	}
	writeOperatorState(w, s.PauseSnapshot(), nil)
}

func sanitizeNameList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c != "" {
			seen[c] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func toSchedHold(h persistedPause) scheduler.Hold {
	return scheduler.Hold{
		ID: h.ID, All: h.All, New: h.New, Clients: h.Clients,
		Providers: h.Providers, KnownAtNew: h.KnownAtNew,
		Until: h.Until, MaxQueued: h.MaxQueued,
	}
}

func (s *Server) applyHolds(holds []persistedPause) error {
	for i := range holds {
		if holds[i].ID == "" {
			holds[i].ID = newPauseID()
		}
	}
	s.pause.mu.Lock()
	s.pause.holds = append([]persistedPause(nil), holds...)
	sh := make([]scheduler.Hold, 0, len(s.pause.holds))
	for _, h := range s.pause.holds {
		sh = append(sh, toSchedHold(h))
	}
	s.scheduler.RestoreHolds(sh)
	return s.commitPause()
}

// syncNamePins recomputes every pinned vocabulary from the live operator
// documents: each hold's scope and each debug session's scope. It is the single
// derivation owner, called whenever either document set changes, so a removed
// document releases its names instead of pinning them for the process lifetime.
// Names that could not be stored because the vocabulary is entirely pinned are
// logged, never dropped silently.
// firstN returns at most n entries, for a bounded log line.
func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

func (s *Server) syncNamePins() {
	s.pause.mu.Lock()
	var holdClients, holdProviders []string
	for _, h := range s.pause.holds {
		holdClients = append(holdClients, h.Clients...)
		holdProviders = append(holdProviders, h.Providers...)
	}
	s.pause.mu.Unlock()

	s.debug.mu.Lock()
	var debugClients, debugProviders, debugModels []string
	for _, d := range s.debug.sessions {
		debugClients = append(debugClients, d.Clients...)
		debugProviders = append(debugProviders, d.Providers...)
		debugModels = append(debugModels, canonicalModelList(d.Models)...)
	}
	s.debug.mu.Unlock()

	var dropped []string
	dropped = append(dropped, s.pause.clients.setPins("pause", holdClients)...)
	dropped = append(dropped, s.pause.clients.setPins("debug", debugClients)...)
	dropped = append(dropped, s.pause.providers.setPins("pause", holdProviders)...)
	dropped = append(dropped, s.pause.providers.setPins("debug", debugProviders)...)
	dropped = append(dropped, s.pause.models.setPins("debug", debugModels)...)
	if len(dropped) > 0 {
		log.Printf("pause: known-name vocabulary is full of operator scopes; %d name(s) not listed: %s",
			len(dropped), strings.Join(firstN(dropped, 8), ", "))
	}
}

// editPersisted owns the read/merge/check/commit transaction for both adding
// and editing a hold. An edit cannot restore stale omitted fields over another
// editor's scope, deadline or queue cap. edit must not perform response I/O.
func (s *Server) editPersisted(id string, edit func(persistedPause) (persistedPause, error)) error {
	s.pause.mu.Lock()
	idx := -1
	var prev persistedPause
	for i := range s.pause.holds {
		if s.pause.holds[i].ID == id {
			idx = i
			prev = s.pause.holds[i]
			break
		}
	}
	if id != "" && (idx < 0 || !toSchedHold(prev).Active()) {
		s.pause.mu.Unlock()
		return scheduler.ErrHoldNotFound
	}
	h, err := edit(prev)
	if err != nil {
		s.pause.mu.Unlock()
		return err
	}
	if idx >= 0 {
		h.ID = id
		err = s.scheduler.ReplaceHold(toSchedHold(h))
	} else {
		if h.ID == "" {
			h.ID = newPauseID()
		}
		err = s.scheduler.AddHold(toSchedHold(h))
	}
	if err != nil {
		s.pause.mu.Unlock()
		return err
	}
	if idx >= 0 {
		s.pause.holds[idx] = h
	} else {
		s.pause.holds = append(s.pause.holds, h)
	}
	return s.commitPause()
}

// A timer may already be executing when stopped/rearmed. expectedUntil pins
// its removal to the hold revision it observed; an edited/recreated hold with
// a newer deadline must remain parked. A zero deadline is an operator removal.
func (s *Server) removePause(id string, expectedUntil time.Time) error {
	if id == "" {
		return s.applyHolds(nil)
	}
	s.pause.mu.Lock()
	found := false
	for _, h := range s.pause.holds {
		if h.ID == id && (expectedUntil.IsZero() || h.Until.Equal(expectedUntil) && !time.Now().Before(h.Until)) {
			found = true
			break
		}
	}
	if !found {
		s.pause.mu.Unlock()
		return nil
	}
	s.scheduler.RemoveHold(id)
	kept := s.pause.holds[:0]
	for _, h := range s.pause.holds {
		if h.ID != id {
			kept = append(kept, h)
		}
	}
	s.pause.holds = append([]persistedPause(nil), kept...)
	return s.commitPause()
}

func (s *Server) finishPauseLocked() ([]persistedPause, uint64) {
	kept := s.pause.holds[:0]
	for _, h := range s.pause.holds {
		if !toSchedHold(h).Active() {
			s.scheduler.RemoveHold(h.ID)
			continue
		}
		kept = append(kept, h)
	}
	s.pause.holds = append([]persistedPause(nil), kept...)
	s.rearmTimersLocked()
	s.pause.persistGen++
	return append([]persistedPause(nil), s.pause.holds...), s.pause.persistGen
}

// commitPause finishes a committed policy transition - finalizing the hold
// set, releasing pause.mu, and persisting the accepted snapshot. The caller
// holds pauseMu and must not touch it after this returns; the same lock
// discipline as commitHolds applies.
func (s *Server) commitPause() error {
	snap, gen := s.finishPauseLocked()
	s.pause.mu.Unlock()
	// Every accepted hold mutation passes through here - create, edit, resume,
	// expiry and restore - so this is the one place pins are derived. Doing it
	// per handler instead would leave the removal and expiry paths pinning a
	// scope that no longer exists.
	s.syncNamePins()
	return s.persistLatest(snap, gen)
}

func (s *Server) persistHolds(holds []persistedPause) error {
	if s.pause.persist == nil {
		return nil
	}
	return s.persistOperatorDoc("pause", persistedDoc{Holds: holds}, s.pause.persist.SavePause)
}

func (s *Server) persistLatest(snap []persistedPause, gen uint64) error {
	return persistStableGen(snap, gen, &s.pause.mu,
		func() uint64 { return s.pause.persistGen },
		s.persistHolds,
		func() ([]persistedPause, uint64) {
			snap := append([]persistedPause(nil), s.pause.holds...)
			return snap, s.pause.persistGen
		})
}

func (s *Server) rearmTimersLocked() {
	for _, t := range s.pause.timers {
		t.Stop()
	}
	s.pause.timers = make(map[string]*time.Timer)
	now := time.Now()
	for _, h := range s.pause.holds {
		if h.Until.IsZero() {
			continue
		}
		d := h.Until.Sub(now)
		if d <= 0 {
			continue
		}
		id, until := h.ID, h.Until
		s.pause.timers[id] = time.AfterFunc(d, func() {
			s.removePause(id, until)
		})
	}
}

func pauseDescribeAll(holds []persistedPause) string {
	if len(holds) == 0 {
		return "cleared"
	}
	parts := make([]string, 0, len(holds))
	for _, h := range holds {
		parts = append(parts, pauseDescribe(h))
	}
	return strings.Join(parts, "; ")
}

func pauseDescribe(snap persistedPause) string {
	if !snap.All && !snap.New && len(snap.Clients) == 0 && len(snap.Providers) == 0 {
		return "cleared"
	}
	var b strings.Builder
	if snap.All {
		b.WriteString("all")
	} else {
		if snap.New {
			b.WriteString("new")
		}
		if len(snap.Clients) > 0 {
			if b.Len() > 0 {
				b.WriteString("+")
			}
			b.WriteString(strings.Join(snap.Clients, ","))
		}
		if len(snap.Providers) > 0 {
			if b.Len() > 0 {
				b.WriteString("+")
			}
			b.WriteString(strings.Join(snap.Providers, ","))
		}
	}
	if snap.Reason != "" {
		b.WriteString(" ")
		b.WriteString(snap.Reason)
	}
	if snap.Duration != "" {
		b.WriteString(" ")
		b.WriteString(snap.Duration)
	} else if !snap.Until.IsZero() {
		b.WriteString(" until ")
		b.WriteString(snap.Until.UTC().Format(time.RFC3339))
	}
	if snap.MaxQueued > 0 {
		b.WriteString(" cap ")
		b.WriteString(strconv.Itoa(snap.MaxQueued))
	}
	return b.String()
}

// PauseSnapshot builds the GET /admin/pause state document - the single
// owner of that shape, shared by the HTTP handler and the dashboard
// bootstrap payload (so the two can never disagree).
func (s *Server) PauseSnapshot() map[string]any {
	st := s.scheduler.Stats()
	s.pause.mu.Lock()
	holds := append([]persistedPause(nil), s.pause.holds...)
	s.pause.mu.Unlock()

	var clients, providers []string
	var soonest time.Time
	outHolds := make([]map[string]any, 0, len(holds))
	for _, h := range holds {
		if !toSchedHold(h).Active() {
			continue
		}
		clients = append(clients, h.Clients...)
		providers = append(providers, h.Providers...)
		if !h.Until.IsZero() && (soonest.IsZero() || h.Until.Before(soonest)) {
			soonest = h.Until
		}
		outHolds = append(outHolds, map[string]any{
			"id":           h.ID,
			"all":          h.All,
			"new":          h.New,
			"clients":      nullSlice(h.Clients),
			"providers":    nullSlice(h.Providers),
			"known_at_new": nullSlice(h.KnownAtNew),
			"duration":     h.Duration,
			"until":        rfc3339OrNil(h.Until),
			"max_queued":   h.MaxQueued,
			"reason":       h.Reason,
			"queued":       s.scheduler.HoldQueued(h.ID),
		})
	}
	return map[string]any{
		"ok":                 true,
		"paused":             len(outHolds) > 0,
		"clients":            nullSlice(sanitizeNameList(clients)),
		"providers":          nullSlice(sanitizeNameList(providers)),
		"holds":              outHolds,
		"known_clients":      nullSlice(s.pause.clients.list()),
		"known_providers":    nullSlice(s.pause.providers.list()),
		"until":              rfc3339OrNil(soonest),
		"default_max_queued": s.defaultPauseCap(),
		// Every request the scheduler is parking: admission waiters plus
		// send-gate waiters (429/5xx retry pacing, probe serialization).
		"queued": st.Queued + st.SendWaiters,
	}
}

func nullSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
