package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
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
// restart mid-window keeps the remaining time.
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

type pauseRuntime struct {
	persist    pausePersist
	seenMu     sync.Mutex
	seen       map[string]struct{}
	seenProv   map[string]struct{}
	seenModel  map[string]struct{}
	mu         sync.Mutex
	holds      []persistedPause
	timers     map[string]*time.Timer
	persistGen uint64
}

func (s *Server) initPause() {
	s.pause.seen = make(map[string]struct{})
	s.pause.seenProv = make(map[string]struct{})
	s.pause.seenModel = make(map[string]struct{})
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
		s.seedSeen(cs)
	}
	if ps, err := p.ListProviders(ctx); err != nil {
		log.Printf("pause: list providers: %v", err)
	} else {
		s.seedSeenProv(ps)
	}
	if ms, err := p.ListModels(ctx); err != nil {
		log.Printf("pause: list models: %v", err)
	} else {
		s.seedSeenModel(ms)
	}
	raw, err := p.LoadPause(ctx)
	if err != nil {
		log.Printf("pause: load: %v", err)
	} else {
		holds := decodePause(raw)
		kept := holds[:0]
		for _, h := range holds {
			s.seedSeen(h.Clients)
			s.seedSeen(h.KnownAtNew)
			s.seedSeenProv(h.Providers)
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

func (s *Server) seedSeen(cs []string) {
	s.pause.seenMu.Lock()
	defer s.pause.seenMu.Unlock()
	if s.pause.seen == nil {
		s.pause.seen = make(map[string]struct{})
	}
	for _, c := range cs {
		if c != "" {
			s.pause.seen[c] = struct{}{}
		}
	}
}

func (s *Server) seedSeenProv(ps []string) {
	s.pause.seenMu.Lock()
	defer s.pause.seenMu.Unlock()
	if s.pause.seenProv == nil {
		s.pause.seenProv = make(map[string]struct{})
	}
	for _, p := range ps {
		if p != "" {
			s.pause.seenProv[p] = struct{}{}
		}
	}
}

func (s *Server) noteClient(c string) {
	if c == "" {
		return
	}
	s.pause.seenMu.Lock()
	if s.pause.seen == nil {
		s.pause.seen = make(map[string]struct{})
	}
	s.pause.seen[c] = struct{}{}
	s.pause.seenMu.Unlock()
}

func (s *Server) noteProvider(p string) {
	if p == "" {
		return
	}
	s.pause.seenMu.Lock()
	if s.pause.seenProv == nil {
		s.pause.seenProv = make(map[string]struct{})
	}
	s.pause.seenProv[p] = struct{}{}
	s.pause.seenMu.Unlock()
}

func (s *Server) seedSeenModel(ms []string) {
	s.pause.seenMu.Lock()
	defer s.pause.seenMu.Unlock()
	if s.pause.seenModel == nil {
		s.pause.seenModel = make(map[string]struct{})
	}
	for _, m := range ms {
		if c := canonicalModel(m); c != "" {
			s.pause.seenModel[c] = struct{}{}
		}
	}
}

func (s *Server) noteModel(m string) {
	m = canonicalModel(m)
	if m == "" {
		return
	}
	s.pause.seenMu.Lock()
	if s.pause.seenModel == nil {
		s.pause.seenModel = make(map[string]struct{})
	}
	s.pause.seenModel[m] = struct{}{}
	s.pause.seenMu.Unlock()
}

func (s *Server) knownModels() []string {
	s.pause.seenMu.Lock()
	out := make([]string, 0, len(s.pause.seenModel))
	for m := range s.pause.seenModel {
		out = append(out, m)
	}
	s.pause.seenMu.Unlock()
	sort.Strings(out)
	return out
}

func (s *Server) knownClients() []string {
	s.pause.seenMu.Lock()
	out := make([]string, 0, len(s.pause.seen))
	for c := range s.pause.seen {
		out = append(out, c)
	}
	s.pause.seenMu.Unlock()
	sort.Strings(out)
	return out
}

func (s *Server) knownProviders() []string {
	s.pause.seenMu.Lock()
	out := make([]string, 0, len(s.pause.seenProv))
	for p := range s.pause.seenProv {
		out = append(out, p)
	}
	s.pause.seenMu.Unlock()
	sort.Strings(out)
	return out
}

func (s *Server) defaultPauseCap() int {
	if s == nil {
		return 0
	}
	return s.cfg().MaxConcurrent
}

// SetPaused parks every client (true) or clears the hold (false).
// In-flight slots finish; new requests queue until resume.
func (s *Server) SetPaused(paused bool) {
	if paused {
		s.applyHolds([]persistedPause{{ID: newPauseID(), All: true}})
		return
	}
	s.applyHolds(nil)
}

// PauseStats is the scheduler occupancy snapshot (paused / in-flight / queued).
func (s *Server) PauseStats() scheduler.Stats {
	return s.scheduler.Stats()
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
	if !rejectUnlessGetPost(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		writeOperatorState(w, s.PauseSnapshot(), nil)
	case http.MethodPost:
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
		if err := adminjson.Decode(w, r, &body); err != nil || body.Paused == nil {
			http.Error(w, `{"error":"paused boolean required"}`, http.StatusBadRequest)
			return
		}
		id, err := adminjson.OptionalID(body.ID)
		if err != nil {
			http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
			return
		}
		if !*body.Paused {
			var persistErr error
			if id != "" {
				persistErr = s.removePause(id, time.Time{})
				log.Printf("proxy resumed hold %s", id)
			} else {
				persistErr = s.applyHolds(nil)
				log.Printf("proxy resumed (in-flight finish, queued drain)")
			}
			writeOperatorState(w, s.PauseSnapshot(), persistErr)
			return
		}
		hasPrev := id != ""
		var snap persistedPause
		err = s.editPersisted(id, func(prev persistedPause) (persistedPause, error) {
			dur := ""
			if body.Duration != nil {
				dur = strings.TrimSpace(*body.Duration)
			} else if hasPrev {
				dur = prev.Duration
			}
			wait, ok := pauseDurations[dur]
			if !ok {
				return prev, pauseDurationError()
			}
			if body.MaxQueued != nil && *body.MaxQueued < 0 {
				return prev, fmt.Errorf("max_queued must be >= 0")
			}
			clients := sanitizeNameList(body.Clients)
			providers := sanitizeNameList(body.Providers)
			newc := body.New.value
			scopeOmitted := !body.All.set && !body.New.set && body.Clients == nil && body.Providers == nil
			var all bool
			if hasPrev && scopeOmitted {
				// Replace without restated scope keeps the hold. Compat
				// {"paused":true} → All is only for a new hold.
				all, newc = prev.All, prev.New
				clients, providers = prev.Clients, prev.Providers
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
			}
			if hasPrev {
				snap.ID = id
			}
			if newc {
				if hasPrev && prev.New {
					snap.KnownAtNew = prev.KnownAtNew
				} else {
					snap.KnownAtNew = s.knownClients()
				}
			}
			if hasPrev && body.Duration == nil {
				snap.Until = prev.Until
			} else if wait > 0 {
				if hasPrev && prev.Duration == dur && !prev.Until.IsZero() && time.Now().Before(prev.Until) {
					snap.Until = prev.Until
				} else {
					snap.Until = time.Now().Add(wait)
				}
			}
			return snap, nil
		})
		if err != nil {
			var persistErr *operatorPersistenceError
			if errors.As(err, &persistErr) {
				writeOperatorState(w, s.PauseSnapshot(), persistErr)
				return
			}
			if errors.Is(err, scheduler.ErrOverlap) {
				http.Error(w, `{"error":"pause overlaps existing hold"}`, http.StatusConflict)
				return
			}
			if errors.Is(err, scheduler.ErrHoldNotFound) {
				http.Error(w, `{"error":"pause hold not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		if hasPrev {
			log.Printf("proxy pause updated %s (%s)", id, pauseDescribe(snap))
		} else {
			log.Printf("proxy paused (%s)", pauseDescribe(snap))
		}
		writeOperatorState(w, s.PauseSnapshot(), nil)
	}
}

func sanitizeNameList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
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
	snap, gen := s.finishPauseLocked()
	s.pause.mu.Unlock()
	return s.persistLatest(snap, gen)
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
	snap, gen := s.finishPauseLocked()
	s.pause.mu.Unlock()
	return s.persistLatest(snap, gen)
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
	snap, gen := s.finishPauseLocked()
	s.pause.mu.Unlock()
	return s.persistLatest(snap, gen)
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

func (s *Server) persistHolds(holds []persistedPause) error {
	if s.pause.persist == nil {
		return nil
	}
	raw, err := json.Marshal(persistedDoc{Holds: holds})
	if err != nil {
		log.Printf("pause: persist: %v", err)
		return err
	}
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if err := s.pause.persist.SavePause(ctx, raw); err != nil {
		log.Printf("pause: persist: %v", err)
		return err
	}
	return nil
}

func (s *Server) persistLatest(snap []persistedPause, gen uint64) error {
	for {
		err := s.persistHolds(snap)
		s.pause.mu.Lock()
		if s.pause.persistGen == gen {
			s.pause.mu.Unlock()
			if err != nil {
				return &operatorPersistenceError{err}
			}
			return nil
		}
		snap = append([]persistedPause(nil), s.pause.holds...)
		gen = s.pause.persistGen
		s.pause.mu.Unlock()
	}
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
	var until any
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
		var u any
		if !h.Until.IsZero() {
			u = h.Until.UTC().Format(time.RFC3339)
		}
		outHolds = append(outHolds, map[string]any{
			"id":           h.ID,
			"all":          h.All,
			"new":          h.New,
			"clients":      nullSlice(h.Clients),
			"providers":    nullSlice(h.Providers),
			"known_at_new": nullSlice(h.KnownAtNew),
			"duration":     h.Duration,
			"until":        u,
			"max_queued":   h.MaxQueued,
			"queued":       s.scheduler.HoldQueued(h.ID),
		})
	}
	if !soonest.IsZero() {
		until = soonest.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"ok":                 true,
		"paused":             len(outHolds) > 0,
		"clients":            nullSlice(sanitizeNameList(clients)),
		"providers":          nullSlice(sanitizeNameList(providers)),
		"holds":              outHolds,
		"known_clients":      nullSlice(s.knownClients()),
		"known_providers":    nullSlice(s.knownProviders()),
		"until":              until,
		"default_max_queued": s.defaultPauseCap(),
		"queued":             st.Queued,
	}
}

func nullSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
