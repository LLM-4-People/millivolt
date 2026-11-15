package proxy

// W1 regression: SSE joins the data lines of one event with a newline, so a
// pretty-printed terminal document (several data lines making one JSON
// document) must be relayed as one event. A synthetic separator is only
// correct when the event's joined head is already a complete document, or
// before the [DONE] sentinel.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// assertParseableEvents requires every SSE event on the client wire to be
// dispatchable: its data lines join into one valid JSON document, or it is the
// [DONE] sentinel. A separator inserted inside a multi-line document reddens
// here because the fragments stop parsing.
func assertParseableEvents(t *testing.T, body string) int {
	t.Helper()
	events := 0
	for _, event := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(event) == "" {
			continue
		}
		events++
		var data []string
		for _, line := range strings.Split(event, "\n") {
			if payload, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimSpace(payload))
			}
		}
		if len(data) == 0 {
			t.Fatalf("SSE event carries no data: %q", event)
		}
		joined := strings.Join(data, "\n")
		if joined == "[DONE]" {
			continue
		}
		if !jsonValid(joined) {
			t.Fatalf("SSE event data does not parse as one JSON document: %q", event)
		}
	}
	return events
}

func jsonValid(s string) bool { return json.Valid([]byte(s)) }

func TestStreamPrettyPrintedTerminalEventStaysOneEvent(t *testing.T) {
	frames := `data: {"id":"A","choices":[{"delta":{"content":"hello"}}]}` + "\n\n" +
		"data: {\n" +
		"data:   \"id\": \"A\",\n" +
		"data:   \"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]\n" +
		"data: }\n\n" +
		"data: [DONE]\n\n"
	upstream := streamUpstream(t, frames)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", status)
	}
	// The four document lines must stay in one event.
	if !strings.Contains(got, "data: {\ndata:   \"id\": \"A\",") {
		t.Fatalf("the pretty-printed document was split: %q", got)
	}
	if events := assertParseableEvents(t, got); events != 3 {
		t.Fatalf("events = %d, want content, terminal document and [DONE] (body %q)", events, got)
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.FinishReason != "stop" || rec.IsError() {
		t.Fatalf("record = finish %q error %v, want a healthy stop", rec.FinishReason, rec.IsError())
	}
}

// TestStreamSeparatesCompleteEventsWithoutABlank pins the other half: a
// provider that omits the blank line between events still gets one event per
// document, and [DONE] never joins an open event.
func TestStreamSeparatesCompleteEventsWithoutABlank(t *testing.T) {
	frames := `data: {"id":"A","choices":[{"delta":{"content":"one"}}]}` + "\n" +
		`data: {"id":"A","choices":[{"delta":{"content":"two"}}]}` + "\n" +
		`data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n" +
		`data: [DONE]` + "\n\n"
	upstream := streamUpstream(t, frames)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", status)
	}
	if events := assertParseableEvents(t, got); events != 4 {
		t.Fatalf("events = %d, want four separate events (body %q)", events, got)
	}
	for _, needle := range []string{`"content":"one"`, `"content":"two"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if strings.Count(got, needle) != 1 {
			t.Fatalf("%q appears %d times, want 1 (body %q)", needle, strings.Count(got, needle), got)
		}
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.FinishReason != "stop" || rec.IsError() {
		t.Fatalf("record = finish %q error %v, want a healthy stop", rec.FinishReason, rec.IsError())
	}
}
