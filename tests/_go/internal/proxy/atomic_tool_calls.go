package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Atomic tool calls (config.AtomicToolCalls) withhold every tool-call-bearing
// event until the stream's terminator, so an uncommitted tool call can never be
// on the wire: a client that commits one at end of body can never run a tool
// from a response the model never finished.
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
	// atomicStopOnlyFrame is the terminator for a stream with no tool call: a
	// tool_calls finish_reason with zero tool-call deltas is its own degenerate
	// class (empty_tool_call) and would have its terminal region substituted.
	atomicStopOnlyFrame = `data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	atomicDoneFrame     = "data: [DONE]\n\n"
)

// toolCallMarker is what one tool_calls delta contributes to the wire. The
// deprecated function_call shape spells its own (see toolCallCount).
const toolCallMarker = `"function":{"name"`

// toolCallCount counts the tool-call DELTAS the client actually received, so a
// test can assert it never saw an uncommitted one. It keys on the delta's
// function payload, never on the bare "tool_calls" token, which also appears in
// the finish chunk's finish_reason. The deprecated function_call shape is
// counted by the cases that need it with its own spelling.
func toolCallCount(wire string) int {
	return strings.Count(wire, toolCallMarker)
}

// TestAtomicToolCallReleasedVerbatimAtTerminator asserts the RELEASE side: a
// tool call that reaches the terminator arrives byte-identical, in order, and
// exactly once, so a client only ever sees a complete one. It deliberately
// asserts nothing about mid-flight state, which no offline test can observe
// without a timing race - TestAtomicWithholdingIsBoundedByTheTerminator owns
// the withholding half, by contrasting the same bytes with and without a
// terminator.
func TestAtomicToolCallReleasedVerbatimAtTerminator(t *testing.T) {
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
	if cfg.AtomicToolCalls {
		t.Fatal("atomic_tool_calls must default OFF: withholding can discard a tool call from a provider that sends no terminator, which is a compatibility trade an operator must make deliberately")
	}
	cfg.AtomicToolCalls = true
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
// as a test. The stream relays text and a COMPLETE tool call, then dies with no
// completion marker. Withholding means the uncommitted tool call never reaches
// the client at all - so no client can commit and execute it, which is the
// hazard this setting exists to remove (a Vercel AI SDK at provider-utils 4.x
// does exactly that with the call eagerly relayed). The turn still fails, and
// says so: the truncation surfaces in-band and no upstream re-send happens.
func TestAtomicToolCallNeverReachesClientOnTruncation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
		w.(http.Flusher).Flush()
		// Clean EOF, no finish_reason, no [DONE].
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call deltas, want 0: an uncommitted tool call must never be relayed: %s", n, got)
	}
	if !strings.Contains(got, atomicTextFrame) {
		t.Fatalf("already-relayed answer content must still reach the client: %s", got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no re-send is configured)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation class: %+v", recs[0])
	}
}

// TestAtomicToolCallsDisabledRelaysEagerly pins the operator escape hatch: with
// atomic tool calls off, a tool call is relayed as it streams (the behavior
// that predates this feature). No rescue budget applies either way here, since
// the client received a tool call and every budget guarantees the client
// received no completion-contract bytes.
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
		t.Fatalf("upstream calls = %d, want 1 (no rescue budget is configured)", n)
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

// truncateForMsg bounds a failure message so a short wire cannot panic the
// test that reports it.
func truncateForMsg(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:400] + "...(truncated)"
}

// TestOversizedToolCallFramesAreCountedAndRelayed pins the whole-line case: a
// tool-call data line longer than the analyzer's per-EVENT buffer, delivered
// whole by the relay's line loop. The event buffer dropped it, so the raw-line
// producer is the only one that can see the call - without it the classifier
// read zero calls and REPLACED a complete, valid tool call with the
// empty_tool_call error. Both tool-call shapes are covered, terminated and
// truncated, because the truncated case is the one that used to leave a silent
// success: the region committed, so nothing may be substituted over its bytes,
// but the stream's own outcome is still surfaced after them.
func TestOversizedToolCallFramesAreCountedAndRelayed(t *testing.T) {
	big := strings.Repeat("x", oversizedArgs)
	// sse.Analyzer's per-event buffer cap is 1 MiB, package-private to that
	// package; the fixture must sit past it for the raw-line producer to be the
	// only producer that sees the call.
	if oversizedArgs <= 1<<20 {
		t.Fatalf("the fixture must sit past the analyzer's event buffer cap, got %d", oversizedArgs)
	}
	cases := []struct {
		name  string
		frame string
		tail  string
		// marker is the substring one whole call contributes to the wire; the
		// tool_calls shape spells it one way and the deprecated shape another.
		marker     string
		wantName   string
		wantTrunc  bool
		wantFrames int
	}{
		{
			name:       "tool_calls, terminated",
			frame:      atomicToolCallWithArgs(big, "call_1", "bash", false),
			tail:       atomicStopFrame + atomicDoneFrame,
			marker:     toolCallMarker,
			wantName:   "bash",
			wantFrames: 1,
		},
		{
			name:       "tool_calls, truncated after the region committed",
			frame:      atomicToolCallWithArgs(big, "call_1", "bash", false),
			marker:     toolCallMarker,
			wantName:   "bash",
			wantTrunc:  true,
			wantFrames: 1,
		},
		{
			name: "deprecated function_call, terminated",
			frame: `data: {"id":"A","choices":[{"delta":{"function_call":{"name":"lookup","arguments":"` + big +
				`"}}}]}` + "\n\n",
			tail:       atomicStopFrame + atomicDoneFrame,
			marker:     `"function_call":{"name":"lookup"`,
			wantName:   "lookup",
			wantFrames: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, calls, rec := atomicStream(t, atomicRoleFrame+c.frame+c.tail)

			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: the call reached the wire, so no rescue may follow it", calls)
			}
			if n := strings.Count(got, c.marker); n != c.wantFrames {
				t.Fatalf("client saw %d tool-call deltas, want %d: %s", n, c.wantFrames, truncateForMsg(got))
			}
			if strings.Contains(got, metrics.CodeEmptyToolCall) {
				t.Fatalf("a stream that DID call a tool must not be substituted with %s: %s",
					metrics.CodeEmptyToolCall, truncateForMsg(got))
			}
			if rec.ToolCalls != 1 {
				t.Fatalf("record ToolCalls = %d, want 1: the event buffer dropped this data line, so the raw-line producer is the only one that saw the call",
					rec.ToolCalls)
			}
			if len(rec.ToolNames) != 1 || rec.ToolNames[0] != c.wantName {
				t.Fatalf("record ToolNames = %v, want [%s]", rec.ToolNames, c.wantName)
			}
			if surfaced, want := strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`), c.wantTrunc; surfaced != want {
				t.Fatalf("in-band truncation surfaced = %v, want %v: %s", surfaced, want, truncateForMsg(got))
			}
			if c.wantTrunc {
				if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
					t.Fatalf("a committed region must still surface the outcome: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
				}
			} else if rec.IsError() {
				t.Fatalf("record must not be an error: %+v", rec)
			}
		})
	}
}

// TestToolCallFramePastTheRelayGuardrailIsRelayedWholeAndTerminatedOnce covers
// the case the previous test deliberately leaves out: a tool-call data line so
// long that the relay's newline-free guardrail fires before the line completes.
// The region must open THERE - the opener is inside the first megabyte, and a
// newline-free frame never returns to the line loop - so the tail, the finish
// chunk and the terminator are all withheld and released once. Before the fix
// the region never opened, the client received the call AND a second
// `data: [DONE]` appended behind a substituted empty_tool_call error, on a
// record flagged as a failed request.
func TestToolCallFramePastTheRelayGuardrailIsRelayedWholeAndTerminatedOnce(t *testing.T) {
	for _, c := range []struct {
		name      string
		tail      string
		wantTrunc bool
	}{
		{name: "terminated", tail: atomicStopFrame + atomicDoneFrame},
		{name: "truncated", tail: "", wantTrunc: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			frame := atomicToolCallWithArgs(strings.Repeat("x", guardrailArgs), "call_1", "bash", false)
			if len(frame) <= sseScannerBufMax {
				t.Fatalf("the fixture must outgrow the relay's line guardrail, got %d bytes", len(frame))
			}
			got, calls, rec := atomicStream(t, atomicRoleFrame+frame+c.tail)

			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: the call reached the wire, so no rescue may follow it", calls)
			}
			if n := toolCallCount(got); n != 1 {
				t.Fatalf("client saw %d tool-call deltas, want 1: the guardrail relays the frame, it does not drop it", n)
			}
			if n := strings.Count(got, "data: [DONE]"); n != 1 {
				t.Fatalf("client saw %d [DONE] markers, want exactly 1: a second one lands behind whatever the relay then decided", n)
			}
			if strings.Contains(got, metrics.CodeEmptyToolCall) || strings.Contains(got, metrics.CodeEmptyCompletion) {
				t.Fatalf("no void may be claimed for a call the client received: %s", truncateForMsg(got))
			}
			// The count is the documented under-count direction: an array that
			// spans the guardrail boundary belongs to neither producer, so the
			// record reports no call rather than inventing one. What the void
			// rule protects is exactly this state.
			if rec.ToolCalls != 0 {
				t.Fatalf("record ToolCalls = %d, want 0: neither producer can reassemble a frame split at the guardrail", rec.ToolCalls)
			}
			if surfaced, want := strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`), c.wantTrunc; surfaced != want {
				t.Fatalf("in-band truncation surfaced = %v, want %v", surfaced, want)
			}
			if c.wantTrunc {
				if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
					t.Fatalf("a committed region must still surface the outcome: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
				}
			} else if rec.IsError() {
				t.Fatalf("record must not be an error: %+v", rec)
			}
		})
	}
}
