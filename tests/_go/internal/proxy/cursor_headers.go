package proxy

// The cursor bidi path used to hand-copy a subset of the upstream header
// capture, losing the X-Openai-Request-Id / X-Api-Request-Id aliases, the
// Server header, the processing-time and rate-limit fields. It now shares
// captureUpstreamHeaders with the generic relay; this pins that the fields
// the old subset missed land on the record when the upstream sends them.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestCursorPathCapturesUpstreamHeaderMetadata(t *testing.T) {
	cfg := stormTestConfig()
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		readFrame(t, r.Body)
		resp := stormCursorReply(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"fixture"}}`)
		// X-Api-Request-Id is an alias only the shared owner reads; the old
		// cursor subset looked at X-Request-Id and Request-Id only.
		resp.Header.Set("X-Api-Request-Id", "cursor-req-42")
		resp.Header.Set("Server", "fixture-server")
		resp.Header.Set("X-Openai-Processing-Ms", "42")
		return resp, nil
	})}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"neutral-model","messages":[{"role":"user","content":"fixture"}]}`))
	r.Header.Set(hdrBaseURL, "http://neutral.invalid")
	r.Header.Set(hdrFormat, "cursor")
	s.ServeHTTP(httptest.NewRecorder(), r)
	rec := waitForRecord(t, buf, 1)[0]
	if rec.ProviderRequestID != "cursor-req-42" {
		t.Errorf("ProviderRequestID = %q, want cursor-req-42 via the X-Api-Request-Id alias", rec.ProviderRequestID)
	}
	if rec.ProviderServer != "fixture-server" {
		t.Errorf("ProviderServer = %q, want fixture-server", rec.ProviderServer)
	}
	if rec.ProcessingMs != 42 {
		t.Errorf("ProcessingMs = %d, want 42", rec.ProcessingMs)
	}
	if rec.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", rec.StatusCode)
	}
}
