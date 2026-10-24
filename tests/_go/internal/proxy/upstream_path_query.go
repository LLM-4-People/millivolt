package proxy

// Round 4 batch 12 regressions (D1, D2): a literal # in the client query is
// not reinterpreted as a fragment, and empty path segments keep their shape.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const chatCompletionBody = `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestClientQueryLiteralHashPreserved(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "literal hash", raw: "tenant=a#b", want: "tenant=a#b"},
		{name: "encoded hash", raw: "tenant=a%23b", want: "tenant=a%23b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, chatCompletionBody)
			s := New(config.Default(), metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("Authorization", "Bearer sk-test")
			r.Header.Set("X-Proxy-Base-URL", up.server.URL)
			r.URL.RawQuery = tc.raw
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
			}
			if got := up.lastRawQuery(); got != tc.want {
				t.Fatalf("upstream raw query = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientEmptyPathSegmentsPreserved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		header string
		want   string
	}{
		{name: "double leading", path: "//custom", want: "//custom"},
		{name: "double trailing", path: "/custom//", want: "/custom//"},
		{name: "inner double", path: "/a//b", want: "/a//b"},
		{name: "header double leading", path: "/v1/chat/completions", header: "//custom", want: "//custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, chatCompletionBody)
			s := New(config.Default(), metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			r := httptest.NewRequest(http.MethodPost, "http://proxy.example"+tc.path,
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("Authorization", "Bearer sk-test")
			r.Header.Set("X-Proxy-Base-URL", up.server.URL)
			if tc.header != "" {
				r.Header.Set("X-Proxy-Path", tc.header)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
			}
			if got := up.lastEscapedPath(); got != tc.want {
				t.Fatalf("upstream escaped path = %q, want %q", got, tc.want)
			}
			if got := up.lastPath(); got != tc.want {
				t.Fatalf("upstream path = %q, want %q", got, tc.want)
			}
		})
	}
}
