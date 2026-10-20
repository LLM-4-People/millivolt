package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Delivery deadlines are operator settings owned by config. The server
// deliberately has no WriteTimeout (it would cut long-lived SSE streams) and
// no global ReadTimeout (a legitimate 32 MiB upload can be slow), but the
// request and response boundaries still enforce configured progress bounds:
// a peer that stops reading or dribbles a body would otherwise pin its
// handler, socket, upstream generation and scheduler slot forever, and
// shutdown/restart drain could never finish.
type deadlinePolicy struct {
	bodyIdle      time.Duration
	bodyRead      time.Duration
	responseWrite time.Duration
}

// deadlineHandler arms the request-body progress deadline at the outermost
// server boundary and wraps the writer so every write re-arms a progress
// deadline. It is the one delivery-level choke point, so no handler can bypass
// it. policy is resolved once per request so a reload applies to new requests
// and in-flight requests keep the values they started with.
type deadlineHandler struct {
	next   http.Handler
	policy func() deadlinePolicy
}

func (h deadlineHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.policy == nil {
		http.Error(w, "delivery policy unavailable", http.StatusInternalServerError)
		return
	}
	policy := h.policy()
	if r.Body != nil && r.Body != http.NoBody {
		rc := http.NewResponseController(w)
		absolute := time.Now().Add(policy.bodyRead)
		// Arm the absolute bound before the handler runs (the pre-progress
		// contract), then let the wrapper replace it with the shorter idle
		// bound before every read.
		_ = rc.SetReadDeadline(absolute)
		r.Body = &progressReadCloser{
			body:            r.Body,
			setReadDeadline: rc.SetReadDeadline,
			idle:            policy.bodyIdle,
			absolute:        absolute,
		}
	}
	h.next.ServeHTTP(&deadlineResponseWriter{
		ResponseWriter: w,
		responseWrite:  policy.responseWrite,
	}, r)
}

// progressReadCloser re-arms the connection read deadline before each body
// read. net/http's body Read returns the data currently available, so one
// deadline per call measures progress between reads, not how long the whole
// upload takes. The absolute bound is the request_body_read_timeout value
// captured when the handler started, so a peer that keeps trickling bytes
// cannot extend delivery forever.
type progressReadCloser struct {
	body            io.ReadCloser
	setReadDeadline func(time.Time) error
	idle            time.Duration
	absolute        time.Time
}

func (b *progressReadCloser) Read(p []byte) (int, error) {
	deadline := time.Now().Add(b.idle)
	if b.absolute.Before(deadline) {
		deadline = b.absolute
	}
	// A writer without deadline support (httptest.ResponseRecorder) leaves the
	// body readable; the real server always supports it.
	_ = b.setReadDeadline(deadline)
	n, err := b.body.Read(p)
	if err != nil {
		// The body phase is over. Clear the connection read deadline so it
		// cannot expire later while the handler is still streaming a response
		// (common for an idle body bound that is much shorter than a stream).
		// The next body read, if any, re-arms the bound.
		_ = b.setReadDeadline(time.Time{})
	}
	return n, err
}

func (b *progressReadCloser) Close() error {
	_ = b.setReadDeadline(time.Time{})
	return b.body.Close()
}

// deadlineResponseWriter keeps the optional interfaces handlers rely on
// (Flusher, Hijacker, ResponseController's Unwrap chain) while re-arming the
// write deadline before each write.
type deadlineResponseWriter struct {
	http.ResponseWriter
	responseWrite time.Duration
	explicit      time.Time
	hasExplicit   bool
}

func (w *deadlineResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetWriteDeadline is the optional interface http.ResponseController looks for
// before unwrapping. Capture the owner's shorter progress bound so the next
// Write cannot replace it with the generic response bound.
func (w *deadlineResponseWriter) SetWriteDeadline(t time.Time) error {
	w.explicit = t
	w.hasExplicit = !t.IsZero()
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(t)
}

func (w *deadlineResponseWriter) Write(p []byte) (int, error) {
	deadline := time.Now().Add(w.responseWrite)
	if w.hasExplicit && w.explicit.Before(deadline) {
		deadline = w.explicit
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
	return w.ResponseWriter.Write(p)
}

func (w *deadlineResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *deadlineResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		// Hijacked protocols (the restart watch) own their own deadlines.
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return conn, rw, err
}
