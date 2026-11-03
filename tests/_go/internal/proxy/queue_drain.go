package proxy

// Regression tests for the operator queue-drain endpoint: it is POST-only,
// requires exactly one explicit scope, and rejects an ambiguous request
// instead of silently draining everything.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestQueueDrainEndpointValidation(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	assertFlatErrorDecodes(t, s.HandleQueueDrain, `{}`, http.StatusBadRequest)
	assertFlatErrorDecodes(t, s.HandleQueueDrain, `{"all":true,"provider":"p"}`, http.StatusBadRequest)
	assertFlatErrorDecodes(t, s.HandleQueueDrain, `{"provider":"p","model":"m"}`, http.StatusBadRequest)

	w := httptest.NewRecorder()
	s.HandleQueueDrain(w, httptest.NewRequest(http.MethodGet, "/admin/queue/drain", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}

	for _, body := range []string{`{"all":true}`, `{"provider":"p"}`, `{"model":"m"}`, `{"client":"c"}`} {
		w = httptest.NewRecorder()
		s.HandleQueueDrain(w, httptest.NewRequest(http.MethodPost, "/admin/queue/drain", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body %s)", body, w.Code, w.Body.String())
		}
	}
}
