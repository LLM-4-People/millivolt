package proxy

import (
	"encoding/json"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the bounded test timeout expires. A
// fixed sleep guesses how long another goroutine needs to reach an observable
// state; under suite load that guess either wastes wall time or flakes. Poll
// the state itself and let the caller phrase the failure when the deadline
// passes.
//
// Literal sleeps remain only where the duration is the subject, and their
// fixtures are intentionally visible: upstream handlers that must stall past a
// deadline or trickle chunks (pause.go, keepalive files, sse_pacer_body_type.go,
// translated_timing.go), the deliberate negative-proof windows that assert an
// event did NOT happen before a bound (pause.go, error_surface.go,
// recheck_regressions.go, stream_idle.go), pacer cadence (retry_mode.go), the
// one-second create-stamp boundary (cursor_helpers.go), and request-id time
// ordering (request_id.go).
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// snapshotWithoutClock decodes one operator-document body and removes the
// server clock field, so two independently-taken snapshots can be compared for
// everything the document means. `server_ms` is a wall-clock read at render
// time (operator_state.go serverClockMs), so comparing it byte-for-byte makes
// the equality of two calls a race: measured at 42 failures in 400 runs, which
// reddened the default gate whenever the two reads straddled a millisecond.
// The clock is not part of the contract under test; every other field is.
func snapshotWithoutClock(t *testing.T, body []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode operator document: %v (%q)", err, body)
	}
	delete(doc, "server_ms")
	canonical, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode operator document: %v", err)
	}
	return string(canonical)
}
