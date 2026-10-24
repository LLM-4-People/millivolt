package proxy

// Round 6 D2 regression: a bare trailing "#" parses to an empty Fragment, so
// the boundary's empty-fragment check passed and the structural re-parse
// dropped the marker. A raw fragment marker must be rejected; a percent-encoded
// %23 is path data and stays valid.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestBaseURLBareFragmentRejected(t *testing.T) {
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

	for _, base := range []string{"http://fixture.example#", "http://fixture.example/v1#"} {
		if w := post(base); w.Code != http.StatusBadRequest {
			t.Fatalf("bare-fragment base %q status = %d, want 400 (body %s)", base, w.Code, w.Body.String())
		}
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0 for rejected bases", up.calls.Load())
	}

	for _, base := range []string{"http://fixture.example/%23x", "http://fixture.example", "http://fixture.example/v1"} {
		if w := post(base); w.Code != http.StatusOK {
			t.Fatalf("base %q status = %d, want 200 (body %s)", base, w.Code, w.Body.String())
		}
	}
	if up.calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3 accepted bases", up.calls.Load())
	}
}
