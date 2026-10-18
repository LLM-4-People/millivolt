package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// operatorStreamWriter counts SSE writes so the test can cancel the stream once
// its snapshot has landed, without reading the body concurrently.
type operatorStreamWriter struct {
	*httptest.ResponseRecorder
	writes atomic.Int64
}

func (w *operatorStreamWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	return w.ResponseRecorder.Write(p)
}

func runOperatorStream(t *testing.T, h http.Handler, req *http.Request) *operatorStreamWriter {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	w := &operatorStreamWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req.WithContext(ctx))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for w.writes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w.writes.Load() == 0 {
		cancel()
		<-done
		t.Fatal("stream produced no SSE write")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
	return w
}

// TestAuthenticatedLiveStreamPreservesNoStore drives the real operator gate in
// front of the live dashboard stream. A valid Bearer mints the session cookie
// and stages Cache-Control: no-store; the SSE writer must preserve it. A plain
// (ungated) stream keeps the shared no-cache policy.
func TestAuthenticatedLiveStreamPreservesNoStore(t *testing.T) {
	const token = "operator-stream-fixture"
	b := metrics.NewBuffer(2)
	b.Record(&metrics.Record{ID: "stream", StatusCode: 200, Start: time.Now()})

	t.Run("bearer gate", func(t *testing.T) {
		gate := protectOperatorRequests(http.HandlerFunc(b.HandleStream), newOperatorGate(token))
		req := httptest.NewRequest(http.MethodGet, "http://proxy.example/metrics/live/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := runOperatorStream(t, gate, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store (the gate's stricter header was downgraded)", got)
		}
		if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
			t.Fatalf("Content-Type = %q, want text/event-stream", got)
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
			t.Fatalf("authenticated stream cookies = %+v, want the minted session cookie", cookies)
		}
	})

	t.Run("plain stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/metrics/live/stream", nil)
		w := runOperatorStream(t, http.HandlerFunc(b.HandleStream), req)
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("plain stream Cache-Control = %q, want no-cache", got)
		}
		if cookies := w.Result().Cookies(); len(cookies) != 0 {
			t.Fatalf("plain stream minted cookies: %+v", cookies)
		}
	})
}
