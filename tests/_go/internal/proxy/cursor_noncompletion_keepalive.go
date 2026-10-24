package proxy

// Round 34 NEW-6 regression: a slow 202 body is committed through the pacer's
// applyStatus owner so the body mode comes from Content-Type. A JSON (or
// Connect) acknowledgement with a newline must stay byte-exact and quiet; an
// event-stream acknowledgement may receive comments. A 202 that arrives after
// the pacer already committed a keepalive 200 cannot be represented, so it
// fails closed in band instead of corrupting the committed stream.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// slowCursor202Upstream returns a 202 and writes body in two pieces with a
// pause long enough for the 20 ms keepalive ticker to fire.
func slowCursor202Upstream(t *testing.T, contentType, first, second string) *httptest.Server {
	t.Helper()
	up := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(first))
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte(second))
		w.(http.Flusher).Flush()
	}), &http2.Server{}))
	up.EnableHTTP2 = true
	up.Start()
	return up
}

func cursorSlow202Request(t *testing.T, srv *httptest.Server, upstreamURL string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-4.5-sonnet-thinking","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("X-Proxy-Format", "cursor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func slow202Config(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	cfg := cursorTestCfg(upstreamURL)
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	return cfg
}

func TestCursorSlow202JSONBodyStaysQuiet(t *testing.T) {
	const body = "{\n\"ok\":true}\n"
	up := slowCursor202Upstream(t, "application/json", "{\n", "\"ok\":true}\n")
	defer up.Close()
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(slow202Config(t, up.URL), buf))
	defer srv.Close()

	status, got := cursorSlow202Request(t, srv, up.URL)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", status, got)
	}
	if got != body {
		t.Fatalf("202 body = %q, want the exact %q", got, body)
	}
	if strings.Contains(got, ": keepalive") {
		t.Fatalf("JSON 202 body received SSE comments: %q", got)
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.ResponseBytes != int64(len(body)) {
		t.Fatalf("ResponseBytes = %d, want the %d body bytes", rec.ResponseBytes, len(body))
	}
	if rec.IsError() {
		t.Fatalf("record = %+v, want no error for a relayed 202", rec)
	}
}

func TestCursorSlow202SSEBodyMayKeepAlive(t *testing.T) {
	up := slowCursor202Upstream(t, "text/event-stream", "data: one\n\n", "data: two\n\n")
	defer up.Close()
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(slow202Config(t, up.URL), buf))
	defer srv.Close()

	status, got := cursorSlow202Request(t, srv, up.URL)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", status, got)
	}
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("event-stream 202 body got no comment despite the 150 ms pause: %q", got)
	}
	if !strings.Contains(got, "data: one\n\n") || !strings.Contains(got, "data: two\n\n") {
		t.Fatalf("event-stream 202 body lost its data: %q", got)
	}
}

// TestCommittedPacer202FailsClosed pins the documented race: once a keepalive
// 200 owns the status line, a later 202 is surfaced as an in-band error rather
// than written as a body under the wrong status.
func TestCommittedPacer202FailsClosed(t *testing.T) {
	recorder := httptest.NewRecorder()
	s := New(config.Default(), metrics.Noop{})
	pacer := s.newSSEPacerFor(recorder)
	defer pacer.Stop()
	pacer.Start()
	resp := &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader("accepted")),
	}
	rec := &metrics.Record{}
	s.relayNonCompletionStatus(context.Background(), pacer, resp, rec, ir.Chat)
	body := recorder.Body.String()
	if strings.Contains(body, "accepted") {
		t.Fatalf("committed pacer wrote the 202 body: %q", body)
	}
	if !strings.Contains(body, `"api_error"`) || !strings.Contains(body, "upstream HTTP 202") {
		t.Fatalf("committed pacer did not surface the 202 in band: %q", body)
	}
	if rec.ErrorType != "api_error" {
		t.Fatalf("record error type = %q, want api_error", rec.ErrorType)
	}
}
