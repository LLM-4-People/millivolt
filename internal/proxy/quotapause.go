package proxy

import (
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// isOpenAIWire is the single format predicate: it reports the neutral wire
// fact whether a target format speaks the OpenAI-compatible wire - the
// explicit "openai" spelling or the empty passthrough default. The quota
// pause keys on it (only these upstreams return the insufficient_quota /
// insufficient_credits envelope family the pause is defined on), and
// overrideBodyWireFormat extends it with the translated-anthropic target.
func isOpenAIWire(format string) bool {
	return format == "" || format == "openai"
}

// quotaPauseArmed reports whether any durable quota/billing reaction is
// configured. Off keeps every existing behavior: the 429 surfaces verbatim
// and nothing is parked or retried.
func (s *Server) quotaPauseArmed() bool {
	return s.cfg().QuotaPauseMode != config.QuotaPauseOff
}

// settleQuotaFailure owns the single durable-quota reaction shared by every
// upstream observation path (generic relay 429, in-band stream errors): it
// reports whether the quota pause parks this request - a configured mode on
// an OpenAI-wire target with a known provider - and settles the in-flight
// permit. Retry mode settles the permit as a quota observation, opening or
// re-pacing the provider's quota gate so the caller's next admission parks
// on the gate until its recovery window; manual mode cancels the permit and
// installs an indefinite provider-scoped operator hold, which the caller's
// next admission parks on through WaitSend's hold rule - but only when this
// request is actually held: an existing hold that parks other clients only
// (a client-scoped or new-client pause overlapping the provider pair)
// rejects the provider hold, and the request falls back to the historical
// surface-immediately answer rather than re-sending unparked. A request
// whose response already reached the client (the in-band path) only arms
// the pause. When false is returned the permit is released and the caller
// keeps its existing surface-immediately behavior.
func (s *Server) settleQuotaFailure(permit *scheduler.StormPermit, format, client, provider, reason string, retryAfter time.Duration) bool {
	if !s.quotaPauseArmed() || !isOpenAIWire(format) || provider == "" {
		permit.Cancel()
		return false
	}
	switch s.cfg().QuotaPauseMode {
	case config.QuotaPauseRetry:
		permit.ObserveQuota(reason, retryAfter)
		return true
	case config.QuotaPauseManual:
		permit.Cancel()
		return s.addQuotaHold(client, provider, reason)
	default:
		permit.Cancel()
		return false
	}
}

// addQuotaHold installs the manual-mode provider pause: an indefinite
// provider-scoped hold, labeled with the quota token, that resumes only
// through the operator pause surface. The in-memory hold lands synchronously
// so parking starts with the triggering request's next admission; the durable
// snapshot persists on a helper goroutine because the relay path must never
// wait on SQLite - the generation guard inside persistLatest keeps the write
// latest-wins with concurrent operator edits. ErrOverlap means some hold
// covers the provider pair, not necessarily this request: the park promise
// is per-request, so the outcome is decided by whether the triggering
// client is actually held. A crash before the write lands simply re-arms on
// the provider's next durable quota 429.
func (s *Server) addQuotaHold(client, provider, reason string) bool {
	h := persistedPause{
		ID:        newPauseID(),
		Providers: []string{provider},
		Reason:    reason,
		MaxQueued: s.defaultPauseCap(),
	}
	s.pause.mu.Lock()
	err := s.scheduler.AddHold(toSchedHold(h))
	if err != nil {
		s.pause.mu.Unlock()
		if errors.Is(err, scheduler.ErrOverlap) {
			// An existing hold already parks this request on the provider:
			// the manual pause holds it without creating a duplicate.
			return s.scheduler.HoldsTarget(client, provider)
		}
		log.Printf("quota pause: hold %s: %v", provider, err)
		return false
	}
	s.pause.holds = append(s.pause.holds, h)
	snap, gen := s.finishPauseLocked()
	s.pause.mu.Unlock()
	log.Printf("quota pause: provider %s paused (%s)", provider, reason)
	go func() {
		if err := s.persistLatest(snap, gen); err != nil {
			log.Printf("quota pause: hold %s: %v", provider, err)
		}
	}()
	return true
}

// HandleQuotaPause serves GET /admin/quota (the shared storm snapshot, which
// includes open quota gates) and POST {"provider": string, "resume": true},
// the operator action that closes a provider's open retry-mode quota gate
// without waiting for the next recovery probe; parked sends resume
// immediately. Resuming a provider without an open gate is a no-op state
// report (the pause may have recovered between banner and click).
// Manual-mode quota holds are ordinary operator holds and resume through
// the pause surface, not here.
func (s *Server) HandleQuotaPause(w http.ResponseWriter, r *http.Request) {
	if !s.operatorStateGet(w, r, s.StormSnapshot) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Resume   *bool  `json:"resume"`
	}
	// The strict decoder's cause surfaces verbatim; io.EOF is the empty-body
	// command, which falls through to the requirement message below.
	if err := adminjson.Decode(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Provider == "" || body.Resume == nil || !*body.Resume {
		adminjson.WriteError(w, http.StatusBadRequest, "provider and resume true required")
		return
	}
	if s.scheduler.ReleaseQuota(body.Provider) {
		log.Printf("quota pause: provider %s resumed by operator", body.Provider)
	}
	writeOperatorState(w, s.StormSnapshot(), nil)
}
