package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// TestInProcessRecorderRetainsAtMostTheClientBudget is the memory bound on the
// in-process recorder: a 100 MiB downstream body used to be buffered whole
// before the client's 64 MiB read cap applied, so per-call memory was bounded
// only by each route's own cap. The recorder must retain the client's budget
// plus the one byte that fires its over-budget check, whatever the handler
// writes, and its live backing array must stay about one budget: the
// bytes.Buffer it used to hold doubled a full buffer, so three abandoned calls
// retained about 402 MB. A small body must not pay the budget either.
func TestInProcessRecorderRetainsAtMostTheClientBudget(t *testing.T) {
	const chunk = 1 << 20
	offered := 512 << 20
	recorder := &inProcessRecorder{header: http.Header{}}
	block := make([]byte, chunk)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for written := 0; written < offered; written += chunk {
		n, err := recorder.Write(block)
		if err != nil || n != chunk {
			t.Fatalf("Write = %d, %v, want %d accepted bytes", n, err, chunk)
		}
	}
	runtime.ReadMemStats(&after)

	if len(recorder.body) != inProcessRecorderBufferMax {
		t.Fatalf("recorder retained %d bytes after %d offered, want the %d byte budget",
			len(recorder.body), offered, inProcessRecorderBufferMax)
	}
	// The live backing array, not just the retained length, must stay about
	// one budget: a full bytes.Buffer held 128 MiB per recorder.
	if cap(recorder.body) > inProcessRecorderBufferMax+(1<<20) {
		t.Fatalf("recorder capacity = %d after %d offered, want about the %d byte budget",
			cap(recorder.body), offered, inProcessRecorderBufferMax)
	}
	// The race-enabled build measures roughly twice the plain allocation, so
	// the bound carries headroom for it. Without the cap the same fixtures
	// grow the buffer to the offered size: over 1 GiB plain, over 2 GiB under
	// race, against a few budgets here.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 768<<20 {
		t.Fatalf("the recorder allocated %d bytes while %d were offered; the budget must bound it", allocated, offered)
	}

	small := &inProcessRecorder{header: http.Header{}}
	if _, err := small.Write(make([]byte, 4<<10)); err != nil {
		t.Fatal(err)
	}
	if len(small.body) != 4<<10 || cap(small.body) > 64<<10 {
		t.Fatalf("a 4 KiB body retained len %d cap %d; a small body must not allocate the budget",
			len(small.body), cap(small.body))
	}
}

// TestInProcessTransportOversizedBodyKeepsTheClientError pins the
// client-visible behavior over an oversized downstream body: the recorder's
// new bound must not turn the client's explicit over-budget error into a
// truncated success or a decode failure.
func TestInProcessTransportOversizedBodyKeepsTheClientError(t *testing.T) {
	gate := newOperatorGate(mcpEndpointToken)
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics/prometheus" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		chunk := make([]byte, 1<<20)
		for range 100 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	endpoint := httptest.NewServer(newMCPHandler(gate, dispatch))
	t.Cleanup(endpoint.Close)

	session := connectMCPHTTP(t, endpoint.URL+"/mcp", mcpEndpointToken)
	result := mcpCall(t, session, "prometheus", map[string]any{})
	if !result.IsError {
		t.Fatalf("a 100 MiB downstream body must be a tool error, got: %s", mcpText(t, result))
	}
	if text := mcpText(t, result); !strings.Contains(text, fmt.Sprintf("%d byte read budget", int64(64<<20))) {
		t.Fatalf("the tool error must keep the client's read-budget message: %q", text)
	}
}
