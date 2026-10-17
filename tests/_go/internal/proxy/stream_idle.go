package proxy

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type gatedReader struct {
	gate    chan struct{}
	reached chan struct{}
}

func (r *gatedReader) Read([]byte) (int, error) {
	close(r.reached)
	<-r.gate
	return 0, io.EOF
}
func (r *gatedReader) Close() error { return nil }

// TestIdleCancelBodyCancelsSilentRead pins the upstream body guard: a read
// that makes no progress cancels the attempt context, while a progressing
// read never fires it.
func TestIdleCancelBodyCancelsSilentRead(t *testing.T) {
	var canceled atomic.Bool
	cancel := func() { canceled.Store(true) }
	r := &gatedReader{gate: make(chan struct{}), reached: make(chan struct{})}
	body := newIdleCancelBody(r, cancel, 30*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, err := body.Read(buf[:])
		done <- err
	}()
	<-r.reached
	deadline := time.Now().Add(2 * time.Second)
	for !canceled.Load() {
		if time.Now().After(deadline) {
			close(r.gate)
			t.Fatal("silent read never canceled the attempt context")
		}
		time.Sleep(time.Millisecond)
	}
	close(r.gate)
	<-done

	var fastCalls atomic.Int32
	fast := newIdleCancelBody(io.NopCloser(&oneShotReader{}), func() { fastCalls.Add(1) }, 30*time.Millisecond)
	var buf [1]byte
	if _, err := fast.Read(buf[:]); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if fastCalls.Load() != 0 {
		t.Fatal("a completed read left the idle timer armed")
	}
}

type oneShotReader struct{}

func (*oneShotReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, io.EOF
}
