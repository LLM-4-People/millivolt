package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
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

// dispatchSSE parses raw bytes the way the WHATWG event-stream algorithm does
// and returns the data of every event it would DISPATCH. That algorithm is the
// whole point of this test: a line beginning with a colon is a comment and is
// ignored WITHOUT ending the event, while a blank line dispatches whatever the
// event's data lines accumulated. A keepalive written as a comment plus a blank
// line therefore terminates the client's open event and hands it a document that
// is not finished.
func dispatchSSE(raw string) []string {
	var (
		events []string
		data   []string
	)
	flush := func() {
		// The spec discards an event with no data rather than dispatching an
		// empty one, so a lone comment can never reach the client as a frame.
		if len(data) > 0 {
			events = append(events, strings.Join(data, "\n"))
		}
		data = data[:0]
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// comment: ignored, event stays open
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			data = append(data, strings.TrimPrefix(value, " "))
		default:
			// a field line (event:/id:/retry:) contributes no data
		}
	}
	flush()
	return events
}

// TestKeepaliveDoesNotDispatchTheOpenEvent pins the idle comment against the
// client's event framing. The keepalive used to be written as ": keepalive"
// followed by a blank line, and the blank line is an SSE event terminator: a
// keepalive landing between the two data lines of one event dispatched an
// INCOMPLETE document to the client. Measured with a 1.2 s withhold and a
// 300 ms interval, the dispatched event was invalid JSON for two of three
// document shapes, which every conforming SDK raises on - failing the whole
// turn over a keepalive. The withheld tool-call bytes were never interleaved
// (the region is written in one w.Write); only the framing was corrupted.
//
// The fixture is a legal multi-line data event - the spec joins several data
// lines with a newline, so a provider that splits a document across data lines
// splits it where a newline is insignificant JSON whitespace, and a client's
// parser reassembles it. The upstream pauses between the two lines, which is
// where the keepalive lands.
func TestKeepaliveDoesNotDispatchTheOpenEvent(t *testing.T) {
	const firstLine = `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hello",`
	const secondLine = `data: "note":"world"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		// First data line of a two-line event, with no blank line yet: the
		// client now has an OPEN event.
		w.Write([]byte(firstLine + "\n"))
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(secondLine + "\n\n"))
		w.(http.Flusher).Flush()
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.(http.Flusher).Flush()
		w.Write([]byte("data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	if cfg.SSEKeepaliveInterval <= 0 {
		t.Fatal("the fixture needs a keepalive interval the pacer will actually tick on")
	}
	buf := metrics.NewBuffer(50)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)

	// The keepalive really did land mid-event, or nothing here is proven.
	comment := strings.Index(got, ": keepalive")
	open := strings.Index(got, firstLine+"\n")
	if comment < 0 || open < 0 || comment < open {
		t.Fatalf("the fixture must put a keepalive after the open event's first data line: %q", got)
	}
	if strings.Index(got, secondLine) < comment {
		t.Fatalf("the keepalive must land between the two data lines: %q", got)
	}
	events := dispatchSSE(got)
	var chunks []map[string]any
	for i, data := range events {
		if data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("dispatched event %d is not valid JSON (%v), so every conforming SDK raises on a keepalive: %q", i, err, data)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 3 {
		t.Fatalf("dispatched %d data events, want 3 (role, the reassembled two-line content event, the finish chunk): %q", len(chunks), got)
	}
	delta := chunks[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != "hello" || delta["note"] != "world" {
		t.Fatalf("the split data lines must reassemble into one event, got delta %v", delta)
	}
}
