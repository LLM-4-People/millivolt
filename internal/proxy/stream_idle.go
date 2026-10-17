package proxy

import (
	"context"
	"io"
	"sync"
	"time"
)

// upstreamStreamIdleTimeout bounds silence inside an upstream response body.
// ResponseHeaderTimeout only covers header arrival: a gateway that returns 200
// and then emits nothing would otherwise pin the handler, the upstream
// connection and the scheduler slot forever. Ten minutes is far beyond any
// documented reasoning gap and remains an internal safety guardrail.
const upstreamStreamIdleTimeout = 10 * time.Minute

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
}

func newIdleCancelBody(rc io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) io.ReadCloser {
	if rc == nil || cancel == nil || timeout <= 0 {
		return rc
	}
	return &idleCancelBody{rc: rc, cancel: cancel, timeout: timeout}
}

func (b *idleCancelBody) arm() {
	b.mu.Lock()
	if !b.closed {
		if b.timer == nil {
			b.timer = time.AfterFunc(b.timeout, b.cancel)
		} else {
			b.timer.Reset(b.timeout)
		}
	}
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
