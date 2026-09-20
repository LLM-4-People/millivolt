package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The suppress_client_retries contract: while enabled, every error response
// on the LLM relay surface carries `x-should-retry: false` (the official
// OpenAI SDK convention, honored over the 408/409/429/5xx auto-retry defaults
// and over Retry-After); success responses stay untouched; disabled (the
// default) stays fully header-transparent, including a provider's own
// x-should-retry hint. One wrapper owns the stamp - the tests exercise it
// through the real surfaces (relayed upstream statuses, transport 502,
// validation 400, streaming and non-streaming success).

func retryHeaderOf(resp *http.Response) (string, bool) {
	v := resp.Header.Get("X-Should-Retry") // name lookup is case-insensitive
	if v == "" {
		return "", false
	}
	return v, true
}

func newSuppressServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	return httptest.NewServer(New(cfg, metrics.NewBuffer(10)))
}

// deadTransportUpstream accepts every connection and closes it without a
// response - a deterministic transport failure (a refused port can be
// firewall-dropped into a long hang on some hosts; this never is).
func deadTransportUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
}

// Enabled: the terminal status classes clients auto-retry (429, 5xx) carry
// the directive once the proxy's own ladder is exhausted, and it replaces a
// relayed provider hint to retry.
func TestSuppressClientRetriesStampsTerminalRelays(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A provider hint must not survive the operator's directive.
			w.Header().Set("x-should-retry", "true")
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"no","type":"api_error","code":"none"}}`))
		}))
		cfg := config.Default()
		cfg.MaxRetries = 0 // relay the provider's terminal status immediately
		cfg.SuppressClientRetries = true
		srv := newSuppressServer(t, cfg)
		resp := postChat(t, srv.URL, upstream.URL, nil)
		if resp.StatusCode != status {
			t.Errorf("status %d: relayed %d", status, resp.StatusCode)
		}
		if v, ok := retryHeaderOf(resp); !ok || v != "false" {
			t.Errorf("status %d: x-should-retry = %q (present=%v), want exactly \"false\"", status, v, ok)
		}
		srv.Close()
		upstream.Close()
	}
}

// Enabled: proxy-generated errors (transport 502, validation 400) carry the
// directive too; successes never do, on either surface (the event-stream
// upstream exercises the committed-200 streaming path).
func TestSuppressClientRetriesCoversProxyErrorsAndSkipsSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.SuppressClientRetries = true
	srv := newSuppressServer(t, cfg)
	defer srv.Close()

	// Transport failure: the upstream closes the connection without a
	// response.
	dead := deadTransportUpstream(t)
	defer dead.Close()
	resp := postChat(t, srv.URL, dead.URL, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("transport status = %d, want 502", resp.StatusCode)
	}
	if v, ok := retryHeaderOf(resp); !ok || v != "false" {
		t.Errorf("transport 502: x-should-retry = %q (present=%v), want \"false\"", v, ok)
	}

	// Validation error: a bad request header.
	vresp := postChat(t, srv.URL, dead.URL, func(r *http.Request) {
		r.Header.Set("X-Proxy-Max-Concurrency", "not-a-number")
	})
	if vresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("validation status = %d, want 400", vresp.StatusCode)
	}
	if v, ok := retryHeaderOf(vresp); !ok || v != "false" {
		t.Errorf("validation 400: x-should-retry = %q (present=%v), want \"false\"", v, ok)
	}

	// Non-streaming success stays untouched.
	jsonUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`))
	}))
	defer jsonUpstream.Close()
	plain := postChat(t, srv.URL, jsonUpstream.URL, nil)
	if plain.StatusCode != 200 {
		t.Fatalf("non-streaming success status = %d", plain.StatusCode)
	}
	if v, ok := retryHeaderOf(plain); ok {
		t.Errorf("non-streaming success carries x-should-retry = %q", v)
	}

	// Streaming success (an event-stream upstream commits the 200 and rides
	// the SSE pacer) stays untouched too.
	sseUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer sseUpstream.Close()
	streamResp := postChat(t, srv.URL, sseUpstream.URL, nil)
	if streamResp.StatusCode != 200 {
		t.Fatalf("streaming success status = %d", streamResp.StatusCode)
	}
	if v, ok := retryHeaderOf(streamResp); ok {
		t.Errorf("streaming success carries x-should-retry = %q", v)
	}
}

// Disabled (the default): the relay stays header-transparent - no directive
// is stamped anywhere, and a provider's own x-should-retry hint passes
// through verbatim exactly as it arrived.
func TestSuppressClientRetriesOffByDefaultStaysTransparent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-should-retry", "true")
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"no","type":"api_error","code":"none"}}`))
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.MaxRetries = 0
	srv := newSuppressServer(t, cfg)
	defer srv.Close()

	resp := postChat(t, srv.URL, upstream.URL, nil)
	if v, ok := retryHeaderOf(resp); !ok || v != "true" {
		t.Errorf("disabled relay must pass the provider hint verbatim: got %q (present=%v), want \"true\"", v, ok)
	}

	// And no directive appears on proxy-generated errors either.
	dead := deadTransportUpstream(t)
	defer dead.Close()
	tresp := postChat(t, srv.URL, dead.URL, nil)
	if _, ok := retryHeaderOf(tresp); ok {
		t.Error("disabled relay must not stamp x-should-retry on proxy errors")
	}
}

// The reload contract: the wrapper reads the live config snapshot at
// WriteHeader time, so a Reload flips the directive for the very next
// response without a restart.
func TestSuppressClientRetriesHotReloadFlipsNextResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"no","type":"api_error","code":"none"}}`))
	}))
	defer upstream.Close()
	// No retry ladder: the 500 relays immediately, so the test measures the
	// flip, not the group's exhausted-failure pacing. The tiny base keeps
	// that incidental follow-up first-send pace near-zero too; the
	// assertions are header-only.
	base := config.Default()
	base.MaxRetries = 0
	base.BaseBackoff = 5 * time.Millisecond
	s := New(base, metrics.NewBuffer(10))
	srv := httptest.NewServer(s)
	defer srv.Close()

	before := postChat(t, srv.URL, upstream.URL, nil)
	if _, ok := retryHeaderOf(before); ok {
		t.Fatal("default server stamped x-should-retry before the flip")
	}

	flipped := base.Clone()
	flipped.SuppressClientRetries = true
	s.Reload(flipped)
	after := postChat(t, srv.URL, upstream.URL, nil)
	if v, ok := retryHeaderOf(after); !ok || v != "false" {
		t.Errorf("after the flip: x-should-retry = %q (present=%v), want \"false\"", v, ok)
	}
}
