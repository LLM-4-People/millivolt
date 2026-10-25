package proxy

// Round 6 D1 regression: "http://:PORT" parses with a non-empty Host but an
// empty Hostname, and Go's transport dials the local host for it. A hostless
// request base must be rejected at the routing boundary, and a hostless
// allowed_base_urls entry must fail at config load instead of loading as a dead
// rule.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestHostlessBaseURLRejected(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, chatCompletionBody)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	post := func(base string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", base)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	for _, base := range []string{"http://:8080", "https://:443/v1", "http://:/path"} {
		if w := post(base); w.Code != http.StatusBadRequest {
			t.Fatalf("hostless base %q status = %d, want 400 (body %s)", base, w.Code, w.Body.String())
		}
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0 for hostless bases", up.calls.Load())
	}

	// A hostless base must not be admitted by an allowlist either.
	allowCfg := config.Default()
	allowCfg.AllowedBaseURLs = []string{"http://fixture.example"}
	allowed := New(allowCfg, metrics.Noop{})
	allowed.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://:8080")
	w := httptest.NewRecorder()
	allowed.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("hostless base with allowlist status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}

	for _, base := range []string{"http://fixture.example", "http://127.0.0.1:9", "https://fixture.example:8443/v1"} {
		if w := post(base); w.Code != http.StatusOK {
			t.Fatalf("normal base %q status = %d, want 200 (body %s)", base, w.Code, w.Body.String())
		}
	}
	if up.calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3 for accepted bases", up.calls.Load())
	}
}
