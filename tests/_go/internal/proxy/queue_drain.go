package proxy

// Regression tests for the operator queue-drain endpoint: it is POST-only,
// requires exactly one explicit scope, and rejects an ambiguous request
// instead of silently draining everything.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
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

// TestQueueDrainedResponseIsRetryable pins the wire and record contract of a
// released request: a retryable 503 with Retry-After, recorded as flow control
// (queue_drained), never as a provider or transport failure.
func TestQueueDrainedResponseIsRetryable(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{}
	w := httptest.NewRecorder()
	if !s.writeQueueDrainedError(w, rec, ir.Chat, scheduler.ErrQueueDrained) {
		t.Fatal("a drain release was not recognised")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("released request carried no Retry-After")
	}
	if rec.StatusCode != http.StatusServiceUnavailable || rec.ErrorType != "queue_drained" {
		t.Fatalf("record = %d/%q, want 503/queue_drained", rec.StatusCode, rec.ErrorType)
	}
	// Any other error stays with its own owner.
	if s.writeQueueDrainedError(httptest.NewRecorder(), rec, ir.Chat, context.Canceled) {
		t.Fatal("an unrelated error was answered as a drain release")
	}
	// The quality re-send classifier must not stamp the 502 transport trio.
	rec = &metrics.Record{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	gone, storm, drained := s.classifyResendFailure(r, rec, scheduler.ErrQueueDrained)
	if gone || storm || !drained {
		t.Fatalf("classifyResendFailure = gone %v, storm %v, drained %v", gone, storm, drained)
	}
	if rec.StatusCode != 0 || rec.ErrorType != "" {
		t.Fatalf("classifier stamped %d/%q before the sink ran", rec.StatusCode, rec.ErrorType)
	}
	if typ, msg, ok := queueDrainedStreamError(rec, scheduler.ErrQueueDrained); !ok || typ != "rate_limit_error" || msg == "" {
		t.Fatalf("in-band drain error = %q/%q/%v", typ, msg, ok)
	}
}
