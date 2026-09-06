package proxy

// Provider-configured upstream headers (config providers.<label>.headers):
// applied at the upstream request boundary on LLM calls and model discovery,
// per-request {{uuid4}}/{{platform}} templates expand, client-forwarded
// headers lose, and the explicit X-Proxy-Headers injection map wins. The
// mechanism is label-blind: the test keys the map by whatever provider label
// the upstream URL derives to (an httptest IP host keeps host:port).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// upstreamLabel derives the provider label providerFromURL mints for the mock
// upstream's URL (an IP host keeps its host:port form).
func upstreamLabel(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
}

// headerCapture records the full header set of every upstream request.
type headerCapture struct {
	mu   sync.Mutex
	hdrs []http.Header
}

func (c *headerCapture) snapshot() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]http.Header, len(c.hdrs))
	copy(out, c.hdrs)
	return out
}

// captureUpstream serves one JSON body and records every request's headers.
func captureUpstream(t *testing.T, body string) (*httptest.Server, *headerCapture) {
	t.Helper()
	cap := &headerCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.hdrs = append(cap.hdrs, r.Header.Clone())
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	return srv, cap
}

func postChat(t *testing.T, srvURL, upstreamURL string, extra func(*http.Request)) {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("Content-Type", "application/json")
	if extra != nil {
		extra(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestProviderHeadersAppliedToUpstream(t *testing.T) {
	upstream, cap := captureUpstream(t, `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`)
	defer upstream.Close()
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		upstreamLabel(upstream.URL): {Headers: map[string]string{
			"User-Agent":   "my-shell/1.2.3 ({{platform}})",
			"X-Req-Id":     "{{uuid4}}",
			"X-Cli-Static": "v1",
		}},
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	for i := 0; i < 2; i++ {
		postChat(t, srv.URL, upstream.URL, nil)
	}
	hs := cap.snapshot()
	if len(hs) != 2 {
		t.Fatalf("captured %d upstream requests, want 2", len(hs))
	}
	for _, h := range hs {
		if got := h.Get("User-Agent"); !strings.HasPrefix(got, "my-shell/1.2.3 (") ||
			!strings.Contains(got, "; ") || strings.Contains(got, "{{") {
			t.Fatalf("User-Agent = %q, want templated my-shell form", got)
		}
		if got := h.Get("X-Cli-Static"); got != "v1" {
			t.Fatalf("X-Cli-Static = %q, want v1", got)
		}
	}
	if hs[0].Get("X-Req-Id") == hs[1].Get("X-Req-Id") {
		t.Fatalf("{{uuid4}} did not expand per request: both %q", hs[0].Get("X-Req-Id"))
	}
	for _, h := range hs {
		id := h.Get("X-Req-Id")
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Fatalf("X-Req-Id = %q, want a UUID v4", id)
		}
	}
}

func TestProviderHeadersClientInjectionWins(t *testing.T) {
	upstream, cap := captureUpstream(t, `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`)
	defer upstream.Close()
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		upstreamLabel(upstream.URL): {Headers: map[string]string{
			"User-Agent":   "my-shell/1.2.3",
			"X-Cli-Static": "v1",
		}},
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	postChat(t, srv.URL, upstream.URL, func(r *http.Request) {
		r.Header.Set("X-Proxy-Headers", `{"User-Agent":["client-wins"]}`)
	})
	h := cap.snapshot()[0]
	if got := h.Get("User-Agent"); got != "client-wins" {
		t.Fatalf("User-Agent = %q, want the client's explicit injection to win", got)
	}
	if got := h.Get("X-Cli-Static"); got != "v1" {
		t.Fatalf("X-Cli-Static = %q, want the provider default to persist", got)
	}
}

func TestProviderHeadersOnModelsUpstream(t *testing.T) {
	upstream, cap := captureUpstream(t, `{"data":[{"id":"m-1","object":"model"}]}`)
	defer upstream.Close()
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		upstreamLabel(upstream.URL): {Headers: map[string]string{
			"User-Agent": "my-shell/1.2.3 ({{platform}})",
		}},
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("models status = %d, want 200", resp.StatusCode)
	}
	if got := cap.snapshot()[0].Get("User-Agent"); !strings.HasPrefix(got, "my-shell/1.2.3 (") {
		t.Fatalf("models User-Agent = %q, want the provider header applied to discovery too", got)
	}
}

// Cursor (format=cursor) rides the SAME provider headers map for its identity
// headers - the shipped cursor_client_version knob is gone. The Connect
// protocol headers (Content-Type per path, Connect-Protocol-Version, te:
// trailers) are protocol constants: even a configured Content-Type must lose.
func TestCursorHeadersFromProviderConfig(t *testing.T) {
	h2s := &http2.Server{}
	// GetUsableModelsResponse with one ModelDetails (model_id=1, display_name=4).
	var respBody []byte
	respBody = append(respBody, cstr(1, "m-1")...)
	respBody = append(respBody, cstr(4, "Model One")...)
	respBody = append(respBody, cmsg(1, cstr(1, "m-1"))...)
	var sawRequestID string
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequestID = r.Header.Get("X-Request-Id")
		for name, want := range map[string]string{
			"X-Cursor-Client-Type":     "cli",
			"X-Ghost-Mode":             "true",
			"Connect-Protocol-Version": "1",
			"Te":                       "trailers",
			"Content-Type":             "application/proto", // protocol constant beats the configured override
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		id := r.Header.Get("X-Cursor-Client-Version")
		if id != "cli-test-1" {
			t.Errorf("X-Cursor-Client-Version = %q, want the configured value", id)
		}
		if len(sawRequestID) != 36 || strings.Count(sawRequestID, "-") != 4 {
			t.Errorf("X-Request-Id = %q, want a UUID v4", sawRequestID)
		}
		w.Header().Set("Content-Type", "application/proto")
		w.Write(respBody)
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		strings.TrimPrefix(upstream.URL, "http://"): {Headers: map[string]string{
			"X-Cursor-Client-Version": "cli-test-1",
			"X-Cursor-Client-Type":    "cli",
			"X-Ghost-Mode":            "true",
			"X-Request-Id":            "{{uuid4}}",
			"Content-Type":            "application/json", // must NOT reach the wire
		}},
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
