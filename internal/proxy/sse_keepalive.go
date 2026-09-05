package proxy

import (
	"io"
	"net/http"
	"sync"
	"time"
)

const sseKeepaliveComment = ": keepalive\n\n"

// sseKeepaliveInterval is how often a quiet SSE socket gets a comment.
// Chat Completions is data-only SSE: spec-compliant clients (including the
// OpenAI SDKs) ignore comment lines. The Responses API also emits typed
// `{"type":"keepalive","sequence_number":N}` data events during long
// reasoning; those are Responses-only and would break a Chat Completions
// parser, so this writer always uses the comment form. The interval comes
// from config.SSEKeepaliveInterval (default 15s) and is captured per pacer
// so a reload applies to new streams only.

// Compile-time: streamBody / transformResponse / cursor emitters type-assert
// w.(http.Flusher). The wrapper is assigned to w, so Flush must exist.
var _ http.Flusher = (*ssePacer)(nil)

// ssePacer is the lowest choke point for a streaming request: the
// ResponseWriter itself. Queue wait, operator hold, upstream TTFB, retry
// backoff, and mid-stream reasoning gaps all go silent here. A ticker
// writes `: keepalive` whenever the socket has been idle for
// sseKeepaliveInterval. Start() commits SSE immediately (known wait);
// Arm() only runs the ticker (first comment after one idle interval, so
// a fast stream still gets the real upstream status).
//
// Non-stream requests cannot be pinged without committing a JSON body -
// their sockets stay open on this server (no WriteTimeout) but a client
// TTFB timeout can still drop them.
type ssePacer struct {
	mu         sync.Mutex
	w          http.ResponseWriter
	stop       chan struct{}
	done       chan struct{}
	armed      bool
	started    bool
	sse        bool // true only after commitSSELocked (not a JSON status)
	stopped    bool
	stopClosed bool
	lastWrite  time.Time
	interval   time.Duration
}

func newSSEPacer(w http.ResponseWriter, interval time.Duration) *ssePacer {
	return &ssePacer{w: w, lastWrite: time.Now(), interval: interval}
}

func (p *ssePacer) Header() http.Header { return p.w.Header() }

func (p *ssePacer) Unwrap() http.ResponseWriter { return p.w }

func (p *ssePacer) WriteHeader(code int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.stopped {
		return
	}
	p.started = true
	if code < 400 {
		p.sse = true
	}
	p.lastWrite = time.Now()
	p.w.WriteHeader(code)
}

func (p *ssePacer) Write(b []byte) (int, error) {
	if p == nil {
		return 0, io.ErrClosedPipe
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return 0, io.ErrClosedPipe
	}
	if !p.started {
		p.commitSSELocked()
	}
	p.lastWrite = time.Now()
	return p.w.Write(b)
}

func (p *ssePacer) Flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *ssePacer) commitSSELocked() {
	if p.started || p.stopped {
		return
	}
	writeSSEHeaders(p.w, http.StatusOK)
	p.started = true
	p.sse = true
	p.lastWrite = time.Now()
}

func (p *ssePacer) ensureArmedLocked() {
	if p.armed || p.stopped || p.w == nil {
		return
	}
	p.armed = true
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go p.loop()
}

// Arm starts the idle ticker without committing headers, so a fast stream
// still writes the real upstream status.
func (p *ssePacer) Arm() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureArmedLocked()
}

// Start commits SSE headers and flushes one comment now - used when we
// already know the request is waiting (queue or operator hold).
func (p *ssePacer) Start() {
	if p == nil || p.w == nil {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.ensureArmedLocked()
	first := !p.started
	if first {
		p.commitSSELocked()
	}
	p.mu.Unlock()
	if first {
		p.writeComment(true)
	}
}

func (p *ssePacer) loop() {
	defer close(p.done)
	// Validate rejects interval 0; time.NewTicker(0) panics. Fail closed:
	// wait for stop instead of ticking. Not a "0 = disabled" dual-default.
	if p.interval <= 0 {
		<-p.stop
		return
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			idle := !p.stopped && time.Since(p.lastWrite) >= p.interval
			started := p.started
			sse := p.sse
			stopped := p.stopped
			p.mu.Unlock()
			if stopped || !idle {
				continue
			}
			if !started {
				p.Start()
				continue
			}
			if !sse {
				continue
			}
			if !p.writeComment(false) {
				return
			}
		}
	}
}

func (p *ssePacer) writeComment(force bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.w == nil {
		return false
	}
	if !p.sse {
		return true
	}
	if !force && time.Since(p.lastWrite) < p.interval {
		return true
	}
	if _, err := io.WriteString(p.w, sseKeepaliveComment); err != nil {
		return false
	}
	p.lastWrite = time.Now()
	if f, ok := p.w.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

func (p *ssePacer) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.armed {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	if !p.stopClosed {
		p.stopClosed = true
		close(p.stop)
	}
	done := p.done
	p.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (p *ssePacer) Started() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

// applyUpstream copies hdr and writes status if headers are still open.
// Returns true when SSE was already committed (caller must not stream a
// raw 4xx JSON body onto that socket).
func (p *ssePacer) applyUpstream(hdr http.Header, status int) (alreadySSE bool) {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return p.sse
	}
	if p.started {
		return p.sse
	}
	if hdr != nil {
		copyResponseHeaders(p.w.Header(), hdr)
	}
	p.started = true
	if status < 400 {
		p.sse = true
	}
	p.lastWrite = time.Now()
	p.w.WriteHeader(status)
	return false
}

// commitError writes a JSON HTTP error under p.mu if headers are still
// open. Returns true when SSE was already committed (caller emits in-band).
func (p *ssePacer) commitError(extra http.Header, code int, body string) (alreadySSE bool) {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return false
	}
	if p.sse {
		return true
	}
	if p.started {
		return false
	}
	if extra != nil {
		copyResponseHeaders(p.w.Header(), extra)
	}
	http.Error(p.w, body, code)
	p.started = true
	p.lastWrite = time.Now()
	return false
}
