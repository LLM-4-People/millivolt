package proxy

// Round 6 E3 regression: a keepalive comment must not add a blank line. The
// blank line is an SSE dispatch boundary, so a keepalive written between an
// event name and its data lines would dispatch an empty/partial event.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestKeepaliveCommentDoesNotDispatchPartialEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: x\n")
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {\"v\":1}\n\n")
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, ": keepalive\n") {
		t.Fatalf("body %q, want a keepalive comment", got)
	}
	if strings.Contains(got, ": keepalive\n\n") {
		t.Fatalf("keepalive still carries a blank dispatch line: %q", got)
	}

	emptyEvents, dataEvents := 0, 0
	for _, frame := range strings.Split(got, "\n\n") {
		hasEvent := strings.Contains(frame, "event: x")
		hasData := strings.Contains(frame, "data: ")
		if hasEvent && !hasData {
			emptyEvents++
		}
		if hasEvent && hasData {
			dataEvents++
		}
	}
	if emptyEvents != 0 {
		t.Fatalf("dispatched %d event frames without data: %q", emptyEvents, got)
	}
	if dataEvents != 1 {
		t.Fatalf("data dispatches = %d, want 1: %q", dataEvents, got)
	}
}
