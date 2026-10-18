package main

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"time"
)

// Delivery deadlines are safety guardrails, not operator tunables. The server
// deliberately has no WriteTimeout (it would cut long-lived SSE streams) and
// no ReadTimeout (a legitimate 32 MiB upload can be slow), but neither may be
// unbounded: a peer that stops reading or dribbles a body would otherwise pin
// its handler, socket, upstream generation and scheduler slot forever, and
// shutdown/restart drain could never finish.
const (
	// requestBodyReadDeadline bounds reading one request body after headers.
	// At the 32 MiB default body cap this allows about 55 KiB/s.
	requestBodyReadDeadline = 10 * time.Minute
	// responseWriteDeadline bounds any single buffered write (exports,
	// backups, debug downloads). Streaming surfaces arm a shorter deadline
	// per event at their own owner.
	responseWriteDeadline = 10 * time.Minute
)

// deadlineHandler arms the request-body deadline at the outermost server
// boundary and wraps the writer so every write re-arms a progress deadline.
// It is the one delivery-level choke point, so no handler can bypass it.
type deadlineHandler struct{ next http.Handler }

func (h deadlineHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if r.Body != nil && r.Body != http.NoBody {
		_ = rc.SetReadDeadline(time.Now().Add(requestBodyReadDeadline))
	}
	h.next.ServeHTTP(&deadlineResponseWriter{ResponseWriter: w}, r)
}

// deadlineResponseWriter keeps the optional interfaces handlers rely on
// (Flusher, Hijacker, ResponseController's Unwrap chain) while re-arming the
// write deadline before each write.
type deadlineResponseWriter struct {
	http.ResponseWriter
	explicit    time.Time
	hasExplicit bool
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
	deadline := time.Now().Add(responseWriteDeadline)
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
