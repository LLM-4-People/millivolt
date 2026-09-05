package proxy

// In-memory store of live, resumable Cursor Run streams, keyed so a follow-up
// client request that carries a tool result can find the parked conversation
// and write the result into the SAME open upstream stream (mimicking Cursor's
// agent loop, where the stream pauses on a tool call and resumes on the result).
//
// Correlation: each parked run is indexed by the exact tool_call_id(s) it
// surfaced. A follow-up request's role:"tool" messages carry those ids, so we
// look up the run by id - the only exact, client-agnostic signal (any
// OpenAI-compliant client echoes tool_call_id). Clients send CUMULATIVE
// history, so requests also carry consumed ids from earlier turns; those are
// ignored - the match is the longest TRAILING run of ids one parked run owns
// (the current turn's answers). A request whose trailing ids match no parked
// run cold-starts a fresh Run, never a hijack.
//
// Lifecycle: runs are in-memory only (a parked HTTP/2 stream can't survive a
// restart anyway). Parked runs expire after the configured idle TTL
// (cursor_park_ttl - long enough that a slow sub-agent still finds its parked
// stream; a restart or expiry falls back to a cold start with the continuation
// prompt) and are bounded in count; expiry/overflow closes the stream. This
// mirrors cursor-bridge's SessionStore invariants.

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
)

// cursorRunMax bounds parked runs so a flood of tool calls can't pin streams.
// Internal guardrail, not user-tunable (no operator scenario needs to raise
// it: the count is bounded by live sub-agent tool batches, not time).
const cursorRunMax = 256

// Only an immutable fingerprint is retained; no routing credential is stored.
type cursorScope [sha256.Size]byte

func (s *Server) cursorScopeFor(t *target, key, model, client string) cursorScope {
	auth := ""
	if key != "" {
		auth = hashKey(t.authPrefix + key)
	}
	raw, _ := json.Marshal(struct {
		URL, AuthHeader, AuthHash, Model, Client string
		Headers                                  map[string]string
	}{cursorRunURL(t), strings.ToLower(t.authHeader), auth, model, client, s.cfg().Providers[t.provider].Headers})
	return sha256.Sum256(raw)
}

type cursorCallKey struct {
	scope cursorScope
	id    string
}

// cursorRunEntry is one parked run.
type cursorRunEntry struct {
	run     *providerformat.CursorRun
	scope   cursorScope
	pending map[string]bool // tool_call_ids awaiting results
	at      time.Time       // when it was parked
}

type cursorRunStore struct {
	mu            sync.Mutex
	ttl           time.Duration
	byCall        map[cursorCallKey]*cursorRunEntry
	entries       map[*providerformat.CursorRun]*cursorRunEntry
	timer         *time.Timer
	timerRevision uint64
}

func newCursorRunStore(ttl time.Duration) *cursorRunStore {
	return &cursorRunStore{
		ttl:     ttl,
		byCall:  map[cursorCallKey]*cursorRunEntry{},
		entries: map[*providerformat.CursorRun]*cursorRunEntry{},
	}
}

// UpdateTTL re-applies the parked-run idle TTL (config hot-reload). Parked
// runs keep their parked-at time; the new TTL applies from the next sweep.
func (s *cursorRunStore) UpdateTTL(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = ttl
	s.sweepLocked()
	s.rearmLocked()
}

// park registers a run as parked on the given pending tool_call_ids. Returns
// false (and does not park) if at capacity - the caller then closes the run
// and the client cold-starts on the next request.
func (s *cursorRunStore) park(scope cursorScope, run *providerformat.CursorRun) bool {
	ids := run.PendingToolCallIDs()
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	return s.parkWith(scope, run, list)
}

// parkWith is park() with an explicit pending id list (used by tests to park
// fabricated runs).
func (s *cursorRunStore) parkWith(scope cursorScope, run *providerformat.CursorRun, ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rearmLocked()
	s.sweepLocked()
	if len(s.entries) >= cursorRunMax || run.Closed() || s.entries[run] != nil {
		return false
	}
	e := &cursorRunEntry{run: run, scope: scope, pending: map[string]bool{}, at: time.Now()}
	for _, id := range ids {
		if id == "" || e.pending[id] || s.byCall[cursorCallKey{scope, id}] != nil {
			return false
		}
		e.pending[id] = true
	}
	s.entries[run] = e
	for id := range e.pending {
		s.byCall[cursorCallKey{scope, id}] = e
	}
	return true
}

// findByToolTail returns the parked run owning the LONGEST contiguous trailing
// run of tool_result ids from a request, plus those ids (in message order).
// Clients send cumulative history, so a request carries every
// tool_call id ever seen; only the TRAILING ids are the current turn's answers
// - older consumed ids must be ignored, and an id no parked run owns (or one
// owned by a different run) ends the tail. On a match the entry is removed
// from the index (the run is being resumed now).
func (s *cursorRunStore) findByToolTail(scope cursorScope, ids []string) (*providerformat.CursorRun, []string) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.rearmLocked()
	s.sweepLocked()
	var target *cursorRunEntry
	var tail []string
	for i := len(ids) - 1; i >= 0; i-- {
		e, ok := s.byCall[cursorCallKey{scope, ids[i]}]
		if !ok {
			break // consumed/unknown id - everything after is history, not the answer
		}
		if target == nil {
			target = e
		} else if target != e {
			break // ids span two runs - stop at this run's tail
		}
		tail = append(tail, ids[i])
	}
	if target == nil || target.run.Closed() {
		return nil, nil
	}
	for l, r := 0, len(tail)-1; l < r; l, r = l+1, r-1 {
		tail[l], tail[r] = tail[r], tail[l]
	}
	s.removeLocked(target)
	return target.run, tail
}

// removeLocked drops an entry from both indexes.
func (s *cursorRunStore) removeLocked(e *cursorRunEntry) {
	delete(s.entries, e.run)
	for id := range e.pending {
		key := cursorCallKey{e.scope, id}
		if s.byCall[key] == e {
			delete(s.byCall, key)
		}
	}
}

// drop removes a run from the store (e.g. after it finishes or errors).
func (s *cursorRunStore) drop(run *providerformat.CursorRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[run]; ok {
		s.removeLocked(e)
		s.rearmLocked()
	}
}

// One store timer owns idle expiry independently of future request traffic.
// A revision check makes an already-fired stale callback harmless on resume.
func (s *cursorRunStore) rearmLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.timerRevision++
	var until time.Time
	for _, e := range s.entries {
		d := e.at.Add(s.ttl)
		if until.IsZero() || d.Before(until) {
			until = d
		}
	}
	if until.IsZero() {
		return
	}
	revision := s.timerRevision
	s.timer = time.AfterFunc(time.Until(until), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.timerRevision != revision {
			return
		}
		s.sweepLocked()
		s.rearmLocked()
	})
}

// sweepLocked closes and removes expired/closed parked runs. Caller holds mu.
func (s *cursorRunStore) sweepLocked() {
	now := time.Now()
	for run, e := range s.entries {
		if run.Closed() || now.Sub(e.at) >= s.ttl {
			s.removeLocked(e)
			go run.Close()
		}
	}
}
