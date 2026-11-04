package proxy

// Operator queue draining: release requests the scheduler has parked without
// waiting for their provider limit, pause hold or retry window to open. This is
// flow control, not a failure signal: each released request answers with a
// retryable response and is recorded as queue_drained, never as a provider
// error. In-flight requests are untouched, matching pause semantics.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// HandleQueueDrain is POST /admin/queue/drain. The body selects exactly one
// scope: {"all":true}, {"provider":...}, {"model":...} or {"client":...}. A
// missing or ambiguous scope is rejected rather than silently widened.
func (s *Server) HandleQueueDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		adminjson.WriteError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		All      bool   `json:"all"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Client   string `json:"client"`
	}
	if err := adminjson.Decode(w, r, &body); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	scope := scheduler.DrainScope{
		Provider: strings.TrimSpace(body.Provider),
		Model:    strings.TrimSpace(body.Model),
		Client:   strings.TrimSpace(body.Client),
	}
	set := 0
	for _, value := range []string{scope.Provider, scope.Model, scope.Client} {
		if value != "" {
			set++
		}
	}
	if body.All && set > 0 {
		adminjson.WriteError(w, http.StatusBadRequest, "all cannot be combined with provider, model, or client")
		return
	}
	if !body.All && set != 1 {
		adminjson.WriteError(w, http.StatusBadRequest, "exactly one of all, provider, model, or client required")
		return
	}
	if body.All {
		scope = scheduler.DrainScope{}
	}
	seq := s.scheduler.DrainQueue(scope)
	log.Printf("queue drained (%s)", drainScopeLabel(scope))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"drained_through": seq,
		"provider":        scope.Provider,
		"model":           scope.Model,
		"client":          scope.Client,
	})
}

func drainScopeLabel(scope scheduler.DrainScope) string {
	switch {
	case scope.Provider != "":
		return "provider " + scope.Provider
	case scope.Model != "":
		return "model " + scope.Model
	case scope.Client != "":
		return "client " + scope.Client
	default:
		return "all queued requests"
	}
}

// queueDrainedType is the record error type for a request an operator drain
// released. It is distinct from queue_full: the operator asked for the request
// to leave the queue, and the client may retry immediately.
const queueDrainedType = "queue_drained"

// queueDrainedMessage is the single client-facing sentence for a released
// request.
const queueDrainedMessage = "queued request released by the operator; retry"

// isQueueDrained reports whether err is the scheduler's operator-drain release.
func isQueueDrained(err error) bool {
	return errors.Is(err, scheduler.ErrQueueDrained)
}

// queueDrainedRecord stamps the released-by-operator outcome on rec. It is
// flow control, so the record never carries a provider error type or code.
func queueDrainedRecord(rec *metrics.Record, err error) {
	if rec.StatusCode == 0 {
		rec.StatusCode = http.StatusServiceUnavailable
	}
	rec.ErrorType = queueDrainedType
	rec.ErrorMsg = err.Error()
}

// writeQueueDrainedError answers an uncommitted response whose request an
// operator drain released: retryable 503 with Retry-After. It reports whether
// err was a drain release at all.
func (s *Server) writeQueueDrainedError(w http.ResponseWriter, rec *metrics.Record, inbound ir.Protocol, err error) bool {
	if !isQueueDrained(err) {
		return false
	}
	queueDrainedRecord(rec, err)
	writeClientErrorHdr(w, rec, inbound, "rate_limit_error", queueDrainedMessage, http.StatusServiceUnavailable, s.retryAfterHeader())
	return true
}

// queueDrainedStreamError is the in-band spelling for a response whose status
// line is already committed: a released request still reaches the client as a
// retryable error event instead of a silent close.
func queueDrainedStreamError(rec *metrics.Record, err error) (typ, msg string, ok bool) {
	if !isQueueDrained(err) {
		return "", "", false
	}
	queueDrainedRecord(rec, err)
	return "rate_limit_error", queueDrainedMessage, true
}
