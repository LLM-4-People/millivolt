package proxy

// The client_error_class disclosure and the operator vocabulary extensions.
// The header is computed at the relay's ONE outer writer from the same
// classifier the retry ladder and the health predicates use, so a rate limit
// announced by a 400 is labeled rate_limit (and marked retryable for the SDKs)
// rather than filed as a client fault, and a durable quota block is labeled
// quota (and not retryable) rather than left looking transient. Off (the
// default) leaves responses exactly as the provider sent them, and
// suppress_client_retries still wins over the class advice.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// clientClassCase is one upstream error shape and the class headers it must
// produce when client_error_class=header.
type clientClassCase struct {
	name      string
	status    int
	body      string
	wantClass string
	wantRetry string
}

var clientClassCases = []clientClassCase{
	{
		// The reported incident: a membership RPM cap announced by a 400 whose
		// only machine-readable member is the code.
		name:      "400 carrying a rate-limit code",
		status:    http.StatusBadRequest,
		body:      `{"error":{"code":"tier_limit_exceeded","message":"membership rpm limit exceeded: 101/100 resets 2026-11-03T23:11:00Z"}}`,
		wantClass: string(metrics.ClassRateLimit),
		wantRetry: "true",
	},
	{
		name:      "429 rate limit",
		status:    http.StatusTooManyRequests,
		body:      `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
		wantClass: string(metrics.ClassRateLimit),
		wantRetry: "true",
	},
	{
		name:      "400 genuine client fault",
		status:    http.StatusBadRequest,
		body:      `{"error":{"type":"invalid_request_error","message":"bad field"}}`,
		wantClass: string(metrics.ClassClientError),
		wantRetry: "false",
	},
	{
		name:      "500 upstream failure",
		status:    http.StatusInternalServerError,
		body:      `{"error":{"type":"server_error","message":"boom"}}`,
		wantClass: string(metrics.ClassUnavailable),
		wantRetry: "true",
	},
	{
		name:      "429 durable quota block",
		status:    http.StatusTooManyRequests,
		body:      `{"error":{"type":"insufficient_quota","message":"check your plan and billing details"}}`,
		wantClass: string(metrics.ClassQuota),
		wantRetry: "false",
	},
}

// serveClientClassCase drives one non-streaming relay request to a fixture
// upstream returning the case's error and returns the proxy response.
func serveClientClassCase(t *testing.T, tc clientClassCase, mutate func(*config.Config)) *httptest.ResponseRecorder {
	t.Helper()
	up := newFixtureUpstream(t, "application/json", tc.status, tc.body)
	cfg := config.Default()
	// No transparent retries: this test observes the surfaced error, not the
	// ladder (the ladder's classification is pinned elsewhere).
	cfg.MaxRetries = 0
	if mutate != nil {
		mutate(cfg)
	}
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	return proxyRequest(t, s, "/v1/chat/completions", "",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
}

func TestClientErrorClassHeader(t *testing.T) {
	for _, tc := range clientClassCases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveClientClassCase(t, tc, func(c *config.Config) {
				c.ClientErrorClass = config.ClientErrorClassHeader
			})
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.status, w.Body.String())
			}
			if got := w.Header().Get("X-Proxy-Error-Class"); got != tc.wantClass {
				t.Errorf("X-Proxy-Error-Class = %q, want %q", got, tc.wantClass)
			}
			if got := w.Header().Get("X-Should-Retry"); got != tc.wantRetry {
				t.Errorf("X-Should-Retry = %q, want %q", got, tc.wantRetry)
			}
			// The body stays verbatim: the disclosure adds headers, never a
			// rewritten envelope.
			if got := w.Body.String(); got != tc.body {
				t.Errorf("body = %q, want the provider's verbatim %q", got, tc.body)
			}
		})
	}
}

// TestClientErrorClassOffLeavesResponsesUntouched pins the default: no class
// headers, body verbatim.
func TestClientErrorClassOffLeavesResponsesUntouched(t *testing.T) {
	for _, tc := range clientClassCases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveClientClassCase(t, tc, nil)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if got := w.Header().Get("X-Proxy-Error-Class"); got != "" {
				t.Errorf("X-Proxy-Error-Class = %q, want none with client_error_class=off", got)
			}
			if got := w.Header().Get("X-Should-Retry"); got != "" {
				t.Errorf("X-Should-Retry = %q, want none with client_error_class=off", got)
			}
			if got := w.Body.String(); got != tc.body {
				t.Errorf("body = %q, want %q", got, tc.body)
			}
		})
	}
}

// TestSuppressClientRetriesWinsOverTheClassAdvice pins the precedence between
// the two retry headers' owners: while suppression is on, x-should-retry stays
// false on every error even when the class would advise a retry; the class
// header still identifies the failure.
func TestSuppressClientRetriesWinsOverTheClassAdvice(t *testing.T) {
	tc := clientClassCases[0] // the 400 rate limit, whose class says retry
	w := serveClientClassCase(t, tc, func(c *config.Config) {
		c.ClientErrorClass = config.ClientErrorClassHeader
		c.SuppressClientRetries = true
	})
	if got := w.Header().Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry = %q, want false (suppress_client_retries wins)", got)
	}
	if got := w.Header().Get("X-Proxy-Error-Class"); got != tc.wantClass {
		t.Fatalf("X-Proxy-Error-Class = %q, want %q", got, tc.wantClass)
	}
}

// TestOperatorRateLimitExtensionDrivesRetryAndClass pins the extension end to
// end: a spelling the operator listed under rate_limit_error_classes is
// retried transparently (the upstream sees a second call), counted as a rate
// limit rather than an error, and disclosed as rate_limit when it finally
// surfaces.
func TestOperatorRateLimitExtensionDrivesRetryAndClass(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusBadRequest,
		`{"error":{"type":"gateway_throttle","message":"slow down at the gateway"}}`)
	cfg := config.Default()
	cfg.MaxRetries = 1
	cfg.RateLimitErrorClasses = []string{"gateway_throttle"}
	cfg.ClientErrorClass = config.ClientErrorClassHeader
	t.Cleanup(func() { metrics.InstallClassExtensions(metrics.ClassExtensions{}) })
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if got := up.calls.Load(); got < 2 {
		t.Fatalf("upstream calls = %d, want the extension spelling retried at least once", got)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the surfaced 400 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Proxy-Error-Class"); got != string(metrics.ClassRateLimit) {
		t.Fatalf("X-Proxy-Error-Class = %q, want %q", got, metrics.ClassRateLimit)
	}
	if got := w.Header().Get("X-Should-Retry"); got != "true" {
		t.Fatalf("X-Should-Retry = %q, want true", got)
	}
}
