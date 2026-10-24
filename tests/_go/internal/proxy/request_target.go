package proxy

// Round 3 batch 6 regressions (D2): client path escaping survives to the
// upstream, and path/query header overrides cannot reinterpret a delimiter or
// smuggle an invalid request target.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func targetServer(t *testing.T) (*fixtureUpstream, *Server) {
	t.Helper()
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	return up, s
}

func TestClientEscapedPathPreserved(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		wantEscaped string
		wantQuery   string
	}{
		{name: "encoded slash", path: "/custom%2Fpart", wantEscaped: "/custom%2Fpart"},
		{name: "encoded question", path: "/custom%3Ftenant=1", wantEscaped: "/custom%3Ftenant=1"},
		{name: "encoded fragment", path: "/custom%23frag", wantEscaped: "/custom%23frag"},
		{name: "encoded percent", path: "/custom%25zz", wantEscaped: "/custom%25zz"},
		{name: "plain query", path: "/custom?tenant=1", wantEscaped: "/custom", wantQuery: "tenant=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, s := targetServer(t)
			w := proxyRequest(t, s, tc.path, "", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if got := up.lastEscapedPath(); got != tc.wantEscaped {
				t.Fatalf("upstream escaped path = %q, want %q", got, tc.wantEscaped)
			}
			if got := up.lastRawQuery(); got != tc.wantQuery {
				t.Fatalf("upstream query = %q, want %q", got, tc.wantQuery)
			}
		})
	}
}

func targetHeaderRequest(t *testing.T, s *Server, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	r.Header.Set(header, value)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestProxyPathHeaderRejectsQueryFragmentAndSpace(t *testing.T) {
	for _, value := range []string{"/a?b=1", "/a#frag", "/a b", "/a\tb", "/a%zz"} {
		up, s := targetServer(t)
		w := targetHeaderRequest(t, s, "X-Proxy-Path", value)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("path %q status = %d, want 400 (body %s)", value, w.Code, w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("path %q called upstream %d times", value, up.calls.Load())
		}
	}
}

func TestProxyPathHeaderAllowsEncodedPath(t *testing.T) {
	up, s := targetServer(t)
	w := targetHeaderRequest(t, s, "X-Proxy-Path", "/a%2Fb")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := up.lastEscapedPath(); got != "/a%2Fb" {
		t.Fatalf("upstream escaped path = %q, want /a%%2Fb", got)
	}
}

func TestProxyQueryHeaderRejectsFragmentAndSpace(t *testing.T) {
	for _, value := range []string{"a#b", "a b", "a\tb"} {
		up, s := targetServer(t)
		w := targetHeaderRequest(t, s, "X-Proxy-Query", value)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want 400 (body %s)", value, w.Code, w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("query %q called upstream %d times", value, up.calls.Load())
		}
	}
}

func TestProxyQueryHeaderKeepsQueryData(t *testing.T) {
	up, s := targetServer(t)
	w := targetHeaderRequest(t, s, "X-Proxy-Query", "a=1&b=2")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := up.lastRawQuery(); got != "a=1&b=2" {
		t.Fatalf("upstream query = %q, want a=1&b=2", got)
	}
}
