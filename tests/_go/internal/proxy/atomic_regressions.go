package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Two regressions found by the fourth and fifth reviews, at the DEFAULT
// configuration - both pre-existing, both about a fact having two owners.

// TestSplitToolCallIsNotAlsoReportedAsEmptyToolCall pins the event-level owner
// of tool-call accounting. A tool_calls array is legal SSE spread across
// several data lines of ONE event. A per-line scan cannot see it, so the relay
// used to relay a complete, valid tool call to the client AND simultaneously
// classify the stream as the empty_tool_call degenerate class, substituting an
// error claiming no tool was called. The record said IsError with a tool call
// in it, and the client got a tool call and an error.
func TestSplitToolCallIsNotAlsoReportedAsEmptyToolCall(t *testing.T) {
	split := `data: {"id":"A","choices":[{"delta":{"tool_calls":` + "\n" +
		`data: [{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{\"command\":\"ls -la\"}"}}]}}]}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + split + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(50)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call deltas, want 1: the split call must be relayed", n)
	}
	if strings.Contains(got, `"empty_tool_call"`) {
		t.Fatalf("a stream that DID call a tool must not be substituted with empty_tool_call: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].IsError() {
		t.Fatalf("record must not be an error: %+v", recs[0])
	}
	if recs[0].ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1", recs[0].ToolCalls)
	}
	if len(recs[0].ToolNames) != 1 || recs[0].ToolNames[0] != "bash" {
		t.Fatalf("record ToolNames = %v, want [bash]", recs[0].ToolNames)
	}
}

// TestDefaultsReportEveryTruncationAndCompletion is the guard on the promise
// this whole branch is built around: with both settings at their defaults the
// relay's classification and its bytes are exactly the pre-feature behavior.
// An earlier commit added a bail-out that fired on the TERMINAL withhold region
// - which has nothing to do with atomic tool calls - and silently turned
// truncated streams into clean successes with the finish chunk dropped.
func TestDefaultsReportEveryTruncationAndCompletion(t *testing.T) {
	cases := []struct {
		name      string
		wire      string
		wantError string
	}{
		{
			name: "role only, no terminator",
			wire: atomicRoleFrame,
			// No terminator at all, so this is a truncation whatever else it is.
			wantError: metrics.CodeTruncated,
		},
		{
			name:      "finish_reason split across data lines, no [DONE]",
			wire:      atomicRoleFrame + `data: {"id":"A","choices":[{"delta":{},` + "\n" + `data: "finish_reason":"stop"}]}` + "\n\n",
			wantError: metrics.CodeEmptyCompletion,
		},
		{
			name:      "answer then truncation",
			wire:      atomicRoleFrame + atomicTextFrame,
			wantError: metrics.CodeTruncated,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.Write([]byte(c.wire))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(50)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			_, got := streamRequest(t, srv, upstream)
			if !strings.Contains(got, `"code":"`+c.wantError+`"`) {
				t.Fatalf("the %s class must be surfaced in-band at defaults, got: %s", c.wantError, got)
			}
			recs := waitForRecord(t, buf, 1)
			if !recs[0].IsError() || recs[0].ErrorCode != c.wantError {
				t.Fatalf("record must carry %s, got IsError=%v code=%q", c.wantError, recs[0].IsError(), recs[0].ErrorCode)
			}
		})
	}
}

// Regressions found in review, all about the same two facts: the bound of the
// withhold region a tool call opens, and the single owner of "how many distinct
// tool calls has this stream made".

// oversizedArgs sizes a tool-call `arguments` string so the data line carrying
// it lands between the analyzer's per-event buffer cap (1 MiB) and the relay's
// scanner line guardrail (1 MiB): the frame is still whole and legal, but no
// single raw line reaches the analyzer with the array intact after the event
// buffer has dropped it. Shared by every fixture that needs that window.
const oversizedArgs = 1<<20 + 2048 - 200

// atomicToolCallWithArgs builds a tool-call frame whose `arguments` string is
// args bytes, in the shape a provider sends it: one data line, or the two-line
// split the withhold region has to open on the first of.
func atomicToolCallWithArgs(args, id, name string, split bool) string {
	entry := `{"index":0,"id":"` + id + `","function":{"name":"` + name + `","arguments":"` + args + `"}}`
	prefix := `data: {"id":"A","choices":[{"delta":{"tool_calls":[`
	if split {
		return prefix + "\n" + "data: " + entry + `]}}]}` + "\n\n"
	}
	return prefix + entry + `]}}]}` + "\n\n"
}

// atomicStream serves wire once and returns what the client received plus the
// finalized record, with atomic tool calls on.
func atomicStream(t *testing.T, wire string) (string, *metrics.Record) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(wire))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.AtomicToolCalls = true
	buf := metrics.NewBuffer(50)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()
	_, got := streamRequest(t, srv, upstream)
	return got, waitForRecord(t, buf, 1)[0]
}

// TestToolCallLargerThanTheTerminalCapIsStillWithheld pins the bound of the
// region a tool call OPENS. The relay keeps two bounds - 64 KiB for a terminal
// region, 1 MiB for one a tool call opens - and the larger one has to be chosen
// on the line that opens the region, not only raised for a tool call that turns
// up later inside a region some other line opened. When it was not, a tool call
// between the two caps overflowed the smaller bound, committed verbatim (the
// uncommitted-call-on-the-wire hazard the setting exists to remove) and left
// the record a silent success, because a committed region suppresses the
// degenerate classification. Measured before the fix at 70 KiB of arguments:
// ToolCalls=0, the call on the wire, ErrorCode="" and IsError=false.
func TestToolCallLargerThanTheTerminalCapIsStillWithheld(t *testing.T) {
	// Comfortably past the terminal cap, comfortably inside the tool-call cap,
	// so only a correct bound choice withholds it.
	const argsSize = 128 * 1024
	if argsSize <= 64*1024 || argsSize >= 1<<20 {
		t.Fatalf("the fixture must sit between the two hold bounds, got %d", argsSize)
	}
	frame := atomicToolCallWithArgs(strings.Repeat("x", argsSize), "call_1", "bash", true)
	// No terminator: the region can only be dropped, never released.
	got, rec := atomicStream(t, atomicRoleFrame+atomicTextFrame+frame)

	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call deltas, want 0: a %d-byte tool call exceeded the withhold region and was committed uncommitted: %s",
			n, argsSize, truncateForMsg(got))
	}
	if !strings.Contains(got, atomicTextFrame) {
		t.Fatalf("already-relayed answer content must still reach the client: %s", truncateForMsg(got))
	}
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation, not be a silent success: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band: %s", truncateForMsg(got))
	}
	// The record counts what the provider sent, so the withheld call is still
	// accounted for even though the client never received it.
	if rec.ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1: the withheld call the provider sent must still be counted", rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "bash" {
		t.Fatalf("record ToolNames = %v, want [bash]", rec.ToolNames)
	}
}

// TestOversizedToolCallDataLineIsCountedAndRelayed pins the accounting
// fallback for a data line the analyzer's per-EVENT buffer cannot hold. The
// event buffer refuses to grow past 1 MiB, and a single tool-call frame can
// exceed that, so the call is invisible to the event-level counter - which then
// classified the stream as the empty_tool_call degenerate class and REPLACED a
// complete, valid tool call with an error claiming no tool was called. The raw
// data line is the producer that still sees it. The frame sits in the narrow
// window where it is still whole and legal: past the scanner bound a frame is
// newline-free and the relay's guardrail relays it, which the sibling test
// covers.
func TestOversizedToolCallDataLineIsCountedAndRelayed(t *testing.T) {
	if oversizedArgs <= 1<<20 {
		t.Fatalf("the fixture must sit past the analyzer's event buffer cap, got %d", oversizedArgs)
	}
	frame := atomicToolCallWithArgs(strings.Repeat("x", oversizedArgs), "call_1", "bash", false)
	got, rec := atomicStream(t, atomicRoleFrame+frame+atomicStopFrame+atomicDoneFrame)

	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call deltas, want 1: a complete, valid tool call must never be destroyed: %s", n, truncateForMsg(got))
	}
	if strings.Contains(got, metrics.CodeEmptyToolCall) {
		t.Fatalf("a stream that DID call a tool must not be substituted with %s: %s", metrics.CodeEmptyToolCall, truncateForMsg(got))
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1: the analyzer dropped this data line, so the per-line producer is the only one that saw the call", rec.ToolCalls)
	}
	if rec.IsError() {
		t.Fatalf("record must not be an error: %+v", rec)
	}
}

// TestAtomicFunctionCallIsWithheldAndCounted pins the deprecated function_call
// shape in both wirings. It is a tool call in the older shape, so it must be
// counted (the counter used to set the withhold flag and then count nothing,
// which made the relay substitute empty_tool_call over a call it had just
// withheld - a completed tool call destroyed), and a function_call key whose
// value continues on the next data line must open the withhold region exactly
// as a tool_calls one does. Without that second half the same logical shape was
// withheld or not depending on where the line break happened to fall.
func TestAtomicFunctionCallIsWithheldAndCounted(t *testing.T) {
	cases := []struct {
		name  string
		frame string
	}{
		{"single data line", `data: {"id":"A","choices":[{"delta":{"function_call":{"name":"lookup","arguments":"{}"}}]}` + "\n\n"},
		{"split across two data lines", `data: {"id":"A","choices":[{"delta":{"function_call":` + "\n" +
			`data: {"name":"lookup","arguments":"{}"}}]}` + "\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Terminated run: the call is withheld, then released verbatim.
			got, rec := atomicStream(t, atomicRoleFrame+c.frame+atomicStopFrame+atomicDoneFrame)
			if !strings.Contains(got, `"function_call"`) {
				t.Fatalf("the terminator must release the function_call verbatim: %s", got)
			}
			if strings.Contains(got, metrics.CodeEmptyToolCall) || strings.Contains(got, metrics.CodeEmptyCompletion) {
				t.Fatalf("a stream that DID call a function must not be substituted: %s", got)
			}
			if rec.ToolCalls != 1 {
				t.Fatalf("record ToolCalls = %d, want 1: a deprecated function_call is a tool call", rec.ToolCalls)
			}
			if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
				t.Fatalf("record ToolNames = %v, want [lookup]", rec.ToolNames)
			}

			// Truncated run: withheld and discarded, never substituted.
			got, rec = atomicStream(t, atomicRoleFrame+c.frame)
			if strings.Contains(got, `"function_call"`) {
				t.Fatalf("an uncommitted function_call must never reach the client: %s", truncateForMsg(got))
			}
			if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record must carry the truncation: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
			}
			if rec.ToolCalls != 1 {
				t.Fatalf("record ToolCalls = %d, want 1", rec.ToolCalls)
			}
		})
	}
}

// TestOversizedFunctionCallDataLineIsCountedAndRelayed is the deprecated shape
// in the same window as TestOversizedToolCallDataLineIsCountedAndRelayed: a
// data line the analyzer's per-event buffer cannot hold, so the function_call
// is invisible to the event-level counter and has to be counted from the raw
// line. Without that the relay substitutes empty_tool_call over a complete,
// valid call the client never receives - the same destruction the tool_calls
// shape suffered.
func TestOversizedFunctionCallDataLineIsCountedAndRelayed(t *testing.T) {
	big := strings.Repeat("x", oversizedArgs)
	frame := `data: {"id":"A","choices":[{"delta":{"function_call":{"name":"lookup","arguments":"` + big + `"}}}]}` + "\n\n"
	got, rec := atomicStream(t, atomicRoleFrame+frame+atomicStopFrame+atomicDoneFrame)

	if !strings.Contains(got, `"function_call"`) {
		t.Fatalf("a complete, valid function_call must reach the client: %s", truncateForMsg(got))
	}
	if strings.Contains(got, metrics.CodeEmptyToolCall) {
		t.Fatalf("a stream that DID call a function must not be substituted with %s: %s", metrics.CodeEmptyToolCall, truncateForMsg(got))
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1: the analyzer dropped this data line, so the per-line producer is the only one that saw the call", rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
		t.Fatalf("record ToolNames = %v, want [lookup]", rec.ToolNames)
	}
	if rec.IsError() {
		t.Fatalf("record must not be an error: %+v", rec)
	}
}

// TestTwoToolCallArraysInOneEventAreBothCounted pins the event-level scan. One
// SSE event may legally carry two tool_calls arrays - a gateway that drops the
// blank line between two tool-call frames merges them - and a scan that reads
// only the first occurrence counted one call and lost the other. Measured
// against the shipped testing branch: 2; against the change under review: 1.
//
// The "split" case is the one that isolates the event counter: when neither
// array is whole on a single data line, the per-line scan cannot see either
// call, so only the reassembled event can account for them.
func TestTwoToolCallArraysInOneEventAreBothCounted(t *testing.T) {
	cases := []struct {
		name string
		wire string
	}{
		{name: "both arrays whole on one data line", wire: `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n" +
			`data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"ls","arguments":"{}"}}]}}]}` + "\n\n"},
		{name: "both arrays split across two data lines", wire: `data: {"id":"A","choices":[{"delta":{"tool_calls":[` + "\n" +
			`data: {"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n" +
			`data: {"id":"A","choices":[{"delta":{"tool_calls":[` + "\n" +
			`data: {"index":1,"id":"call_2","function":{"name":"ls","arguments":"{}"}}]}}]}` + "\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rec := atomicStream(t, atomicRoleFrame+c.wire+atomicStopFrame+atomicDoneFrame)

			if n := toolCallCount(got); n != 2 {
				t.Fatalf("client saw %d tool-call deltas, want 2: %s", n, truncateForMsg(got))
			}
			if rec.ToolCalls != 2 {
				t.Fatalf("record ToolCalls = %d, want 2: every tool-call array in an event is counted, not just the first", rec.ToolCalls)
			}
			if len(rec.ToolNames) != 2 || rec.ToolNames[0] != "bash" || rec.ToolNames[1] != "ls" {
				t.Fatalf("record ToolNames = %v, want [bash ls]", rec.ToolNames)
			}
			if rec.IsError() {
				t.Fatalf("record must not be an error: %+v", rec)
			}
		})
	}
}
