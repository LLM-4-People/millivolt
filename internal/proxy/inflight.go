package proxy

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
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

// errBodyBudget reports that the bytes a request is actually uploading would
// cross the process-wide buffered-body budget. It is a retryable admission
// refusal, distinct from the per-request size cap that answers 413.
var errBodyBudget = errors.New("proxy is at its in-flight body budget")

// bodyReservation is one in-flight request's share of the buffered-body
// budget. It reserves no body capacity up front and charges the delta as bytes
// actually arrive, so the budget follows the heap actually held rather than a
// declared length.
//
// Reserving the full declared Content-Length up front let an idle connection
// hold budget it never used: eight clients declaring 32 MiB each exhausted the
// default 256 MiB budget and denied inference to every other client, while the
// separate request-count bound implies tolerance for thousands of clients.
// Reserving even one read hint per socket kept a smaller version of the same
// lever (256 sockets at the default budget, and one socket when
// max_request_bytes was configured small), so the reservation starts at zero.
type bodyReservation struct {
	l       *inflightLimiter
	charged int64
	read    int64
}

// acquireBody reserves one request slot and no body bytes. A refusal backs the
// slot out, so a rejected request leaks nothing.
func (l *inflightLimiter) acquireBody() (*bodyReservation, bool) {
	if !l.acquire(0) {
		return nil, false
	}
	return &bodyReservation{l: l}, true
}

// chargeRead charges the budget for n bytes just read. A refusal leaves the
// counter unchanged; the charged reader aborts retention on that chunk and
// the handler releases the whole reservation on return.
func (r *bodyReservation) chargeRead(n int64) bool {
	if n <= 0 {
		return true
	}
	r.read += n
	delta := r.read - r.charged
	total := r.l.bytes.Add(delta)
	if limit := r.l.byteLimit.Load(); limit > 0 && total > limit {
		r.l.bytes.Add(-delta)
		r.read = r.charged
		return false
	}
	r.charged = r.read
	return true
}

func (r *bodyReservation) release() { r.l.release(r.charged) }

// chargedReader charges every byte it delivers to the body reservation, so a
// declared length never reserves capacity the client does not send.
//
// A refused chunk is counted but not delivered: the reader returns the refusal
// error immediately, so readRequestBody releases its scratch instead of
// buffering the rest of the body. readRequestHint then drains at most the
// remaining cap window (max_request_bytes+1 minus what was already read) to
// keep the cap precedence without retaining those bytes: an over-cap body is
// still 413, which a retry can never change, while a body that fits the cap is
// the retryable 503. Letting the body reader finish instead retained up to
// max_request_bytes of uncharged heap per refused request.
type chargedReader struct {
	r      io.Reader
	charge func(int64) bool
	// read is every byte taken from the underlying reader, including the
	// refused chunk. readRequestHint uses it to bound the cap drain.
	read int64
}

func (c *chargedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.read += int64(n)
		if c.charge != nil && !c.charge(int64(n)) {
			return 0, errBodyBudget
		}
	}
	return n, err
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
func writeResponseBufferBudget(w http.ResponseWriter, rec *metrics.Record, inbound ir.Protocol) {
	if rec != nil {
		rec.StatusCode = http.StatusServiceUnavailable
		rec.ErrorType = metrics.TypeResponseBudget
		rec.ErrorMsg = errResponseBufferBudget.Error()
	}
	extra := http.Header{}
	extra.Set("Retry-After", "1")
	writeProtocolHTTPErrorHdr(w, inbound, http.StatusServiceUnavailable, typeAPIError, errResponseBufferBudget.Error(), extra)
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
