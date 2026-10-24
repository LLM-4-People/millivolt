package proxy

// Round 5 D1 regression: Accept-Encoding and Range are transport-mode headers
// that the proxy must own. A client- or config-supplied value would disable the
// transport's gzip negotiation/decompression, and copyResponseHeaders strips
// Content-Encoding, so the client would receive gzip bytes labelled JSON. The
// client-controlled injection path fails closed; every configured source is
// stripped from the final upstream request.

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type captureRoundTripper struct {
	base http.RoundTripper
	hdr  http.Header
}

func (c *captureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.hdr = r.Header.Clone()
	return c.base.RoundTrip(r)
}

func TestXProxyHeadersRejectsTransportModeHeaders(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{}`)
	cfg := config.Default()
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	for _, header := range []string{"Accept-Encoding", "Range"} {
		t.Run(header, func(t *testing.T) {
			injected := `{"` + header + `":["gzip"]}`
			if header == "Range" {
				injected = `{"Range":["bytes=0-1"]}`
			}
			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("Authorization", "Bearer sk-test")
			r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
			r.Header.Set("X-Proxy-Headers", injected)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s injection status = %d, want 400 (body %s)", header, w.Code, w.Body.String())
			}
			if up.calls.Load() != 0 {
				t.Fatalf("%s injection reached the upstream %d times", header, up.calls.Load())
			}
		})
	}
}

func TestConfiguredTransportModeHeadersAreStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(chatCompletionBody))
			_ = gz.Close()
			return
		}
		_, _ = w.Write([]byte(chatCompletionBody))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		upstreamLabel(upstream.URL): {Headers: map[string]string{
			"Accept-Encoding": "gzip",
			"Range":           "bytes=0-1",
			"X-Keep":          "yes",
		}},
	}
	s := New(cfg, metrics.Noop{})
	capture := &captureRoundTripper{base: rewriteTransport{target: upstream.URL}}
	s.client.Store(&http.Client{Transport: capture})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := capture.hdr.Get("Accept-Encoding"); got != "" {
		t.Fatalf("configured Accept-Encoding reached the transport as %q, want the transport's own negotiation", got)
	}
	if got := capture.hdr.Get("Range"); got != "" {
		t.Fatalf("configured Range reached the transport as %q, want it stripped", got)
	}
	if got := capture.hdr.Get("X-Keep"); got != "yes" {
		t.Fatalf("ordinary configured header = %q, want yes", got)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatalf("client body is not readable JSON: %x", w.Body.Bytes())
	}
	if !strings.Contains(w.Body.String(), `"content":"hi"`) {
		t.Fatalf("client body lost the completion: %s", w.Body.String())
	}
}

func TestRequestOverrideTransportHeadersStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.RequestOverrides = []config.RequestOverride{{
		Headers: map[string]string{
			"Accept-Encoding": "br",
			"Range":           "bytes=0-9",
			"X-Override":      "yes",
		},
	}}
	s := New(cfg, metrics.Noop{})
	capture := &captureRoundTripper{base: rewriteTransport{target: upstream.URL}}
	s.client.Store(&http.Client{Transport: capture})

	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", upstream.URL)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := capture.hdr.Get("Accept-Encoding"); got != "" {
		t.Fatalf("override Accept-Encoding reached the transport as %q", got)
	}
	if got := capture.hdr.Get("Range"); got != "" {
		t.Fatalf("override Range reached the transport as %q", got)
	}
	if got := capture.hdr.Get("X-Override"); got != "yes" {
		t.Fatalf("ordinary override header = %q, want yes", got)
	}
}

func TestModelDiscoveryStripsTransportHeaders(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, `{"data":[{"id":"m"}]}`)
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		"fixture.example": {Headers: map[string]string{
			"Accept-Encoding": "gzip",
			"Range":           "bytes=0-1",
			"X-Keep":          "yes",
		}},
	}
	s := New(cfg, metrics.Noop{})
	capture := &captureRoundTripper{base: rewriteTransport{target: up.server.URL}}
	s.client.Store(&http.Client{Transport: capture})

	r := httptest.NewRequest(http.MethodGet, "http://proxy.example/v1/models", nil)
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := capture.hdr.Get("Accept-Encoding"); got != "" {
		t.Fatalf("discovery Accept-Encoding reached the transport as %q", got)
	}
	if got := capture.hdr.Get("Range"); got != "" {
		t.Fatalf("discovery Range reached the transport as %q", got)
	}
	if got := capture.hdr.Get("X-Keep"); got != "yes" {
		t.Fatalf("ordinary discovery header = %q, want yes", got)
	}
}

func TestCursorIdentityStripsTransportHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		"fixture.example": {Headers: map[string]string{
			"Accept-Encoding": "gzip",
			"Range":           "bytes=0-1",
			"X-Keep":          "yes",
		}},
	}
	s := New(cfg, metrics.Noop{})
	req := httptest.NewRequest(http.MethodPost, "http://cursor.example/agent.v1.Run", nil)
	target := &target{provider: "fixture.example", authHeader: "Authorization", authPrefix: "Bearer "}
	s.setCursorIdentity(req, target, "secret")
	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("cursor Accept-Encoding = %q, want stripped", got)
	}
	if got := req.Header.Get("Range"); got != "" {
		t.Fatalf("cursor Range = %q, want stripped", got)
	}
	if got := req.Header.Get("X-Keep"); got != "yes" {
		t.Fatalf("ordinary cursor header = %q, want yes", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("cursor auth = %q, want the resolved credential", got)
	}
	if got := req.Header.Get("Connect-Protocol-Version"); got != "1" {
		t.Fatalf("cursor protocol header = %q, want 1", got)
	}
}

// Round 6 H5 coverage guard: the X-Proxy-Auth-Header call site uses the same
// transport-mode rejection as the X-Proxy-Headers map; removing that check
// would otherwise leave every test green.
func TestAuthHeaderRejectsTransportModeHeaders(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, chatCompletionBody)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	post := func(name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
		r.Header.Set("X-Proxy-Auth-Header", name)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	for _, name := range []string{"Accept-Encoding", "Range"} {
		if w := post(name); w.Code != http.StatusBadRequest {
			t.Fatalf("X-Proxy-Auth-Header %q status = %d, want 400 (body %s)", name, w.Code, w.Body.String())
		}
	}
	if up.calls.Load() != 0 {
		t.Fatalf("transport-mode auth header reached the upstream %d times", up.calls.Load())
	}

	var seen string
	normal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Custom-Auth")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatCompletionBody)
	}))
	defer normal.Close()
	s.client.Store(&http.Client{Transport: rewriteTransport{target: normal.URL}})
	if w := post("X-Custom-Auth"); w.Code != http.StatusOK {
		t.Fatalf("normal auth header status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if seen != "Bearer sk-test" {
		t.Fatalf("upstream X-Custom-Auth = %q, want the client credential", seen)
	}
}
