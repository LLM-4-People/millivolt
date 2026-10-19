package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
)

// deadlineWriter records the delivery deadlines the handler layer arms.
type deadlineWriter struct {
	header        http.Header
	body          []byte
	readDeadline  time.Time
	writeDeadline time.Time
}

func (w *deadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *deadlineWriter) WriteHeader(int) {}
func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}
func (w *deadlineWriter) SetReadDeadline(t time.Time) error  { w.readDeadline = t; return nil }
func (w *deadlineWriter) SetWriteDeadline(t time.Time) error { w.writeDeadline = t; return nil }
func (w *deadlineWriter) Flush()                             {}

// TestDeadlineHandlerArmsBodyAndWriteBounds pins the one delivery choke point:
// a body-bearing request gets a read deadline, and every write re-arms a
// progress deadline so a stalled peer cannot pin the handler forever.
func TestDeadlineHandlerArmsBodyAndWriteBounds(t *testing.T) {
	w := &deadlineWriter{}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("x"))
	h := deadlineHandler{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
	})}
	before := time.Now()
	h.ServeHTTP(w, r)
	if string(w.body) != "ok" {
		t.Fatalf("body = %q", w.body)
	}
	if w.writeDeadline.Before(before) || time.Until(w.writeDeadline) <= 0 {
		t.Fatalf("write deadline not armed: %v", w.writeDeadline)
	}
	if !w.readDeadline.After(before) {
		t.Fatalf("body read deadline not armed: %v", w.readDeadline)
	}
	// A GET with no body must not arm a read deadline for the (absent) body.
	w2 := &deadlineWriter{}
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !w2.readDeadline.IsZero() {
		t.Fatalf("body-less request armed read deadline %v", w2.readDeadline)
	}
}

type hijackWriter struct {
	deadlineWriter
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (w *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.rw, nil }

// TestDeadlineWriterKeepsOptionalInterfaces pins that wrapping the outermost
// writer does not break the Flusher/Hijacker assertions streaming and the
// restart watch rely on.
// TestDeadlineWriterPreservesShorterExplicitDeadline pins the interaction
// between the delivery choke point and streaming owners: a one-minute SSE
// progress bound set through ResponseController must survive the next Write,
// while an ordinary write still gets the ten-minute default.
func TestDeadlineWriterPreservesShorterExplicitDeadline(t *testing.T) {
	inner := &deadlineWriter{}
	w := &deadlineResponseWriter{ResponseWriter: inner}
	explicit := time.Now().Add(time.Minute)
	if err := http.NewResponseController(w).SetWriteDeadline(explicit); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("event")); err != nil {
		t.Fatal(err)
	}
	if !inner.writeDeadline.Equal(explicit) {
		t.Fatalf("write deadline = %v, want the explicit %v", inner.writeDeadline, explicit)
	}

	plain := &deadlineWriter{}
	(&deadlineResponseWriter{ResponseWriter: plain}).Write([]byte("body"))
	if remaining := time.Until(plain.writeDeadline); remaining < 9*time.Minute {
		t.Fatalf("ordinary write deadline = %v, want the ten-minute default", remaining)
	}
}

func TestDeadlineWriterKeepsOptionalInterfaces(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	inner := &hijackWriter{conn: server, rw: bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server))}
	w := &deadlineResponseWriter{ResponseWriter: inner}
	if _, ok := any(w).(http.Flusher); !ok {
		t.Fatal("deadline writer hides http.Flusher")
	}
	if _, ok := any(w).(http.Hijacker); !ok {
		t.Fatal("deadline writer hides http.Hijacker")
	}
	if _, ok := w.Unwrap().(http.ResponseWriter); !ok {
		t.Fatal("deadline writer breaks the ResponseController unwrap chain")
	}
	w.Flush()
	conn, _, err := w.Hijack()
	if err != nil || conn != server {
		t.Fatalf("hijack = %v, %v", conn, err)
	}
}

// TestProgressBodyRearmsIdleAndAbsoluteBounds pins the request-body progress
// guard itself: every read arms the idle bound, capped by the absolute body
// deadline captured when the handler started.
func TestProgressBodyRearmsIdleAndAbsoluteBounds(t *testing.T) {
	w := &deadlineWriter{}
	body := &progressReadCloser{
		body:            io.NopCloser(strings.NewReader("abc")),
		setReadDeadline: http.NewResponseController(w).SetReadDeadline,
		idle:            30 * time.Second,
		absolute:        time.Now().Add(time.Hour),
	}
	before := time.Now()
	if _, err := body.Read(make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if got := w.readDeadline.Sub(before); got < 25*time.Second || got > 35*time.Second {
		t.Fatalf("idle read deadline = %v, want about 30s", got)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	capped := &deadlineWriter{}
	absolute := time.Now().Add(50 * time.Millisecond)
	body = &progressReadCloser{
		body:            io.NopCloser(strings.NewReader("abc")),
		setReadDeadline: http.NewResponseController(capped).SetReadDeadline,
		idle:            time.Hour,
		absolute:        absolute,
	}
	if _, err := body.Read(make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if !capped.readDeadline.Equal(absolute) {
		t.Fatalf("read deadline = %v, want the absolute bound %v", capped.readDeadline, absolute)
	}
}

// TestDeadlineHandlerStalledBodyFailsWithinIdleBound reproduces the admission
// H6 case: a declared body that never sends bytes must fail within the idle
// bound, not the ten-minute absolute bound, and release both the request slot
// and its body reservation so the next request is admitted.
func TestDeadlineHandlerStalledBodyFailsWithinIdleBound(t *testing.T) {
	oldIdle := requestBodyIdleTimeout
	requestBodyIdleTimeout = 100 * time.Millisecond
	defer func() { requestBodyIdleTimeout = oldIdle }()

	cfg := config.Default()
	cfg.MaxInflightRequests = 1
	cfg.MaxInflightBodyBytes = config.MaxInflightBodyBytesMin
	cfg.MaxRequestBytes = config.MaxInflightBodyBytesMin
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(deadlineHandler{next: proxy.New(cfg, metrics.Noop{})})
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Headers arrive, then the peer sends no body bytes at all.
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\nX-Proxy-Base-URL: %s\r\nAuthorization: Bearer fixture-key\r\nConnection: close\r\n\r\n", cfg.MaxInflightBodyBytes, upstream.URL); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("stalled body did not produce a response: %v", err)
	}
	elapsed := time.Since(start)
	resp.Body.Close()
	if elapsed > time.Second {
		t.Fatalf("stalled body failed after %v, want within the idle bound", elapsed)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stalled body status = %d, want 400", resp.StatusCode)
	}

	// The released request slot and body budget must admit the next request
	// immediately; a leaked reservation would answer 503 forever.
	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	admitted := false
	for attempt := 0; attempt < 50; attempt++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer fixture-key")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("request after stalled body = %d, want 200", resp.StatusCode)
			}
			admitted = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !admitted {
		t.Fatal("admission capacity was not released after the stalled body timed out")
	}
}

// TestDeadlineHandlerSlowProgressingBodyCompletes pins the other half of H6: a
// slow but progressing upload is not mistaken for a stalled one.
func TestDeadlineHandlerSlowProgressingBodyCompletes(t *testing.T) {
	oldIdle := requestBodyIdleTimeout
	requestBodyIdleTimeout = 500 * time.Millisecond
	defer func() { requestBodyIdleTimeout = oldIdle }()

	lengths := make(chan int, 1)
	srv := httptest.NewServer(deadlineHandler{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			lengths <- -1
			return
		}
		lengths <- len(body)
		w.WriteHeader(http.StatusNoContent)
	})})
	defer srv.Close()

	const chunk = 4 << 10
	const chunks = 8
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "POST /upload HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", chunk*chunks); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'x'}, chunk)
	for i := 0; i < chunks; i++ {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("slow progressing body did not complete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("slow progressing body status = %d, want 204", resp.StatusCode)
	}
	select {
	case got := <-lengths:
		if got != chunk*chunks {
			t.Fatalf("slow progressing body length = %d, want %d", got, chunk*chunks)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow progressing body handler never completed")
	}
}

// TestDeadlineHandlerNormalUploadUnchanged keeps the ordinary client upload on
// the same path with no behavior change.
func TestDeadlineHandlerNormalUploadUnchanged(t *testing.T) {
	oldIdle := requestBodyIdleTimeout
	requestBodyIdleTimeout = 250 * time.Millisecond
	defer func() { requestBodyIdleTimeout = oldIdle }()

	srv := httptest.NewServer(deadlineHandler{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Write(body)
	})})
	defer srv.Close()

	payload := strings.Repeat("upload-", 1024)
	resp, err := http.Post(srv.URL, "application/octet-stream", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != payload {
		t.Fatalf("normal upload status=%d len=%d, want 200 and %d bytes", resp.StatusCode, len(body), len(payload))
	}
}
