package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// sendAndWait drives one request through the proxy and returns the n-th
// finalized record (records land in submission order; the body is drained so
// ServeHTTP's deferred upstream-body Close returns the connection to the
// transport pool before the record is observable).
func sendAndWait(t *testing.T, buf *metrics.Buffer, srv *httptest.Server, build func() *http.Request, n int) *metrics.Record {
	t.Helper()
	resp, err := http.DefaultClient.Do(build())
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	recs := waitForRecord(t, buf, n)
	if len(recs) != n {
		t.Fatalf("recorded %d records, want %d", len(recs), n)
	}
	return recs[n-1]
}

// TestUpstreamTimingFreshDialThenPooled pins the decomposition's pooling facts
// across two requests through one proxy server to one httptest upstream: the
// shared stdlib transport pools the test-server connection by itself (no
// explicit transport needed), so the first request's adopted attempt dialed
// fresh (connect observed, not reused) while the immediate second request's
// adopted attempt rode the pooled connection (reused, no dial - 0 means the
// hook never fired). The plaintext httptest upstream can never run the TLS
// handshake pair: no TLS fixture exists in the tree, so the TLS hook path is
// exercised by construction only and upstream_tls_ms is asserted 0 here.
func TestUpstreamTimingFreshDialThenPooled(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	build := func() *http.Request {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-k")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		return req
	}

	first := sendAndWait(t, buf, srv, build, 1)
	if first.UpstreamConnectMs <= 0 {
		t.Errorf("first request UpstreamConnectMs = %d, want > 0 (fresh dial to the test upstream)", first.UpstreamConnectMs)
	}
	if first.UpstreamConnReused {
		t.Error("first request UpstreamConnReused = true, want false (nothing was pooled yet)")
	}
	if first.UpstreamTTFBMs <= 0 {
		t.Errorf("first request UpstreamTTFBMs = %d, want > 0 (the attempt waited for response headers)", first.UpstreamTTFBMs)
	}
	if first.UpstreamTLSMs != 0 {
		t.Errorf("first request UpstreamTLSMs = %d, want 0 (plaintext upstream: the TLS hook never fired)", first.UpstreamTLSMs)
	}

	second := sendAndWait(t, buf, srv, build, 2)
	if !second.UpstreamConnReused {
		t.Error("second request UpstreamConnReused = false, want true (the transport pools the test-server connection)")
	}
	if second.UpstreamConnectMs != 0 {
		t.Errorf("second request UpstreamConnectMs = %d, want 0 (no dial happened: 0 = not observed)", second.UpstreamConnectMs)
	}
	if second.UpstreamTTFBMs <= 0 {
		t.Errorf("second request UpstreamTTFBMs = %d, want > 0 (the adopted attempt waited for headers too)", second.UpstreamTTFBMs)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}

// TestCursorUpstreamTimingFreshDialThenPooled is the h2 twin: the cursor
// transport is x/net/http2 with a custom plain dialer (a net.Dialer), which
// still fires the nettrace connect pair and the h2 GotConn/GotFirstResponseByte
// hooks. quality_retries=0 makes every void turn surface empty_turn after its
// SINGLE send, so each request's adopted attempt is exactly its one open, and
// the second request must ride the pooled h2 connection to the same authority.
func TestCursorUpstreamTimingFreshDialThenPooled(t *testing.T) {
	var calls atomic.Int32
	upstream := cursorVoidUpstream(t, &calls)

	buf := metrics.NewBuffer(100)
	cfg := cursorTestCfg(upstream.URL)
	cfg.QualityRetries = 0 // the single send is the adopted attempt
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	first := sendAndWait(t, buf, srv, func() *http.Request { return cursorVoidRequest(srv, upstream) }, 1)
	if first.UpstreamConnectMs <= 0 {
		t.Errorf("first cursor request UpstreamConnectMs = %d, want > 0 (fresh h2c dial through the custom net.Dialer)", first.UpstreamConnectMs)
	}
	if first.UpstreamConnReused {
		t.Error("first cursor request UpstreamConnReused = true, want false (fresh dial)")
	}
	if first.UpstreamTTFBMs <= 0 {
		t.Errorf("first cursor request UpstreamTTFBMs = %d, want > 0 (the run's response headers were awaited)", first.UpstreamTTFBMs)
	}

	second := sendAndWait(t, buf, srv, func() *http.Request { return cursorVoidRequest(srv, upstream) }, 2)
	if !second.UpstreamConnReused {
		t.Error("second cursor request UpstreamConnReused = false, want true (the x/net/http2 pool served the same authority)")
	}
	if second.UpstreamConnectMs != 0 {
		t.Errorf("second cursor request UpstreamConnectMs = %d, want 0 (pooled connection: no dial)", second.UpstreamConnectMs)
	}
	if second.UpstreamTTFBMs <= 0 {
		t.Errorf("second cursor request UpstreamTTFBMs = %d, want > 0", second.UpstreamTTFBMs)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream exchanges = %d, want 2", calls.Load())
	}
}
