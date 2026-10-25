package proxy

// Round 7 D2 regression: rejecting a malformed X-Proxy-Max-Concurrency value
// must stamp the request record through the shared client-error owner, not
// leave it at status 0 with no error type.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestMalformedMaxConcurrencyRecordsError(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	r.Header.Set("X-Proxy-Max-Concurrency", "not-a-number")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_request_error") ||
		!strings.Contains(w.Body.String(), hdrMaxConcurrency) {
		t.Fatalf("client error does not name the header: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.StatusCode != http.StatusBadRequest || rec.ErrorType != typeInvalidRequestError || !rec.IsError() {
		t.Fatalf("record = status %d type %q error %v, want 400/invalid_request_error",
			rec.StatusCode, rec.ErrorType, rec.IsError())
	}
	if !strings.Contains(rec.ErrorMsg, hdrMaxConcurrency) {
		t.Fatalf("record message = %q, want the header name", rec.ErrorMsg)
	}
}
