package proxy

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleCancelBody cancels the request context when a Read makes no progress for
// the bound. Cancelling the attempt context is what closes the transport body
// and unblocks a Read; no per-connection deadline is set, so parallel HTTP/2
// streams on the same connection are unaffected.
type idleCancelBody struct {
	rc      io.ReadCloser
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	closed  bool
	// paused suppresses the idle bound while a reader legitimately has no
	// expected progress (a parked Cursor Run waiting for a client tool result).
	// The pauser owns the decision; this type only owns the timer mechanics.
	paused bool
}

func newIdleCancelBody(rc io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) io.ReadCloser {
	if rc == nil || cancel == nil || timeout <= 0 {
		return rc
	}
	return &idleCancelBody{rc: rc, cancel: cancel, timeout: timeout}
}

// wrapUpstreamBody is the single server-side reader of the upstream stream
// idle setting. Both the generic relay and the Cursor bridge use it so the
// configured bound cannot drift between transports.
func (s *Server) wrapUpstreamBody(rc io.ReadCloser, cancel context.CancelFunc) io.ReadCloser {
	return newIdleCancelBody(rc, cancel, s.cfg().UpstreamStreamIdleTimeout)
}

func (b *idleCancelBody) arm() {
	b.mu.Lock()
	b.armLocked()
	b.mu.Unlock()
}

func (b *idleCancelBody) armLocked() {
	if b.closed || b.paused {
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, b.cancel)
	} else {
		b.timer.Reset(b.timeout)
	}
}

// PauseIdle suppresses the idle bound and stops any timer already armed for an
// in-flight Read. The caller decides when silence is legitimate; the generic
// bound stays in force for every other response body.
func (b *idleCancelBody) PauseIdle() {
	b.mu.Lock()
	b.paused = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
}

// ResumeIdle re-arms the idle bound. It also covers a Read that is still
// blocked: the fresh timer cancels that read if progress does not resume.
func (b *idleCancelBody) ResumeIdle() {
	b.mu.Lock()
	b.paused = false
	b.armLocked()
	b.mu.Unlock()
}

func (b *idleCancelBody) stop() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
}

func (b *idleCancelBody) Read(p []byte) (int, error) {
	b.arm()
	n, err := b.rc.Read(p)
	b.stop()
	return n, err
}

func (b *idleCancelBody) Close() error {
	b.mu.Lock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	b.cancel()
	return b.rc.Close()
}
