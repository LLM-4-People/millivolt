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
			wantCalls: 2,
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

// TestPartialTailDoesNotVetoTruncationRescue pins E1: a partial trailing line
// is dropped by a rescue, so it must not be fed to the analyzer before the
// delivery decision. The undelivered content did not reach the client, so the
// pre-content retry must still run and deliver the fresh attempt.
func TestPartialTailDoesNotVetoTruncationRescue(t *testing.T) {
	const contentEvent = `data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		attempt := calls.Add(1)
		if attempt == 1 {
			// A complete data line without its terminating blank line: the
			// relay saves it as an undelivered tail.
			w.Write([]byte(contentEvent))
		} else {
			w.Write([]byte(contentEvent + "\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
		}
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(1), buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Count(got, `"content":"hello"`) != 1 {
		t.Fatalf("content delivered %d times, want once: %s", strings.Count(got, `"content":"hello"`), got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("delivered retry still reported truncation: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one terminal expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 {
		t.Fatalf("record = %+v, want a clean recovered record with retries=1", rec)
	}
}

// TestReasoningRescueClosesPartialEventBeforeRetry pins H4: the read-error
// reasoning rescue must close the client's pending SSE event before the fresh
// attempt appends. A complete reasoning data line whose blank-line boundary
// never arrived was relayed as an open event; without the rescue boundary the
// fresh attempt's first data line joins that event and the client sees a
// two-line payload.
func TestReasoningRescueClosesPartialEventBeforeRetry(t *testing.T) {
	const (
		roleFrame = `data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
		// A complete data line without its terminating blank line: the event
		// is still open on the client wire when the upstream dies.
		reasoningLine = `data: {"id":"A","choices":[{"delta":{"reasoning_content":"thinking hard"}}]}` + "\n"
	)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if calls.Add(1) == 1 {
			w.Write([]byte(roleFrame))
			w.Write([]byte(reasoningLine))
			w.(http.Flusher).Flush()
			// Drop the connection mid-event: the proxy sees a read error,
			// not a clean EOF.
			panic("simulated upstream crash inside a reasoning event")
		}
		w.Write([]byte(`data: {"id":"B","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"B","choices":[{"delta":{"content":"hello"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"B","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
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
	if strings.Count(got, `"reasoning_content":"thinking hard"`) != 1 {
		t.Fatalf("attempt-1 reasoning must stay on the wire once: %s", got)
	}
	if strings.Count(got, `"content":"hello"`) != 1 {
		t.Fatalf("fresh answer delivered %d times, want once: %s", strings.Count(got, `"content":"hello"`), got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one terminal expected: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the reset must be absorbed by the rescue, not surfaced: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("rescued request facts = %+v, want clean with retries=1 and a truncated absorbed attempt", rec)
	}
}

// TestDroppedTailDoesNotMarkAnswer pins NEW-2: a trailing partial content line
// is never delivered when no rescue remains, because finishHold's degenerate
// boundary drops it. Feeding it first published answer content and a
// first-answer time for bytes the client never received.
func TestDroppedTailDoesNotMarkAnswer(t *testing.T) {
	const partialContent = `data: {"id":"1","choices":[{"delta":{"content":"HELLO"}}]}`
	for _, tc := range []struct {
		name        string
		budget      int
		wantCalls   int32
		wantRetries int
	}{
		{name: "no retry budget", budget: 0, wantCalls: 1, wantRetries: 0},
		{name: "budget exhausted", budget: 2, wantCalls: 3, wantRetries: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				calls.Add(1)
				w.Write([]byte(partialContent))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			cfg := newConfigWithQuality(tc.budget)
			cfg.ThinkingRetries = 0
			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, got := streamRequest(t, srv, upstream)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if strings.Contains(got, "HELLO") {
				t.Fatalf("undelivered tail reached the client: %s", got)
			}
			if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
				t.Fatalf("in-band truncated error missing: %s", got)
			}
			if strings.Count(got, "data: [DONE]") != 1 {
				t.Fatalf("exactly one terminal expected: %s", got)
			}
			if n := calls.Load(); n != tc.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", n, tc.wantCalls)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record = %+v, want the truncated error", rec)
			}
			if rec.Retries != tc.wantRetries {
				t.Fatalf("retries = %d, want %d", rec.Retries, tc.wantRetries)
			}
			if rec.HadAnswerContent || !rec.FirstAnswerAt.IsZero() || !rec.FirstTokenAt.IsZero() {
				t.Fatalf("record marks answer content for dropped bytes: hadAnswer=%v firstAnswer=%v firstToken=%v",
					rec.HadAnswerContent, rec.FirstAnswerAt, rec.FirstTokenAt)
			}
		})
	}
}

// TestDeliveredTailStillCounts is the control for NEW-2: a stream that already
// terminated keeps its partial tail on the wire, and the tail still reaches the
// record through the delivering path.
func TestDeliveredTailStillCounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n"))
		w.Write([]byte(`data: {"id":"1","choices":[{"delta":{"content":"TAIL"}}]}`))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := newConfigWithQuality(0)
	cfg.ThinkingRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(got, `"content":"TAIL"`) {
		t.Fatalf("delivered partial tail missing: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("terminated stream surfaced truncation: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || !rec.HadAnswerContent || rec.FirstAnswerAt.IsZero() {
		t.Fatalf("record = %+v, want the delivered answer counted", rec)
	}
}

// TestDegenerateTailCanCompleteStream pins NEW-4: only the truncation class is
// invariant under feeding the trailing partial line. When a terminal marker was
// already seen but the answer or tool call itself arrives in that tail, the feed
// turns empty_completion, empty_tool_call or reasoning_only into a healthy
// completion; dropping the tail would fabricate an error and lose the output.
func TestDegenerateTailCanCompleteStream(t *testing.T) {
	const (
		roleFrame       = `data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
		reasoningFrame  = `data: {"id":"1","choices":[{"delta":{"reasoning_content":"think"}}]}` + "\n\n"
		finishStop      = `data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n"
		finishToolCalls = `data: {"id":"1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n"
		contentTail     = `data: {"id":"1","choices":[{"delta":{"content":"TAIL"}}]}`
		toolCallTail    = `data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`
	)
	for _, tc := range []struct {
		name            string
		thinkingRetries int
		first           []string
		tail            string
		forbidCode      string
		wantBody        string
		wantAnswer      bool
		wantToolCalls   int
	}{
		{
			name:       "empty completion tail supplies the answer",
			first:      []string{roleFrame, finishStop},
			tail:       contentTail,
			forbidCode: metrics.CodeEmptyCompletion,
			wantBody:   `"content":"TAIL"`,
			wantAnswer: true,
		},
		{
			name:          "empty tool call tail supplies the call",
			first:         []string{roleFrame, finishToolCalls},
			tail:          toolCallTail,
			forbidCode:    metrics.CodeEmptyToolCall,
			wantBody:      `"id":"call_1"`,
			wantToolCalls: 1,
		},
		{
			name:       "reasoning-only tail supplies the answer",
			first:      []string{reasoningFrame, finishStop},
			tail:       contentTail,
			forbidCode: metrics.CodeReasoningOnly,
			wantBody:   `"content":"TAIL"`,
			wantAnswer: true,
		},
		{
			name:            "reasoning-only tail before the stop rescue",
			thinkingRetries: 1,
			first:           []string{reasoningFrame, finishStop},
			tail:            contentTail,
			forbidCode:      metrics.CodeReasoningOnly,
			wantBody:        `"content":"TAIL"`,
			wantAnswer:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				calls.Add(1)
				for _, frame := range tc.first {
					w.Write([]byte(frame))
				}
				w.Write([]byte(tc.tail))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			cfg := newConfigWithQuality(0)
			cfg.ThinkingRetries = tc.thinkingRetries
			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, got := streamRequest(t, srv, upstream)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if !strings.Contains(got, tc.wantBody) {
				t.Fatalf("partial tail output missing (%s): %s", tc.wantBody, got)
			}
			if strings.Contains(got, `"code":"`+tc.forbidCode+`"`) {
				t.Fatalf("fabricated %s despite the completing tail: %s", tc.forbidCode, got)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("upstream calls = %d, want 1 (the tail completes the stream)", n)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.IsError() || rec.Retries != 0 {
				t.Fatalf("record = %+v, want a healthy single attempt", rec)
			}
			if rec.HadAnswerContent != tc.wantAnswer || rec.ToolCalls != tc.wantToolCalls {
				t.Fatalf("record answer/tools = %v/%d, want %v/%d",
					rec.HadAnswerContent, rec.ToolCalls, tc.wantAnswer, tc.wantToolCalls)
			}
		})
	}
}

// TestTerminalTailIsDelivered pins NEW-6: an unterminated final Chat line can
// carry the terminal marker itself. The analyzer's tail classifier must
// recognize it so the relay feeds and delivers it; dropping it fabricated a
// truncated error after a complete answer and lost the finish reason.
func TestTerminalTailIsDelivered(t *testing.T) {
	const (
		roleFrame    = `data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
		contentFrame = `data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}` + "\n\n"
		finishTail   = `data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`
		doneTail     = `data: [DONE]`
	)
	for _, tc := range []struct {
		name        string
		source      string
		wantBody    []string
		forbidCode  string
		wantCode    string
		wantFinish  string
		wantAnswer  bool
		wantIsError bool
	}{
		{
			name:       "content then finish without newline",
			source:     roleFrame + contentFrame + finishTail,
			wantBody:   []string{`"content":"hello"`, `"finish_reason":"stop"`},
			forbidCode: metrics.CodeTruncated,
			wantFinish: "stop",
			wantAnswer: true,
		},
		{
			name:       "content then done without newline",
			source:     roleFrame + contentFrame + doneTail,
			wantBody:   []string{`"content":"hello"`, "data: [DONE]"},
			forbidCode: metrics.CodeTruncated,
			wantAnswer: true,
		},
		{
			// The tail still terminates the stream before the answer: the
			// empty completion is the correct class, not a truncation, and
			// the finish reason reaches the record even though the boundary
			// replaces the withheld terminal bytes.
			name:        "empty completion with finish without newline",
			source:      roleFrame + finishTail,
			forbidCode:  metrics.CodeTruncated,
			wantCode:    metrics.CodeEmptyCompletion,
			wantFinish:  "stop",
			wantIsError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tc.source))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			cfg := newConfigWithQuality(0)
			cfg.ThinkingRetries = 0
			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, got := streamRequest(t, srv, upstream)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			for _, want := range tc.wantBody {
				if !strings.Contains(got, want) {
					t.Fatalf("delivered body missing %q: %s", want, got)
				}
			}
			if strings.Contains(got, `"code":"`+tc.forbidCode+`"`) {
				t.Fatalf("terminal tail was reported as %s: %s", tc.forbidCode, got)
			}
			if tc.wantCode != "" && !strings.Contains(got, `"code":"`+tc.wantCode+`"`) {
				t.Fatalf("expected %s error missing: %s", tc.wantCode, got)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.FinishReason != tc.wantFinish {
				t.Fatalf("finish reason = %q, want %q", rec.FinishReason, tc.wantFinish)
			}
			if rec.HadAnswerContent != tc.wantAnswer {
				t.Fatalf("answer content = %v, want %v", rec.HadAnswerContent, tc.wantAnswer)
			}
			if rec.IsError() != tc.wantIsError || rec.ErrorCode != tc.wantCode {
				t.Fatalf("record error = %v/%q, want %v/%q", rec.IsError(), rec.ErrorCode, tc.wantIsError, tc.wantCode)
			}
		})
	}
}
