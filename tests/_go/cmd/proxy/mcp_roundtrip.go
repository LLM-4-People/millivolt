package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// blockingDispatch answers nothing until released. It exists to model a
// handler that does not poll its context, which is what the in-process
// transport's own deadline must survive.
type blockingDispatch struct {
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
}

func newBlockingDispatch() *blockingDispatch {
	return &blockingDispatch{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (b *blockingDispatch) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(b.entered)
		<-b.release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		close(b.done)
	})
}

// stop releases the blocked handler and waits for it to finish, so no test
// ends with a dispatch goroutine still running.
func (b *blockingDispatch) stop(t *testing.T) {
	t.Helper()
	close(b.release)
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking dispatch did not finish after release")
	}
}

// TestInProcessTransportEnforcesTheRequestDeadline is the regression for the
// synchronous dispatch: a non-polling handler ran 300 ms past a 100 ms request
// deadline and returned success. The transport must return the context's own
// timeout instead of waiting for the handler.
func TestInProcessTransportEnforcesTheRequestDeadline(t *testing.T) {
	blocking := newBlockingDispatch()
	transport := inProcessTransport{dispatch: blocking.handler(), timeout: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://millivolt.internal/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := transport.RoundTrip(request)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip error = %v (response %v), want the request deadline", err, response)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("RoundTrip returned after %v, before the 100 ms deadline", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("RoundTrip returned after %v; the deadline must bound it, not the handler", elapsed)
	}
	<-blocking.entered
	blocking.stop(t)
}

// TestInProcessTransportEnforcesTheConfiguredBound covers a context without a
// deadline: the transport's own configured bound applies.
func TestInProcessTransportEnforcesTheConfiguredBound(t *testing.T) {
	blocking := newBlockingDispatch()
	transport := inProcessTransport{dispatch: blocking.handler(), timeout: 50 * time.Millisecond}
	request, err := http.NewRequest(http.MethodGet, "http://millivolt.internal/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := transport.RoundTrip(request)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip error = %v (response %v), want the configured bound", err, response)
	}
	if elapsed > time.Second {
		t.Fatalf("RoundTrip returned after %v; the configured 50 ms bound must apply", elapsed)
	}
	<-blocking.entered
	blocking.stop(t)
}

// TestInProcessTransportNormalCallReturnsPromptly pins the unchanged path: a
// handler that answers keeps working through the deadline machinery.
func TestInProcessTransportNormalCallReturnsPromptly(t *testing.T) {
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	transport := inProcessTransport{dispatch: dispatch, timeout: time.Minute}
	request, err := http.NewRequest(http.MethodGet, "http://millivolt.internal/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("status %d body %q, want the handler's answer", response.StatusCode, body)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("a normal call took %v", elapsed)
	}
}
