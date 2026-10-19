package proxy

import (
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// inflightLimiter is the process-wide admission bound. It is lock-free: a
// request increments, checks the current limit, and backs out when over, so a
// reload can change the bound without rebuilding a channel or stopping
// traffic. The counter is always decremented on the same request's exit.
type inflightLimiter struct {
	limit     atomic.Int64
	cur       atomic.Int64
	byteLimit atomic.Int64
	bytes     atomic.Int64
}

func (l *inflightLimiter) set(limit int, bodyBytes int64) {
	if limit < 0 {
		limit = 0
	}
	if bodyBytes < 0 {
		bodyBytes = 0
	}
	l.limit.Store(int64(limit))
	l.byteLimit.Store(bodyBytes)
}

// acquire reserves one request slot and its expected body bytes. A refusal
// backs both counters out, so a rejected request never leaks capacity.
func (l *inflightLimiter) acquire(bodyBytes int64) bool {
	if bodyBytes < 0 {
		bodyBytes = 0
	}
	n := l.cur.Add(1)
	if limit := l.limit.Load(); limit > 0 && n > limit {
		l.cur.Add(-1)
		return false
	}
	if bodyBytes > 0 {
		b := l.bytes.Add(bodyBytes)
		if limit := l.byteLimit.Load(); limit > 0 && b > limit {
			l.bytes.Add(-bodyBytes)
			l.cur.Add(-1)
			return false
		}
	}
	return true
}

func (l *inflightLimiter) release(bodyBytes int64) {
	if bodyBytes > 0 {
		l.bytes.Add(-bodyBytes)
	}
	l.cur.Add(-1)
}

// responseBufferBudget is the process-wide byte budget for whole-document
// upstream response buffers: translated non-streaming documents, model
// discovery bodies, and the non-streaming 200 quality spool. Reservations cover
// the bytes a bounded read can hold and are released once the buffer has been
// written, translated or decoded, on failure, or when an attempt is abandoned
// for a retry. 0 = unlimited.
type responseBufferBudget struct {
	limit atomic.Int64
	bytes atomic.Int64
}

func (b *responseBufferBudget) set(limit int64) {
	if limit < 0 {
		limit = 0
	}
	b.limit.Store(limit)
}

// acquire reserves n bytes. A refusal changes nothing, so a request that is
// turned away before its first read never leaks capacity.
func (b *responseBufferBudget) acquire(n int64) bool {
	if n <= 0 {
		return true
	}
	total := b.bytes.Add(n)
	if limit := b.limit.Load(); limit > 0 && total > limit {
		b.bytes.Add(-n)
		return false
	}
	return true
}

func (b *responseBufferBudget) release(n int64) {
	if n > 0 {
		b.bytes.Add(-n)
	}
}

// errResponseBufferBudget is the admission failure surfaced as a retryable
// 503 while the status line is still uncommitted.
var errResponseBufferBudget = errors.New("proxy is at its in-flight response buffer limit")

// writeResponseBufferBudget is the one 503 sink for response-buffer
// admission, shared by the translated non-streaming relay, model discovery,
// and the non-streaming quality spool.
func writeResponseBufferBudget(w http.ResponseWriter, rec *metrics.Record) {
	if rec != nil {
		rec.StatusCode = http.StatusServiceUnavailable
		rec.ErrorType = "response_buffer_budget"
		rec.ErrorMsg = errResponseBufferBudget.Error()
	}
	w.Header().Set("Retry-After", "1")
	http.Error(w, errJSON(typeAPIError, errResponseBufferBudget.Error()), http.StatusServiceUnavailable)
}

// responseReadReserve returns the bytes a whole-document read can hold.
// Content-Length is authoritative when present and within the read cap;
// otherwise the cap (which includes any overflow probe byte) is reserved.
func responseReadReserve(contentLength, readCap int64) int64 {
	if contentLength >= 0 && contentLength <= readCap {
		return contentLength
	}
	return readCap
}
