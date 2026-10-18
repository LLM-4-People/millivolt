package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
