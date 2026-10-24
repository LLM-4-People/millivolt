package proxy

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// A comment line needs only its own newline. A trailing blank line would
// dispatch a pending multi-line event if the upstream pauses between its
// event: and data: lines, so the keepalive must never terminate an event.
const sseKeepaliveComment = ": keepalive\n"

// sseKeepaliveInterval is how often a quiet SSE socket gets a comment.
// Chat Completions is data-only SSE: spec-compliant clients (including the
// OpenAI SDKs) ignore comment lines. The Responses API also emits typed
// `{"type":"keepalive","sequence_number":N}` data events during long
// reasoning; those are Responses-only and would break a Chat Completions
// parser, so this writer always uses the comment form. The interval comes
// from config.SSEKeepaliveInterval and is captured per pacer so a reload
// applies to new streams only.

// Compile-time: streamBody / transformResponse / cursor emitters type-assert
// w.(http.Flusher). The wrapper is assigned to w, so Flush must exist.
var _ http.Flusher = (*ssePacer)(nil)

// ssePacer is the lowest choke point for a streaming request: the
// ResponseWriter itself. Queue wait, operator hold, and mid-stream reasoning
// gaps all go silent here. A ticker writes `: keepalive` whenever the socket
// has been idle for
// sseKeepaliveInterval. Start() commits SSE immediately for a known wait;
// Arm() only runs the ticker, which stays silent until an actual event-stream
// body has committed, so a slow JSON response never gets SSE comments.
//
// Non-stream requests cannot be pinged without committing a JSON body -
// their sockets stay open on this server (no WriteTimeout) but a client
// TTFB timeout can still drop them.
type ssePacer struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	stop    chan struct{}
	done    chan struct{}
	armed   bool
	started bool
	sse     bool // true only after commitSSELocked (not a JSON status)
	// modeSet records that the body mode was assigned from the upstream
	// content type before any status commit, so WriteHeader must not treat a
	// 2xx as SSE by default.
	modeSet    bool
	stopped    bool
	stopClosed bool
	lastWrite  time.Time
	// atLineStart records whether the last body byte written was a newline
	// (or no body byte has been written yet). A comment is only safe on its
	// own line: inserting one between a partial `event:`/`data:` line and its
	// remainder corrupts the frame, so writeComment skips while mid-line.
	atLineStart   bool
	interval      time.Duration
	writeProgress time.Duration
}

func newSSEPacer(w http.ResponseWriter, interval, writeProgress time.Duration) *ssePacer {
	return &ssePacer{w: w, lastWrite: time.Now(), atLineStart: true, interval: interval, writeProgress: writeProgress}
}

// newSSEPacerFor is the single server-side reader of the two stream timing
// settings. Both stream paths construct their pacer here so the config
// snapshot is taken once and no caller can drift from the other.
func (s *Server) newSSEPacerFor(w http.ResponseWriter) *ssePacer {
	cfg := s.cfg()
	return newSSEPacer(w, cfg.SSEKeepaliveInterval, cfg.SSEWriteProgressTimeout)
}

func (p *ssePacer) Header() http.Header { return p.w.Header() }

// applyStatus writes an upstream status with its own headers while the pacer
// has not committed anything, and reports whether it won that race. The
// unclassified-status path needs the real status and content type, so it must
// not interleave with a keepalive commit.
func (p *ssePacer) applyStatus(hdr http.Header, status int) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.started {
		return false
	}
	if hdr != nil {
		copyResponseHeaders(p.w.Header(), hdr)
	}
	// The accepted status owns the body mode exactly like applyUpstream: an
	// event-stream body keeps the idle ticker alive, a JSON body stays quiet.
	p.sse = isEventStream(p.w.Header().Get("Content-Type"))
	p.started = true
	p.modeSet = true
	p.lastWrite = time.Now()
	p.w.WriteHeader(status)
	return true
}

// statusCommitted reports whether this pacer already wrote a status line and
// body mode and is still running. A non-completion status arriving after that
// point can no longer be represented as the response status.
func (p *ssePacer) statusCommitted() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started && !p.stopped
}

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
	if !p.modeSet && code < 400 {
		p.sse = true
	}
	p.modeSet = true
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
	return p.writeSSELocked(b)
}

// writeSSELocked is the one owner of a client-facing stream write: it re-arms
// the short progress bound immediately before every event or keepalive frame,
// so a quiet long-lived stream never inherits the deadline armed by an earlier
// frame. Callers hold p.mu.
func (p *ssePacer) writeSSELocked(b []byte) (int, error) {
	// A client that stops reading must not pin the handler (and Stop) inside
	// the network write forever. Re-armed per write, including keepalives.
	if p.writeProgress > 0 {
		_ = http.NewResponseController(p.w).SetWriteDeadline(time.Now().Add(p.writeProgress))
	}
	n, err := p.w.Write(b)
	if n > 0 {
		p.atLineStart = b[n-1] == '\n'
	}
	return n, err
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
	if p.started || p.stopped || (p.modeSet && !p.sse) {
		return
	}
	writeSSEHeaders(p.w, http.StatusOK)
	p.started = true
	p.modeSet = true
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

// Arm starts the idle ticker without committing headers. The body type is
// unknown until upstream headers arrive, so an idle ticker never commits SSE
// on its own: a slow JSON body must keep its real status and type. Queue and
// operator hold call Start explicitly because those waits are known stream
// holds.
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
	// A body mode already proven non-SSE belongs to the buffered owner. Do
	// not commit the status line or write a comment over its Content-Type.
	if p.modeSet && !p.sse {
		p.mu.Unlock()
		return
	}
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
			if stopped || !idle || !started || !sse {
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
	// A comment needs its own line. The force path (the first Start comment)
	// normally runs before any body byte, but the same boundary holds if a
	// caller forces one later.
	if !p.atLineStart {
		return true
	}
	if !force && time.Since(p.lastWrite) < p.interval {
		return true
	}
	if _, err := p.writeSSELocked([]byte(sseKeepaliveComment)); err != nil {
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

// applyUpstream copies hdr and records the body mode (SSE or buffered JSON)
// without committing the status line. The split is required by the stream
// branch: a non-SSE body keeps its status open so the buffered owner can still
// fail with a real 502 instead of writing one under an already committed 201.
// Returns true when keepalive comments already committed SSE; the caller must
// then relay a non-SSE or failed body in-band, never write a contradicting
// status.
func (p *ssePacer) applyUpstream(hdr http.Header) (alreadySSE bool) {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.started {
		return p.sse
	}
	if hdr != nil {
		copyResponseHeaders(p.w.Header(), hdr)
	}
	p.sse = isEventStream(p.w.Header().Get("Content-Type"))
	p.modeSet = true
	return false
}

// commitStatus writes the upstream status once the body mode is known to be
// SSE, where the body streams under exactly that status. It reports whether it
// won the race against a keepalive commit.
func (p *ssePacer) commitStatus(status int) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.started {
		return false
	}
	p.started = true
	p.lastWrite = time.Now()
	p.w.WriteHeader(status)
	return true
}

// commitError writes a protocol-native JSON HTTP error under p.mu if headers
// are still open. The generated status-line envelope is not relayed body bytes,
// so it never increments the record's ResponseBytes; a relayed upstream error
// body counts through its own write path. Returns true when SSE was already
// committed (caller emits in-band).
func (p *ssePacer) commitError(extra http.Header, inbound ir.Protocol, code int, typ, msg string) (alreadySSE bool) {
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
	writeProtocolHTTPErrorHdr(p.w, inbound, code, typ, msg, extra)
	p.started = true
	p.lastWrite = time.Now()
	return false
}
