package proxy

// Round 4 batch 9 regressions (E3, E4): a client write failure on a committed
// non-completion SSE body is a plain 499, and a non-SSE retry body is never
// injected into the committed event-stream socket.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// errorReader fails after producing its optional prefix.
type errorReader struct {
	data []byte
	err  error
}

func (e *errorReader) Read(p []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, e.err
}

func TestNonCompletionSSEClientWriteFailureIsPlain499(t *testing.T) {
	rec := &metrics.Record{StatusCode: http.StatusAccepted}
	resp := &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"accepted\":true}\n\n")),
	}
	s := New(config.Default(), metrics.Noop{})
	s.relayNonCompletionStatus(context.Background(), failingSSEWriter{}, resp, rec)

	if rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("status = %d, want %d", rec.StatusCode, metrics.StatusClientClosedRequest)
	}
	if !rec.ClientDisconnected {
		t.Fatal("client write failure did not mark the disconnect")
	}
	if rec.ErrorType != "" || rec.ErrorCode != "" {
		t.Fatalf("client write failure was stamped as an upstream error: %+v", rec)
	}
	if rec.IsError() {
		t.Fatalf("a plain client 499 must not be an error: %+v", rec)
	}
}

func TestNonCompletionSSEUpstreamReadErrorStillStamped(t *testing.T) {
	rec := &metrics.Record{StatusCode: http.StatusAccepted}
	resp := &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&errorReader{data: []byte("data: {}\n\n"), err: errors.New("upstream reset")}),
	}
	s := New(config.Default(), metrics.Noop{})
	s.relayNonCompletionStatus(context.Background(), httptest.NewRecorder(), resp, rec)

	if rec.ClientDisconnected {
		t.Fatal("a genuine upstream read error was marked as a client disconnect")
	}
	if rec.ErrorType != errStreamRead || !rec.IsError() {
		t.Fatalf("upstream read error = type %q error %v, want %s", rec.ErrorType, rec.IsError(), errStreamRead)
	}
}
