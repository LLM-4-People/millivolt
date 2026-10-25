package proxy

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// requestBodyReadInit is the initial capacity of the growable request-body
// scratch. It is a read granularity, not a request-size limit: the scratch
// grows only as bytes actually arrive.
const requestBodyReadInit = 64 << 10

// requestBodyScratchMax bounds the one request-body scratch retained for reuse
// between requests. A body may grow the scratch past this bound (up to
// max_request_bytes); the oversized scratch is then dropped instead of
// retained, so the pool can never pin an operator-sized body. Internal
// allocation guardrail, not a request-size limit.
const requestBodyScratchMax = 32 << 20

// requestBodyScratchPool parks at most one growable read scratch. A body with
// no declared length has to grow as it arrives; a fresh growable buffer per
// request would pay a doubling sequence (about two body copies) every time.
// Reusing the last scratch leaves the steady-state allocation at the one
// exact buffer returned to the caller. The mutex and the single slot bound the
// retained memory: concurrent readers simply allocate their own scratch and
// cannot share bytes. A single scratch also means a later body of a different
// size reuses whatever capacity was proven necessary before.
var requestBodyScratchPool struct {
	mu  sync.Mutex
	buf []byte
}

func takeRequestBodyScratch(limit int) []byte {
	requestBodyScratchPool.mu.Lock()
	buf := requestBodyScratchPool.buf
	requestBodyScratchPool.buf = nil
	requestBodyScratchPool.mu.Unlock()
	if cap(buf) == 0 {
		return make([]byte, 0, min(requestBodyReadInit, limit))
	}
	return buf[:0]
}

func releaseRequestBodyScratch(buf []byte) {
	if cap(buf) == 0 || cap(buf) > requestBodyScratchMax {
		return
	}
	requestBodyScratchPool.mu.Lock()
	if requestBodyScratchPool.buf == nil {
		requestBodyScratchPool.buf = buf[:0]
	}
	requestBodyScratchPool.mu.Unlock()
}

// readRequest preserves the exact body for forwarding/retries and extracts
// routing + observability metadata once. Direct callers use the 1 MiB
// speculative guardrail; the proxy passes the admitted upload size so a
// declared body can be read once in place instead of prefix + copy.
func readRequest(r *http.Request, maxBytes int64, preview bool) (body []byte, rec *metrics.Record, err error) {
	return readRequestHint(r, maxBytes, preview, requestReadHintMax)
}

func readRequestHint(r *http.Request, maxBytes int64, preview bool, hintMax int64) (body []byte, rec *metrics.Record, err error) {
	rec = new(metrics.Record)
	if r.Body == nil {
		rec.Start = time.Now()
		return nil, rec, nil
	}
	defer r.Body.Close()
	if hintMax <= 0 {
		hintMax = requestReadHintMax
	}
	body, err = readRequestBody(r.Body, r.ContentLength, maxBytes, hintMax)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, &http.MaxBytesError{Limit: maxBytes}
	}
	// The inbound body size: one assignment on the bytes already in hand -
	// no second read, no timing change (Start below still excludes upload).
	// Stamped before the metadata decode so invalid-JSON passthrough
	// requests carry their body size too.
	rec.RequestBytes = int64(len(body))
	// Request timing excludes upload, but includes the unified metadata decode
	// and any later format translation. This is the sole Start owner.
	rec.Start = time.Now()
	if err := parseLLMRequest(body, rec, preview); err != nil {
		return nil, nil, err
	}
	return body, rec, nil
}

// readRequestBody reads at most maxBytes+1 bytes. A declared length that fits
// the caller's admitted hint is read directly into the one body allocation
// that is returned; every other shape (chunked, a zero/understated length, or
// a declared length past the speculative hint) grows through the bounded
// scratch and is copied once into an exact-size result. The returned slice may
// exceed maxBytes by one byte; the caller owns the typed limit error.
func readRequestBody(body io.Reader, contentLength, maxBytes, hintMax int64) ([]byte, error) {
	limit := maxBytes + 1
	reader := io.LimitReader(body, limit)
	hint := min(contentLength, limit, hintMax)
	switch {
	case contentLength < 0:
		// Unknown length (chunked): grow as the bytes arrive.
		return readGrowingRequestBody(reader, maxBytes, limit)
	case hint > 0:
		if contentLength <= hintMax {
			// The declared length fits the admitted allocation: read it in
			// place, with no growth or right-sizing copy. A short body is
			// accepted exactly as before.
			buf := make([]byte, int(hint))
			n, err := io.ReadFull(reader, buf)
			buf = buf[:n]
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				err = nil
			}
			if err != nil {
				return nil, err
			}
			// A body longer than a short/understated Content-Length still
			// reads to the limit, as it always has; the tail probe costs
			// nothing when the reader is already at EOF.
			return readRequestBodyTail(reader, buf, maxBytes, limit)
		}
		// The declared length exceeds the speculative hint. Do not trust it
		// for an allocation; grow from the bytes that actually arrive.
		return readGrowingRequestBody(reader, maxBytes, limit)
	default:
		// A zero or absent declared length is still read to the limit: a
		// client may understate it.
		return readGrowingRequestBody(reader, maxBytes, limit)
	}
}

// readRequestBodyTail appends any bytes past the already-read prefix. A
// one-byte probe answers the normal end-of-body case without an allocation;
// only a genuinely longer body enters the grower, which starts from the
// already-read prefix rather than re-reading it.
func readRequestBodyTail(reader io.Reader, body []byte, maxBytes, limit int64) ([]byte, error) {
	var probe [1]byte
	for {
		n, err := reader.Read(probe[:])
		if n > 0 {
			if err != nil && err != io.EOF {
				return nil, err
			}
			buf := takeRequestBodyScratch(int(limit))
			need := len(body) + n
			if need > cap(buf) {
				buf = growRequestBodyCap(buf, need, int(limit))
			}
			buf = append(buf, body...)
			buf = append(buf, probe[:n]...)
			return finishGrowingRequestBody(buf, reader, maxBytes, limit)
		}
		if err != nil {
			if err == io.EOF {
				return body, nil
			}
			return nil, err
		}
	}
}

// readGrowingRequestBody buffers a body whose length is unknown. It reads
// through the bounded scratch, then copies once into the exact-size result
// returned to the caller.
func readGrowingRequestBody(reader io.Reader, maxBytes, limit int64) ([]byte, error) {
	return finishGrowingRequestBody(takeRequestBodyScratch(int(limit)), reader, maxBytes, limit)
}

func finishGrowingRequestBody(buf []byte, reader io.Reader, maxBytes, limit int64) ([]byte, error) {
	buf, err := growRequestBody(buf, reader, int(limit))
	if err != nil {
		releaseRequestBodyScratch(buf)
		return nil, err
	}
	if int64(len(buf)) > maxBytes {
		// The caller rejects with MaxBytesError and never reads these bytes.
		// Returning the scratch avoids a pointless exact-size copy; it is not
		// pooled because the caller still holds the slice until it returns.
		return buf, nil
	}
	body := make([]byte, len(buf))
	copy(body, buf)
	releaseRequestBodyScratch(buf)
	return body, nil
}

// growRequestBody appends every remaining byte from reader to buf until EOF or
// the limit is reached. Each growth allocates one new buffer, unlike
// bytes.Buffer's growSlice (a make plus an append copy, so two body-sized
// allocations per step), and a full buffer is probed with one byte before
// growing so an exact-size body leaves the scratch at that exact capacity
// instead of triggering a pointless final doubling.
func growRequestBody(buf []byte, reader io.Reader, limit int) ([]byte, error) {
	for {
		if len(buf) < cap(buf) {
			n, err := reader.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
			if err != nil {
				if err == io.EOF {
					return buf, nil
				}
				return nil, err
			}
			continue
		}
		if cap(buf) >= limit {
			// The limit reader is exhausted; a larger body is rejected by
			// the caller, which needs no more bytes.
			return buf, nil
		}
		var probe [1]byte
		n, err := reader.Read(probe[:])
		if n > 0 {
			buf = growRequestBodyCap(buf, len(buf)+1, limit)
			buf = append(buf, probe[0])
		}
		if err != nil {
			if err == io.EOF {
				return buf, nil
			}
			return nil, err
		}
	}
}

// growRequestBodyCap returns buf with room for at least need bytes. Growth
// doubles (the best worst-case total for an unknown length) but never exceeds
// the caller's limit and never falls below the initial read granularity.
func growRequestBodyCap(buf []byte, need, limit int) []byte {
	if need <= cap(buf) {
		return buf
	}
	nextCap := cap(buf) * 2
	if nextCap < need {
		nextCap = need
	}
	if nextCap < requestBodyReadInit {
		nextCap = min(requestBodyReadInit, limit)
	}
	if nextCap > limit {
		nextCap = limit
	}
	if nextCap <= cap(buf) {
		return buf
	}
	grown := make([]byte, len(buf), nextCap)
	copy(grown, buf)
	return grown
}
