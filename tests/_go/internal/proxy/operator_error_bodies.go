package proxy

// Handler-level pins for the flat-error writer on the pause/throttle
// mutation endpoints: an error cause carrying a double quote (the strict
// decoder's duplicate-key message embeds the offending key) must arrive as a
// body that decodes back through a real JSON parse. The adminjson-level
// WriteError test is the primary pin for the raw-interpolation bug class -
// the raw call sites these handlers used to carry (pause's edit-failure 400,
// throttle's update-failure 400) only ever emit static messages, so no
// natural request can put a metacharacter through them; these rows instead
// drive the decoder-cause sites that DO echo attacker-controlled text and
// guard them against any regression back to hand-built JSON.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func assertFlatErrorDecodes(t *testing.T, h http.HandlerFunc, body string, wantStatus int) {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/admin/fixture", strings.NewReader(body)))
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", w.Code, wantStatus, w.Body.String())
	}
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("error body is not valid JSON: %v (%q)", err, w.Body.String())
	}
	if decoded.Error == "" {
		t.Fatalf("error body carried no message: %q", w.Body.String())
	}
}

func TestOperatorMutationErrorBodiesDecode(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	// The strict decoder's duplicate-key rejection embeds the offending key
	// in the message: a quoted key reaches the body through the error cause.
	assertFlatErrorDecodes(t, s.HandlePause, `{"paused":true,"Paused":true}`, http.StatusBadRequest)
	assertFlatErrorDecodes(t, s.HandleThrottle, `{"provider":"p","Provider":"q"}`, http.StatusBadRequest)
	// The static-message failure paths (the former raw-interpolation sites)
	// keep emitting valid JSON bodies.
	assertFlatErrorDecodes(t, s.HandlePause, `{"paused":true,"duration":"30s"}`, http.StatusBadRequest)
	assertFlatErrorDecodes(t, s.HandleThrottle, `{"provider":"p","concurrency":-1}`, http.StatusBadRequest)
}
