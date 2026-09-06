package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPacerApplyUpstreamAfterStartIsSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	p := newSSEPacer(rec, 15*time.Second)
	p.Start()
	if !p.applyUpstream(http.Header{"X-A": {"1"}}, http.StatusTooManyRequests) {
		t.Fatal("applyUpstream after Start must report already SSE")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE already committed)", rec.Code)
	}
}

func TestPacerApplyUpstream2xxEnablesComments(t *testing.T) {
	rec := httptest.NewRecorder()
	p := newSSEPacer(rec, 15*time.Second)
	if p.applyUpstream(http.Header{"Content-Type": {"text/event-stream"}}, http.StatusOK) {
		t.Fatal("first applyUpstream must not report already SSE")
	}
	if !p.writeComment(true) {
		t.Fatal("writeComment after 200 stream")
	}
	if !strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("body %q, want keepalive after 200 stream commit", rec.Body.String())
	}
}

func TestWriteSSEHeadersOnPacer(t *testing.T) {
	rec := httptest.NewRecorder()
	p := newSSEPacer(rec, 15*time.Second)
	writeSSEHeaders(p, http.StatusOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if !p.Started() {
		t.Fatal("writeSSEHeaders on *ssePacer must go through applyUpstream")
	}
}

func TestPacerWriteHeaderIsNotSSEComment(t *testing.T) {
	rec := httptest.NewRecorder()
	p := newSSEPacer(rec, 15*time.Second)
	p.Arm()
	p.WriteHeader(http.StatusTooManyRequests)
	if p.Started() != true {
		t.Fatal("WriteHeader must set started")
	}
	if !p.writeComment(true) {
		t.Fatal("writeComment should keep looping")
	}
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("keepalive comment on JSON 429: %q", rec.Body.String())
	}
}
