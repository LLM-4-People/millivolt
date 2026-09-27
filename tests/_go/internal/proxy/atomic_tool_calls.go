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

// toolCallCount counts the tool-call DELTAS the client actually received, so a
// test can assert it never saw an uncommitted one. It keys on the delta's
// function payload, never on the bare "tool_calls" token, which also appears in
// the finish chunk's finish_reason.
func toolCallCount(wire string) int {
	return strings.Count(wire, `"function":{"name"`)
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

// TestAtomicToolCallOversizedFrameCommitsVerbatim pins the memory bounds a
// single tool-call frame can exceed, and what each one means. A frame whose
// data line lands between the analyzer's per-EVENT buffer cap and the relay's
// scanner line guardrail is still whole and legal: the event counter silently
// dropped the line, so accounting must fall back to the raw data line or the
// record claims no tool call for a stream the client just received one from.
// A frame past the scanner guardrail is newline-free for long enough that the
// guardrail relays it before any withhold region can open, and the truncation
// still surfaces in-band because the region never opened at all.
//
// The two cases differ in whether the in-band truncation appears, and that is
// the design, not an accident: the first case overflows the tool-call hold
// bound, and an overflow is commit-to-verbatim mode (nothing may be
// substituted on top of bytes the client already has), while the second never
// opens a region.
func TestAtomicToolCallOversizedFrameCommitsVerbatim(t *testing.T) {
	cases := []struct {
		name      string
		argsSize  int
		wantTrunc bool
		wantCount int
	}{
		// eventDataMax (the analyzer's event buffer) and sseScannerBufMax (the
		// relay's line guardrail) are both 1 MiB, so the window between them is
		// the scanner's own slack; land in the middle of it. The call is whole
		// there, so a producer can see it.
		{name: "whole frame past the event buffer cap", argsSize: oversizedArgs, wantCount: 1},
		// Past the guardrail the relay splits the frame mid-line, so NO producer
		// can see a whole call: neither the raw line (there is none) nor the
		// event buffer (too large). That is a known, documented limit of the
		// guardrail, not something this test can close; what it does guarantee
		// is that the client still receives the call verbatim and the
		// truncation still surfaces.
		{name: "newline-free frame past the scanner guardrail", argsSize: toolCallHoldMax + 4096, wantTrunc: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.argsSize <= 1<<20 {
				t.Fatalf("the fixture must sit past the analyzer's event buffer cap, got %d", c.argsSize)
			}
			var calls atomic.Int32
			huge := strings.Repeat("x", c.argsSize)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.Write([]byte(atomicRoleFrame + atomicTextFrame))
				w.(http.Flusher).Flush()
				// One enormous tool-call argument frame: past the analyzer's
				// event buffer; past the scanner's line guardrail too in the
				// second case.
				w.Write([]byte(`data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"` + huge + `"}}]}}]}` + "\n\n"))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			cfg := config.Default()
			cfg.AtomicToolCalls = true
			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			_, got := streamRequest(t, srv, upstream)
			if n := toolCallCount(got); n != 1 {
				t.Fatalf("an oversized tool call must commit verbatim, saw %d frames", n)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("upstream calls = %d, want 1 (the call reached the wire, so no rescue can follow it)", n)
			}
			truncSurfaced := strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`)
			if truncSurfaced != c.wantTrunc {
				t.Fatalf("in-band truncation surfaced = %v, want %v", truncSurfaced, c.wantTrunc)
			}
			recs := waitForRecord(t, buf, 1)
			if recs[0].ToolCalls != c.wantCount {
				t.Fatalf("record ToolCalls = %d, want %d: the analyzer dropped this data line, so the per-line producer is the only one that saw the call", recs[0].ToolCalls, c.wantCount)
			}
			if c.wantCount == 1 && (len(recs[0].ToolNames) != 1 || recs[0].ToolNames[0] != "bash") {
				t.Fatalf("record ToolNames = %v, want [bash]", recs[0].ToolNames)
			}
		})
	}
}
