package proxy

// Operator queue draining: release requests the scheduler has parked without
// waiting for their provider limit, pause hold or retry window to open. This is
// flow control, not a failure signal: each released request answers with a
// retryable response and is recorded as queue_drained, never as a provider
// error. In-flight requests are untouched, matching pause semantics.

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
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
