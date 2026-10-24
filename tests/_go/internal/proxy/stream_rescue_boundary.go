package proxy

// Round 8 E1 regression: a rescued first attempt can end with an unterminated
// SSE event (a data line with one newline and no blank separator). The retry's
// first data line must start a new event, not join the open payload. One
// helper now writes the closing newline before every rescue return.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func assertSingleDataEvents(t *testing.T, body string) {
	t.Helper()
	for _, event := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(event) == "" {
			continue
		}
		var data []string
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) != 1 {
			t.Fatalf("SSE event has %d data lines, want 1: %q", len(data), event)
		}
		if data[0] == "[DONE]" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(data[0]), &payload); err != nil {
			t.Fatalf("SSE data is not JSON: %q (%v)", data[0], err)
		}
	}
}

func TestRescueClosesPendingSSEEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  string
		checks []string
	}{
		{
			name:   "empty truncation leaves a partial data line",
			first:  `data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}` + "\n",
			checks: []string{`"content":"hello"`, `"finish_reason":"stop"`},
		},
		{
			name:   "reasoning truncation leaves a partial data line",
			first:  `data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking hard"}}]}` + "\n",
			checks: []string{`"reasoning_content":"thinking hard"`, `"content":"hello"`},
		},
		{
			name:   "reasoning stop holds an unterminated terminal line",
			first:  thinkingRoleFrame + thinkingFrame + `data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n",
			checks: []string{`"reasoning_content":"thinking hard"`, `"content":"hello"`},
		},
		{
			name:   "upstream error boundary remains a blank line",
			first:  `data: {"error":{"message":"temporarily unavailable","type":"api_error","code":"internal_error"}}` + "\n\n",
			checks: []string{`"content":"hello"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if calls.Add(1) == 1 {
					w.Write([]byte(tc.first))
					w.(http.Flusher).Flush()
					return
				}
				w.Write([]byte(freshAnswerFrames))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := streamRequest(t, srv, upstream)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			assertSingleDataEvents(t, got)
			for _, want := range tc.checks {
				if !strings.Contains(got, want) {
					t.Fatalf("client missing %s: %s", want, got)
				}
			}
			if strings.Count(got, "data: [DONE]") != 1 {
				t.Fatalf("exactly one terminal expected: %s", got)
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("upstream calls = %d, want 2", n)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].Retries != 1 {
				t.Fatalf("rescued record = %+v, want one retry", recs)
			}
		})
	}
}

// TestDegenerateErrorClosesPendingSSEEvent is the round 8 verifier NEW-2
// regression: a truncated attempt can end mid-event, and the degenerate error
// appended afterward must be its own event. The boundary helper now runs before
// emitDegenerate*, so a complete data line is closed and a partial line is
// dropped instead of merging content and error.
func TestDegenerateErrorClosesPendingSSEEvent(t *testing.T) {
	const (
		roleEvent    = `data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`
		contentEvent = `data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}`
	)
	for _, tc := range []struct {
		name      string
		first     string
		second    string
		wantCalls int32
		wantHello bool
	}{
		{
			name:      "single attempt with a complete data line",
			first:     contentEvent + "\n",
			wantCalls: 1,
			wantHello: true,
		},
		{
			name:      "single attempt with a partial data line",
			first:     contentEvent,
			wantCalls: 1,
		},
		{
			name:      "retry then a complete data line",
			first:     roleEvent + "\n",
			second:    contentEvent + "\n",
			wantCalls: 2,
			wantHello: true,
		},
		{
			name:      "retry then a partial data line",
			first:     roleEvent + "\n",
			second:    contentEvent,
			wantCalls: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				attempt := calls.Add(1)
				if attempt == 1 {
					w.Write([]byte(tc.first))
				} else {
					w.Write([]byte(tc.second))
				}
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := streamRequest(t, srv, upstream)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			assertSingleDataEvents(t, got)
			if strings.Count(got, "data: [DONE]") != 1 {
				t.Fatalf("exactly one terminal expected: %s", got)
			}
			if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
				t.Fatalf("in-band truncated error missing: %s", got)
			}
			if tc.wantHello && !strings.Contains(got, `"content":"hello"`) {
				t.Fatalf("complete content event was lost: %s", got)
			}
			if n := calls.Load(); n != tc.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", n, tc.wantCalls)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || !recs[0].IsError() || recs[0].ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record = %+v, want the truncated error", recs)
			}
		})
	}
}
