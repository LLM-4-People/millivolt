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
	if !p.applyUpstream(nil, http.StatusOK) {
		t.Fatal("writeSSEHeaders on *ssePacer must go through applyUpstream")
	}
}

func TestPacerWriteHeaderIsNotSSEComment(t *testing.T) {
	rec := httptest.NewRecorder()
	p := newSSEPacer(rec, 15*time.Second)
	p.Arm()
	p.WriteHeader(http.StatusTooManyRequests)
	// The started guard is behavioral: a second WriteHeader must be a no-op
	// so the JSON 429 status stays committed.
	p.WriteHeader(http.StatusInternalServerError)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second WriteHeader = %d, want the first commit to stand", rec.Code)
	}
	if !p.writeComment(true) {
		t.Fatal("writeComment should keep looping")
	}
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("keepalive comment on JSON 429: %q", rec.Body.String())
	}
}

// deadlineRecorder is an httptest.ResponseRecorder with ResponseController's
// deadline hooks, so the pacer's stream write bound is observable.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	write time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error { r.write = t; return nil }

func TestPacerArmsPerWriteDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	p := newSSEPacer(rec, time.Hour)
	if _, err := p.Write([]byte("data: x\n\n")); err != nil {
		t.Fatal(err)
	}
	if rec.write.IsZero() || time.Until(rec.write) <= 0 || time.Until(rec.write) > sseWriteProgressDeadline+time.Second {
		t.Fatalf("pacer write deadline = %v, want a near-future progress bound", rec.write)
	}
}
