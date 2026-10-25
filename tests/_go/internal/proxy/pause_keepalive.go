package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestPauseStreamSendsSSEKeepalive pins the OpenAI-style comment pacer: a
// held stream=true request commits text/event-stream and flushes
// ": keepalive" before any upstream byte, so clients can sit on the hold
// indefinitely.
func TestPauseStreamSendsSSEKeepalive(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\"}\n\n"))
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{{All: true}})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if hits.Load() != 0 {
		t.Fatal("paused stream reached upstream before keepalive")
	}
	buf := make([]byte, 64)
	nch := make(chan int, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		nch <- n
	}()
	var n int
	select {
	case n = <-nch:
	case <-time.After(time.Second):
		t.Fatal("no keepalive comment")
	}
	got := string(buf[:n])
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("body %q, want SSE comment : keepalive", got)
	}
}

// TestQueueStreamSendsSSEKeepalive pins the writer-level pacer on a
// concurrency-cap wait (no operator hold): the queued stream commits SSE
// and flushes ": keepalive" before it is allowed upstream.
func TestQueueStreamSendsSSEKeepalive(t *testing.T) {
	var hits atomic.Int32
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			<-hold
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\"}\n\n"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	newReq := func(stream bool) *http.Request {
		body := `{"model":"m"}`
		if stream {
			body = `{"model":"m","stream":true}`
		}
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		return req
	}

	firstDone := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(newReq(false))
		if err != nil {
			t.Error(err)
			close(firstDone)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		close(firstDone)
	}()
	if !waitFor(t, func() bool { return hits.Load() != 0 }) {
		t.Fatalf("first request never reached upstream (hits=%d)", hits.Load())
	}

	resp, err := http.DefaultClient.Do(newReq(true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("queued stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if hits.Load() != 1 {
		t.Fatal("queued stream reached upstream before keepalive")
	}
	buf := make([]byte, 64)
	nch := make(chan int, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		nch <- n
	}()
	var n int
	select {
	case n = <-nch:
	case <-time.After(time.Second):
		t.Fatal("no keepalive comment on queued stream")
	}
	got := string(buf[:n])
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("body %q, want SSE comment : keepalive", got)
	}
	close(hold)
	<-firstDone
}

// TestStreamKeepaliveDuringIdle pins the same writer pacer mid-stream: a
// quiet gap after the first upstream chunk must emit ": keepalive" even
// though the request is not paused or queued.
func TestStreamKeepaliveDuringIdle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(70 * time.Millisecond)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("body %q, want SSE comment : keepalive between chunks", got)
	}
	if !strings.Contains(got, `"content":"a"`) || !strings.Contains(got, `"content":"b"`) {
		t.Fatalf("body %q, want both upstream chunks", got)
	}
}

// TestQueueWaitExceededAfterKeepaliveEmitsSSEError: OnWait commits 200 SSE,
// then max_queue_wait fires. The client must get an in-band error + [DONE],
// not a silent hang-up.
func TestQueueWaitExceededAfterKeepaliveEmitsSSEError(t *testing.T) {
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\"}\n\n"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	cfg.MaxQueueWait = 80 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	firstDone := make(chan struct{})
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		close(firstDone)
	}()
	// The first request holds the only concurrency slot; the second must queue
	// and observe the queue-wait expiry.
	if !waitFor(t, func() bool { return p.scheduler.Stats().InFlight >= 1 }) {
		t.Fatal("first request never occupied the concurrency slot")
	}

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (already committed SSE)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"type":"rate_limit_error"`) {
		t.Fatalf("body %q, want in-band rate_limit_error", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("body %q, want [DONE]", got)
	}
	close(hold)
	<-firstDone
}

// TestSlowUpstreamJSONErrorKeepsStatus: the pacer does not commit SSE before
// upstream headers because the body may be JSON. A slow 429 therefore keeps
// its real status and content type instead of becoming an in-band error.
func TestSlowUpstreamJSONErrorKeepsStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.SSEKeepaliveInterval = 15 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, ": keepalive") {
		t.Fatalf("JSON error body carried SSE keepalives: %q", got)
	}
	if !strings.Contains(got, "rate_limit_error") {
		t.Fatalf("body %q, want the provider error JSON", got)
	}
}
