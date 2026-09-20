package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

var (
	errDebugOverlap  = errors.New("debug session overlaps existing session")
	errDebugNotFound = errors.New("debug session not found")
)

// persistedDebug is one reboot-surviving operator debug session. Until is
// absolute so a restart mid-window keeps the remaining capture window.
type persistedDebug struct {
	ID        string    `json:"id,omitempty"`
	Clients   []string  `json:"clients,omitempty"`
	Providers []string  `json:"providers,omitempty"`
	Models    []string  `json:"models,omitempty"`
	Duration  string    `json:"duration,omitempty"`
	Until     time.Time `json:"until,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

type persistedDebugDoc struct {
	Sessions []persistedDebug `json:"sessions,omitempty"`
}

type debugRuntime struct {
	mu         sync.Mutex
	sessions   []persistedDebug
	timers     map[string]*time.Timer
	persistGen uint64
}

func (s *Server) initDebug() {
	s.debug.timers = make(map[string]*time.Timer)
}

func decodeDebug(raw []byte) []persistedDebug {
	if len(raw) == 0 {
		return nil
	}
	var doc persistedDebugDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("debug: decode: %v", err)
		return nil
	}
	if len(doc.Sessions) == 0 {
		return nil
	}
	return doc.Sessions
}

func (s *Server) attachDebugPersist(p pausePersist) {
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if _, err := p.ExpireDebugCaptures(ctx, time.Now().UnixMilli()); err != nil {
		log.Printf("debug: expire: %v", err)
	}
	raw, err := p.LoadDebugSessions(ctx)
	if err != nil {
		log.Printf("debug: load: %v", err)
		return
	}
	sessions := decodeDebug(raw)
	now := time.Now()
	kept := sessions[:0]
	for _, d := range sessions {
		s.pause.clients.seed(d.Clients)
		s.pause.providers.seed(d.Providers)
		s.pause.models.seed(canonicalModelList(d.Models))
		if !d.Until.IsZero() && now.After(d.Until) {
			continue
		}
		if len(d.Clients) == 0 && len(d.Providers) == 0 && len(d.Models) == 0 {
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		if len(sessions) > 0 {
			s.applyDebug(nil)
		}
		return
	}
	s.applyDebug(kept)
	log.Printf("debug restored (%s)", debugDescribeAll(kept))
}

func canonicalModel(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return ""
	}
	return providerformat.CursorModelBase(m)
}

func canonicalModelList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, m := range in {
		if c := canonicalModel(m); c != "" {
			out = append(out, c)
		}
	}
	return sanitizeNameList(out)
}

func debugSessionMatches(d persistedDebug, client, provider, model string) bool {
	if len(d.Clients) > 0 && !slices.Contains(d.Clients, client) {
		return false
	}
	if len(d.Providers) > 0 && !slices.Contains(d.Providers, provider) {
		return false
	}
	if len(d.Models) > 0 && !modelIn(d.Models, model) {
		return false
	}
	return len(d.Clients) > 0 || len(d.Providers) > 0 || len(d.Models) > 0
}

func modelIn(list []string, model string) bool {
	want := canonicalModel(model)
	if want == "" {
		return false
	}
	for _, x := range list {
		if canonicalModel(x) == want {
			return true
		}
	}
	return false
}

func debugSessionsOverlap(a, b persistedDebug) bool {
	return debugDimOverlap(a.Clients, b.Clients) &&
		debugDimOverlap(a.Providers, b.Providers) &&
		debugDimOverlap(canonicalModelList(a.Models), canonicalModelList(b.Models))
}

// debugDimOverlap is true when two dimension filters can match the same
// value: empty = any, so it overlaps everything; otherwise the named sets
// must share a name.
func debugDimOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	seen := make(map[string]struct{}, len(a))
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, x := range b {
		if _, ok := seen[x]; ok {
			return true
		}
	}
	return false
}

func (s *Server) matchDebug(client, provider, model string) *persistedDebug {
	s.debug.mu.Lock()
	defer s.debug.mu.Unlock()
	now := time.Now()
	for i := range s.debug.sessions {
		d := s.debug.sessions[i]
		if !d.Until.IsZero() && now.After(d.Until) {
			continue
		}
		if debugSessionMatches(d, client, provider, model) {
			out := d
			return &out
		}
	}
	return nil
}

func (s *Server) debugOverlapsLocked(cand persistedDebug, skipID string) bool {
	for _, d := range s.debug.sessions {
		if d.ID == skipID {
			continue
		}
		if !d.Until.IsZero() && time.Now().After(d.Until) {
			continue
		}
		if debugSessionsOverlap(cand, d) {
			return true
		}
	}
	return false
}

func (s *Server) applyDebug(sessions []persistedDebug) error {
	for i := range sessions {
		if sessions[i].ID == "" {
			sessions[i].ID = newPauseID()
		}
		if sessions[i].StartedAt.IsZero() {
			sessions[i].StartedAt = time.Now()
		}
		sessions[i].Models = canonicalModelList(sessions[i].Models)
	}
	s.debug.mu.Lock()
	s.debug.sessions = append([]persistedDebug(nil), sessions...)
	return s.commitDebug()
}

// editDebug owns the read/merge/check/commit transaction for both adding and
// editing a session. Omitted fields are read from the latest accepted state;
// edit runs under the policy lock and must not perform response I/O.
func (s *Server) editDebug(id string, edit func(persistedDebug) (persistedDebug, error)) error {
	s.debug.mu.Lock()
	idx := -1
	var prev persistedDebug
	for i := range s.debug.sessions {
		if s.debug.sessions[i].ID == id {
			idx = i
			prev = s.debug.sessions[i]
			break
		}
	}
	if id != "" && (idx < 0 || !prev.Until.IsZero() && !time.Now().Before(prev.Until)) {
		s.debug.mu.Unlock()
		return errDebugNotFound
	}
	d, err := edit(prev)
	if err != nil {
		s.debug.mu.Unlock()
		return err
	}
	if idx >= 0 {
		d.ID = id
		d.StartedAt = prev.StartedAt
	} else if d.ID == "" {
		d.ID = newPauseID()
	}
	if d.StartedAt.IsZero() {
		d.StartedAt = time.Now()
	}
	d.Models = canonicalModelList(d.Models)
	if len(d.Clients) == 0 && len(d.Providers) == 0 && len(d.Models) == 0 {
		s.debug.mu.Unlock()
		return errors.New("clients, providers, or models required")
	}
	if s.debugOverlapsLocked(d, id) {
		s.debug.mu.Unlock()
		return errDebugOverlap
	}
	if idx >= 0 {
		s.debug.sessions[idx] = d
	} else {
		s.debug.sessions = append(s.debug.sessions, d)
	}
	return s.commitDebug()
}

// A timer already waiting on mu may outlive Stop. It can retire only the
// exact expiry it was armed for, never a subsequently extended session.
func (s *Server) removeDebug(id string, expectedUntil time.Time) (bool, error) {
	if id == "" {
		return true, s.applyDebug(nil)
	}
	s.debug.mu.Lock()
	found := false
	for _, d := range s.debug.sessions {
		if d.ID == id && (expectedUntil.IsZero() || d.Until.Equal(expectedUntil)) {
			found = true
			break
		}
	}
	if !found {
		s.debug.mu.Unlock()
		return false, nil
	}
	kept := s.debug.sessions[:0]
	for _, d := range s.debug.sessions {
		if d.ID != id || (!expectedUntil.IsZero() && !d.Until.Equal(expectedUntil)) {
			kept = append(kept, d)
		}
	}
	s.debug.sessions = append([]persistedDebug(nil), kept...)
	return true, s.commitDebug()
}

func (s *Server) finishDebugLocked() ([]persistedDebug, uint64) {
	now := time.Now()
	kept := s.debug.sessions[:0]
	for _, d := range s.debug.sessions {
		if !d.Until.IsZero() && now.After(d.Until) {
			continue
		}
		kept = append(kept, d)
	}
	s.debug.sessions = append([]persistedDebug(nil), kept...)
	s.rearmDebugTimersLocked()
	s.debug.persistGen++
	return append([]persistedDebug(nil), s.debug.sessions...), s.debug.persistGen
}

// commitDebug finishes a committed policy transition - finalizing the
// session set, releasing debug.mu, and persisting the accepted snapshot. The
// caller holds debug.mu and must not touch it after this returns; the same
// lock discipline as commitPause applies.
func (s *Server) commitDebug() error {
	snap, gen := s.finishDebugLocked()
	s.debug.mu.Unlock()
	return s.persistDebugLatest(snap, gen)
}

func (s *Server) persistDebugSessions(sessions []persistedDebug) error {
	if s.pause.persist == nil {
		return nil
	}
	return s.persistOperatorDoc("debug", persistedDebugDoc{Sessions: sessions}, s.pause.persist.SaveDebugSessions)
}

func (s *Server) persistDebugLatest(snap []persistedDebug, gen uint64) error {
	return persistStableGen(snap, gen, &s.debug.mu,
		func() uint64 { return s.debug.persistGen },
		s.persistDebugSessions,
		func() ([]persistedDebug, uint64) {
			snap := append([]persistedDebug(nil), s.debug.sessions...)
			return snap, s.debug.persistGen
		})
}

func (s *Server) rearmDebugTimersLocked() {
	for _, t := range s.debug.timers {
		t.Stop()
	}
	s.debug.timers = make(map[string]*time.Timer)
	now := time.Now()
	for _, d := range s.debug.sessions {
		if d.Until.IsZero() {
			continue
		}
		wait := d.Until.Sub(now)
		if wait <= 0 {
			continue
		}
		id, until := d.ID, d.Until
		s.debug.timers[id] = time.AfterFunc(wait, func() {
			if removed, _ := s.removeDebug(id, until); removed {
				log.Printf("debug session %s ended (timer elapsed)", id)
			}
		})
	}
}

func debugDescribeAll(sessions []persistedDebug) string {
	if len(sessions) == 0 {
		return "cleared"
	}
	parts := make([]string, 0, len(sessions))
	for _, d := range sessions {
		parts = append(parts, debugDescribe(d))
	}
	return strings.Join(parts, "; ")
}

func debugDescribe(d persistedDebug) string {
	var b strings.Builder
	if len(d.Clients) > 0 {
		b.WriteString(strings.Join(d.Clients, ","))
	}
	if len(d.Providers) > 0 {
		if b.Len() > 0 {
			b.WriteString("+")
		}
		b.WriteString(strings.Join(d.Providers, ","))
	}
	if len(d.Models) > 0 {
		if b.Len() > 0 {
			b.WriteString("+")
		}
		b.WriteString(strings.Join(d.Models, ","))
	}
	if b.Len() == 0 {
		return "cleared"
	}
	if d.Duration != "" {
		b.WriteString(" ")
		b.WriteString(d.Duration)
	} else if !d.Until.IsZero() {
		b.WriteString(" until ")
		b.WriteString(d.Until.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// HandleDebug is GET/POST /admin/debug. GET returns current sessions; POST
// applies {"enabled": bool} (required - deny by default). enabled:true adds
// one session (clients/providers/models AND, duration). enabled:true with id
// updates that session in place. A replace that omits scope / duration keeps
// the previous values. enabled:false clears everything, or just {"id"} when
// set. Overlapping filters return 409.
func (s *Server) HandleDebug(w http.ResponseWriter, r *http.Request) {
	if !s.operatorStateGet(w, r, s.DebugSnapshot) {
		return
	}
	var body struct {
		Enabled   *bool           `json:"enabled"`
		Clients   []string        `json:"clients"`
		Providers []string        `json:"providers"`
		Models    []string        `json:"models"`
		Duration  *string         `json:"duration"`
		ID        json.RawMessage `json:"id"`
	}
	// Surface the strict decoder's cause; io.EOF is the empty-body
	// command, which falls through to the requirement message below.
	if err := adminjson.Decode(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Enabled == nil {
		adminjson.WriteError(w, http.StatusBadRequest, "enabled boolean required")
		return
	}
	id, err := adminjson.OptionalID(body.ID)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !*body.Enabled {
		if id != "" {
			s.resetOperatorState(w, s.DebugSnapshot, func() error { _, err := s.removeDebug(id, time.Time{}); return err },
				"debug session %s stopped", id)
		} else {
			s.resetOperatorState(w, s.DebugSnapshot, func() error { return s.applyDebug(nil) },
				"debug sessions cleared")
		}
		return
	}
	hasPrev := id != ""
	var snap persistedDebug
	err = s.editDebug(id, func(prev persistedDebug) (persistedDebug, error) {
		dur, wait, ok := resolveHoldDuration(body.Duration, hasPrev, prev.Duration)
		if !ok {
			return prev, pauseDurationError()
		}
		clients := sanitizeNameList(body.Clients)
		providers := sanitizeNameList(body.Providers)
		models := body.Models
		scopeOmitted := body.Clients == nil && body.Providers == nil && body.Models == nil
		if hasPrev && scopeOmitted {
			clients, providers, models = prev.Clients, prev.Providers, prev.Models
		}
		snap = persistedDebug{
			Clients: clients, Providers: providers, Models: models, Duration: dur,
		}
		snap.Until = mergeUntil(hasPrev, body.Duration == nil, dur, wait, prev.Duration, prev.Until)
		return snap, nil
	})
	if err != nil {
		s.writeOperatorEditError(w, err, errDebugOverlap, errDebugNotFound,
			"debug session overlaps existing session", "debug session not found", s.DebugSnapshot)
		return
	}
	if hasPrev {
		log.Printf("debug session updated %s (%s)", id, debugDescribe(snap))
	} else {
		log.Printf("debug session started (%s)", debugDescribe(snap))
	}
	writeOperatorState(w, s.DebugSnapshot(), nil)
}

// DebugSnapshot builds the GET /admin/debug state document - the single
// owner of that shape, shared by the HTTP handler and the dashboard
// bootstrap payload (so the two can never disagree).
func (s *Server) DebugSnapshot() map[string]any {
	s.debug.mu.Lock()
	sessions := append([]persistedDebug(nil), s.debug.sessions...)
	s.debug.mu.Unlock()

	var soonest time.Time
	now := time.Now()
	out := make([]map[string]any, 0, len(sessions))
	active := 0
	for _, d := range sessions {
		if !d.Until.IsZero() && now.After(d.Until) {
			continue
		}
		active++
		if !d.Until.IsZero() && (soonest.IsZero() || d.Until.Before(soonest)) {
			soonest = d.Until
		}
		var captures int64
		if s.pause.persist != nil && d.ID != "" {
			ctx, cancel := s.storeQueryCtx()
			n, err := s.pause.persist.CountDebugSession(ctx, d.ID)
			cancel()
			if err != nil {
				log.Printf("debug: count session %s: %v", d.ID, err)
			} else {
				captures = n
			}
		}
		ranMs := int64(0)
		if !d.StartedAt.IsZero() {
			end := now
			if !d.Until.IsZero() && d.Until.Before(now) {
				end = d.Until
			}
			ranMs = end.Sub(d.StartedAt).Milliseconds()
			if ranMs < 0 {
				ranMs = 0
			}
		}
		out = append(out, map[string]any{
			"id":        d.ID,
			"clients":   nullSlice(d.Clients),
			"providers": nullSlice(d.Providers),
			"models":    nullSlice(d.Models),
			"duration":  d.Duration,
			"until":     rfc3339OrNil(d.Until),
			"ran_ms":    ranMs,
			"captures":  captures,
		})
	}
	knownModels := s.pause.models.list()
	state := map[string]any{
		"ok":              true,
		"enabled":         active > 0,
		"sessions":        out,
		"known_clients":   nullSlice(s.pause.clients.list()),
		"known_providers": nullSlice(s.pause.providers.list()),
		"known_models":    nullSlice(knownModels),
		"until":           rfc3339OrNil(soonest),
		"ttl":             config.FormatDuration(s.cfg().DebugCaptureTTL),
		"max_bytes":       config.FormatByteSize(int64(s.cfg().DebugCaptureMaxBytes)),
	}
	if s.ModelObserver != nil {
		names := append([]string(nil), knownModels...)
		for _, session := range sessions {
			names = append(names, session.Models...)
		}
		state["model_canon"] = s.ModelObserver(names)
	}
	return state
}

// HandleDebugCapture is GET /admin/debug/capture?id= - the drawer fetch for
// one sidecar document. 404 when missing/expired. Never on the live SSE path.
// The plain response is the drawer's render JSON; download=1 serves the same
// document as a saved gzip artifact. The query is parsed from the raw string
// through the shared strict owner (adminjson.StrictQuery) so no malformed
// pair is silently dropped: an unparsable query, a repeated flag or id, or a
// flag outside the standing 0/1 grammar is a 400, never first-wins; an empty
// download value means the render mode. The kinship with the export owner
// stops at unknown keys, a deliberate split in the three-class route
// taxonomy: a filter grammar (the export) rejects unknown keys, an
// idempotent fetch like this one ignores them - only the consumed id and
// download are validated - and a mutating closed-set route (the backup and
// restore plane) rejects every key outside its consumed set. The method,
// id, no-store and 404 semantics are identical in both modes.
func (s *Server) HandleDebugCapture(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		adminjson.WriteError(w, http.StatusMethodNotAllowed, "GET")
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	// The consumed keys' repeated-key rule reads through the shared owner
	// (adminjson.DuplicateQueryKey) in the export owner's wording; the
	// pinned 400 precedence keeps a repeated id ahead of the id gate and a
	// repeated download ahead of the flag grammar.
	if err := adminjson.DuplicateQueryKey(q, "id"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		adminjson.WriteError(w, http.StatusBadRequest, "id required")
		return
	}
	download := q.Get("download")
	if err := adminjson.DuplicateQueryKey(q, "download"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if download != "" && download != "0" && download != "1" {
		adminjson.WriteError(w, http.StatusBadRequest, "download: expected 0 or 1")
		return
	}
	if s.pause.persist == nil {
		// 404, not 503: with no store wired there is no capture to fetch.
		// The message is the one storage-disabled sentinel's canonical text
		// (constant-only import; the persist seam itself stays an interface).
		adminjson.WriteError(w, http.StatusNotFound, storage.ErrStorageDisabled.Error())
		return
	}
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	raw, err := s.pause.persist.LoadDebugCapture(ctx, id)
	if err != nil {
		log.Printf("debug: load capture %s: %v", id, err)
		adminjson.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(raw) == 0 {
		adminjson.WriteError(w, http.StatusNotFound, "debug capture not found")
		return
	}
	if download == "1" {
		// bytes.Buffer writes cannot fail, so the artifact (bounded by the
		// capture size cap) is complete before the 200 starts.
		var compressed bytes.Buffer
		zw := NewArtifactGzipWriter(&compressed)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		WriteArtifactHeaders(w, "debug", "json.gz", "application/gzip", debugCaptureStamp(raw), id)
		w.Write(compressed.Bytes())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}

// debugCaptureStamp extracts the capture document's recorded moment for the
// download artifact name (stored UTC as RFC 3339Nano; any offset converts to
// UTC). Every decode or parse failure returns the zero time, and the caller's
// fallback names the artifact from the record id instead - a damaged payload
// still downloads with a deterministic name.
func debugCaptureStamp(raw []byte) time.Time {
	var doc struct {
		CapturedAt string `json:"captured_at"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, doc.CapturedAt)
	if err != nil {
		return time.Time{}
	}
	return at
}
