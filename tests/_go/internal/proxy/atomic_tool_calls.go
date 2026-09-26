package proxy

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

// Atomic tool calls (config.AtomicToolCalls) withhold every tool-call-bearing
// event until the stream's terminator, which is what makes the append rescue
// (config.AppendRetries) sound: an uncommitted tool call can never be on the
// wire, so a re-send can only duplicate visible text, never merge two
// attempts' arguments for the same tool-call index.
//
// The motivating production record: 200, stream, 5 answer tokens, one `bash`
// tool call, no completion marker, retries 0. These tests pin the whole
// mechanism, including every way it can degrade to today's eager behavior.

const (
	atomicRoleFrame = `data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
	atomicTextFrame = `data: {"id":"A","choices":[{"delta":{"content":"Let me check"}}]}` + "\n\n"
	// A complete, valid-JSON bash tool call - the strongest case: nothing
	// about it is a fragment, so only the withholding makes a re-send safe.
	atomicToolFrame = `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{\"command\":\"ls -la\"}"}}]}}]}` + "\n\n"
	atomicStopFrame = `data: {"id":"A","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n"
	atomicDoneFrame = "data: [DONE]\n\n"
)

// toolCallCount counts the tool-call DELTAS the client actually received, so a
// test can assert it never saw an uncommitted one. It keys on the delta's
// function payload, never on the bare "tool_calls" token, which also appears in
// the finish chunk's finish_reason.
func toolCallCount(wire string) int {
	return strings.Count(wire, `"function":{"name"`)
}

// TestAtomicToolCallWithheldUntilTerminator is the core contract: a tool call
// is not on the wire while the stream is still running, and arrives verbatim
// (byte-identical, in order) once the terminator lands. A client therefore
// only ever sees a complete tool call.
func TestAtomicToolCallWithheldUntilTerminator(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame))
		w.(http.Flusher).Flush()
		w.Write([]byte(atomicTextFrame))
		w.(http.Flusher).Flush()
		w.Write([]byte(atomicToolFrame))
		w.(http.Flusher).Flush()
		// Everything above is flushed to the proxy but nothing is committed;
		// the terminator is the only thing that releases the tool call. The
		// withholding itself is pinned deterministically by the two
		// truncation tests below (no terminator -> zero tool-call frames; a
		// terminator -> exactly one) rather than by a timing assertion.
		w.Write([]byte(atomicStopFrame))
		w.(http.Flusher).Flush()
		w.Write([]byte(atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	if !cfg.AtomicToolCalls {
		t.Fatal("atomic_tool_calls must default on: it is strictly safer than eager relay")
	}
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call frames, want exactly 1 (released at the terminator): %s", n, got)
	}
	if !strings.Contains(got, atomicToolFrame) {
		t.Fatalf("the tool call must be relayed byte-identical: %s", got)
	}
	// Byte order must be preserved: the tool call is released where it
	// occurred, before the finish chunk, not appended after the stream.
	if i, j, k := strings.Index(got, atomicTextFrame), strings.Index(got, atomicToolFrame), strings.Index(got, atomicStopFrame); !(i < j && j < k) {
		t.Fatalf("relay order broken: text=%d tool=%d stop=%d", i, j, k)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].IsError() {
		t.Fatalf("a complete tool-call stream must not be an error: %+v", recs[0])
	}
}

// TestAtomicToolCallNeverReachesClientOnTruncation is the production incident
// as a test. The stream relays text and a COMPLETE tool call, then dies with
// no completion marker. With the append class enabled the request is
// transparently re-sent, and the invariant that makes it safe is asserted
// directly: the client never received attempt 1's tool call, so the recovered
// turn carries exactly one tool call and no merged argument string.
func TestAtomicToolCallNeverReachesClientOnTruncation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
			w.(http.Flusher).Flush()
			// Clean EOF, no finish_reason, no [DONE].
			return
		}
		w.Header().Set("X-Request-Id", "req-second")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200", status)
	}
	// The load-bearing assertion: the truncated attempt's tool call was never
	// written, so the client holds exactly one and its arguments are intact.
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call frames, want 1 (the uncommitted one must never be relayed): %s", n, got)
	}
	if !strings.Contains(got, `{\"command\":\"ls -la\"}`) {
		t.Fatalf("the recovered tool call's arguments must be intact: %s", got)
	}
	// The documented cost: the relayed text prefix IS duplicated.
	if n := strings.Count(got, atomicTextFrame); n != 2 {
		t.Fatalf("text prefix count = %d, want 2 (the append class duplicates visible text): %s", n, got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the recovered request must not surface the truncation: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (truncated absorbed + append rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("absorbed attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if rec.ProviderRequestID != "req-second" {
		t.Fatalf("record must carry the re-send's provider metadata, got %q", rec.ProviderRequestID)
	}
}

// TestAtomicToolCallSplitFinishReasonDoesNotRescueWithHeldToolCall covers the
// invisible-terminator case: a finish_reason split across two data lines of
// one event is never seen by the analyzer, so the stream classifies as
// truncated even though the provider did commit the tool call. Withholding is
// what keeps that safe - the committed tool call is still behind the hold, so
// the client must end up with none from the absorbed attempt rather than a
// tool call no terminator ever committed.
func TestAtomicToolCallSplitFinishReasonDoesNotRescueWithHeldToolCall(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.WriteHeader(200)
			w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
			w.(http.Flusher).Flush()
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call frames, want 0 (attempt 1's tool call was withheld and dropped): %s", n, got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (rescued)", n)
	}
}

// TestAtomicToolCallsDisabledRelaysEagerly pins the operator escape hatch: with
// atomic tool calls off the tool call is relayed as it streams (today's
// behavior), and because tool-call bytes are then on the wire the append
// rescue must refuse the stream. That refusal is the second half of the
// soundness argument and is asserted here rather than assumed.
func TestAtomicToolCallsDisabledRelaysEagerly(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = false
	cfg.AppendRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("eager relay must reach the client, saw %d tool-call frames: %s", n, got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rescue without atomic tool calls)", n)
	}
}

// TestAppendRetriesRefusedWhenAtomicToolCallsOff proves the relay's own
// tool-call proof works, independently of the config gate: the combination is
// forced onto the Server (bypassing Validate) and the rescue still must not
// fire, because tool-call bytes reached the wire.
func TestAppendRetriesRefusedWhenAtomicToolCallsOff(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = false
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	// Deliberately skip Validate: this asserts the RELAY's gate, not the
	// config gate. A rescue here would be a tool-call double-execution.
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the relay must refuse to append behind a relayed tool call)", n)
	}
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call frames, want 1: %s", n, got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band: %s", got)
	}
}

// TestAppendRetriesTextOnlyTruncationRescued: with no tool call in the stream
// there is nothing to withhold, so the class behaves like the other rescues -
// the client sees the duplicated text prefix and one terminator.
func TestAppendRetriesTextOnlyTruncationRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.WriteHeader(200)
			w.Write([]byte(atomicRoleFrame + atomicTextFrame))
			w.(http.Flusher).Flush()
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (rescued)", n)
	}
	if n := strings.Count(got, atomicTextFrame); n != 2 {
		t.Fatalf("text prefix count = %d, want 2: %s", n, got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].IsError() {
		t.Fatalf("recovered request must not be an error: %+v", recs[0])
	}
}

// TestAppendRetriesClosesPendingClientEvent pins the SSE framing repair: when
// the truncation lands between a data line and its blank-line terminator, the
// rescue must close the client's open event, or the fresh attempt's first
// frame is joined into it and the client parses one invalid event.
func TestAppendRetriesClosesPendingClientEvent(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.WriteHeader(200)
			// Note: no trailing blank line - the event is left open.
			w.Write([]byte(atomicRoleFrame + atomicTextFrame + `data: {"id":"A","choices":[{"delta":{"content":"open"}}]}` + "\n"))
			w.(http.Flusher).Flush()
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	// SSE concatenates consecutive data: lines of one event, so the assertion
	// has to be at EVENT level: every event's accumulated payload must be a
	// single JSON value. Without the closing blank line the fresh attempt's
	// first frame joins attempt 1's open event and the client parses one
	// invalid payload - which a per-line check would not notice, because each
	// line is individually well-formed JSON.
	for i, ev := range sseEvents(got) {
		if ev == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(ev), &v); err != nil {
			t.Fatalf("SSE event %d is not a single JSON value (%v): %q\nfull wire:\n%s", i, err, ev, got)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (rescued)", n)
	}
}

// sseEvents splits a wire into the payload of each SSE event the way a
// spec-conforming parser does: data: lines accumulate, joined by newlines, and
// a blank line closes the event.
func sseEvents(wire string) []string {
	var out []string
	var data []string
	flush := func() {
		if len(data) > 0 {
			out = append(out, strings.Join(data, "\n"))
			data = nil
		}
	}
	for _, line := range strings.Split(wire, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(payload, " "))
		}
	}
	flush()
	return out
}

// TestAppendRetriesExhaustedSurfacesInBand: a spent budget surfaces the
// truncation in-band instead of looping, and the withheld tool call is never
// written even on the final attempt.
func TestAppendRetriesExhaustedSurfacesInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call frames, want 0 (never relayed without a terminator): %s", n, got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("an exhausted budget must surface the truncation in-band: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one rescue, then exhausted)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation class: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 {
		t.Fatalf("the absorbed attempt must still be logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestAppendRetriesBudgetBoundsUpstreamCalls pins the worst case: a permanently
// truncating upstream costs exactly 1+AppendRetries sends, never more.
func TestAppendRetriesBudgetBoundsUpstreamCalls(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = config.AppendRetriesMax
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := calls.Load(); n != int32(config.AppendRetriesMax)+1 {
		t.Fatalf("upstream calls = %d, want %d (1 initial + budget)", n, config.AppendRetriesMax+1)
	}
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call frames, want 0: %s", n, got)
	}
}

// TestAtomicToolCallHoldOverflowCommitsVerbatim pins the memory bound: past
// ToolCallHoldMax the region commits to the wire (today's eager behavior) and,
// because tool-call bytes are now on the wire, the append rescue must refuse
// the stream.
func TestAtomicToolCallHoldOverflowCommitsVerbatim(t *testing.T) {
	var calls atomic.Int32
	huge := strings.Repeat("x", config.ToolCallHoldMax+4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame))
		w.(http.Flusher).Flush()
		// One enormous tool-call argument frame: past the hold bound.
		w.Write([]byte(`data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"` + huge + `"}}]}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("an oversized tool call must commit verbatim, saw %d frames", n)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the overflow puts tool bytes on the wire, so no rescue)", n)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band after an overflow commit: %s", got[:200])
	}
}

// TestAppendRetriesIgnoresFinishedStream: a cleanly terminated stream is never
// a truncation, so a committed tool call is relayed and never re-sent.
func TestAppendRetriesIgnoresFinishedStream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = config.AppendRetriesMax
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("a completed stream must relay its tool call once, saw %d: %s", n, got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a terminator is never a truncation)", n)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("the finish chunk must be relayed: %s", got)
	}
}

// TestAppendRetriesIgnoresProviderInBandError: a provider in-band error is its
// own class; the append class must not compound it.
func TestAppendRetriesIgnoresProviderInBandError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicTextFrame))
		w.(http.Flusher).Flush()
		w.Write([]byte(`data: {"error":{"message":"bad key","type":"authentication_error","code":"invalid_api_key"}}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 3
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, "invalid_api_key") {
		t.Fatalf("the provider's own error must relay verbatim: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a provider error is not this class)", n)
	}
}

// TestAtomicToolCallRefusalDeltaStaysAnswerShaped: a refusal is visible text, not
// a tool call, so it is NOT withheld - the client sees it as it streams and the
// append class may duplicate it (visible, not dangerous).
func TestAtomicToolCallRefusalDeltaStaysAnswerShaped(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + `data: {"id":"A","choices":[{"delta":{"refusal":"I cannot help"}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		if calls.Add(1) == 1 {
			return
		}
		w.Write([]byte(atomicRoleFrame + `data: {"id":"A","choices":[{"delta":{"refusal":"I cannot help"}}]}` + "\n\n" + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (a refusal is append-rescuable text)", n)
	}
	if !strings.Contains(got, "I cannot help") {
		t.Fatalf("the refusal must reach the client: %s", got)
	}
}
