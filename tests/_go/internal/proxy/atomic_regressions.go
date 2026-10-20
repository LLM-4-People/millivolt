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

// oversizedArgs sizes a tool-call `arguments` string so the ONE data line
// carrying it is past the analyzer's per-event buffer cap (eventDataMax) while
// still short enough for the relay's newline-free guardrail (sseScannerBufMax)
// to deliver it whole: the frame is legal and complete, but the event buffer
// dropped it, so the raw-line producer is the only one that can see the call.
// The two caps are the same size, so there is no "window between them" - the
// reach that matters is the guardrail's slack, roughly one read buffer past the
// event cap. Past that guardrail the relay itself relays the accumulated
// bytes, which no producer can reassemble: the sibling test covers it.
const oversizedArgs = 1<<20 + 2048 - 200

// guardrailArgs sizes a tool-call `arguments` string so its data line stays
// newline-free long enough for the relay's guardrail to fire BEFORE the line
// completes: the line is then delivered in fragments, so no producer can ever
// reassemble the call.
const guardrailArgs = toolCallHoldMax + 8192

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

// atomicStream serves wire once and returns what the client received, how many
// upstream attempts it took, and the finalized record - with atomic tool calls
// on. The attempt count is part of every wire assertion here: a re-send behind
// bytes the client already holds is the failure mode this setting exists to
// prevent, and it is invisible in the body alone.
func atomicStream(t *testing.T, wire string) (string, int32, *metrics.Record) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
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
	return got, calls.Load(), waitForRecord(t, buf, 1)[0]
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
	got, calls, rec := atomicStream(t, atomicRoleFrame+atomicTextFrame+frame)

	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1: a withheld region is never re-sent", calls)
	}
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
			got, calls, rec := atomicStream(t, atomicRoleFrame+c.frame+atomicStopFrame+atomicDoneFrame)
			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: a healthy terminated stream is never re-sent", calls)
			}
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
			// The truncated run may be rescued (nothing reached the wire, so
			// a fresh attempt is invisible); what it must never do is hand the
			// client the withheld call.
			got, _, rec = atomicStream(t, atomicRoleFrame+c.frame)
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

// TestTwoToolCallArraysInOneEventAreBothCounted pins the event-level scan. One
// SSE event may legally carry two tool_calls arrays - a gateway that drops the
// blank line between two tool-call frames merges them - and a scan that reads
// only the first occurrence counted one call and lost the other.
//
// The two rows measure different producers, and the baseline differs between
// them, so both are here rather than one contrast. "Whole on one data line":
// both arrays reach the per-line producer, and the shipped testing branch
// measures 2. "Split across two data lines": neither array is whole on a single
// line, so only the reassembled event can see either call - and there testing
// measures 0, which is how a complete tool call used to be relayed while the
// stream was classified as the empty_tool_call void (see
// TestSplitToolCallIsNotAlsoReportedAsEmptyToolCall).
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
			got, calls, rec := atomicStream(t, atomicRoleFrame+c.wire+atomicStopFrame+atomicDoneFrame)

			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: a healthy terminated stream is never re-sent", calls)
			}
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

// The soundness holes a later review measured. Every one of them is a case where
// a tool call reached the client - or a rescue landed behind it - on a path the
// setting claims to have closed.

// splitToolCallAfterKey splits a tool_calls frame immediately after the key's
// colon, the shape a gateway emits when it breaks a long arguments value across
// data lines. Reassembled per the SSE spec it is a normal, valid tool call, so
// the client's own parser sees one and executes it.
func splitToolCallAfterKey(args string) string {
	return `data: {"id":"A","choices":[{"delta":{"tool_calls":` + "\n" +
		`data: [{"index":0,"id":"call_1","function":{"name":"bash","arguments":"` + args + `"}}]}}]}` + "\n\n"
}

// TestCommittedToolCallRegionIsSurfacedAndNeverResent pins the wire-safety
// consequence of a withhold region that outgrew its bound. The overflow path
// wrote the region's bytes to the client WITHOUT feeding them to the analyzer,
// so the analyzer still believed the client had received nothing content-bearing
// and the quality rescue re-sent the whole request ON THE SAME COMMITTED
// CONNECTION - the same-index argument merge the whole feature exists to
// prevent, reachable in the shipped configuration (quality_retries: 1).
// Measured before the fix with the fixture below: upstream attempts 2, two
// tool-call frames on the wire, and a record with IsError=false, ErrorCode=""
// and Retries=1 - a clean success for a stream that failed.
//
// The outcome now is the only honest one left: the call is on the wire, so
// nothing may re-send behind it, and the truncation is surfaced in-band after
// the committed bytes.
func TestCommittedToolCallRegionIsSurfacedAndNeverResent(t *testing.T) {
	frame := splitToolCallAfterKey(strings.Repeat("x", guardrailArgs))
	if len(frame) <= sseScannerBufMax {
		t.Fatalf("the fixture must outgrow the withhold region, got %d bytes", len(frame))
	}
	got, calls, rec := atomicStream(t, atomicRoleFrame+frame)

	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1: a region already on the wire must never be re-sent behind", calls)
	}
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call deltas, want the one the region committed", n)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band after the committed bytes: %s", truncateForMsg(got))
	}
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation, not a silent success: IsError=%v ErrorCode=%q Retries=%d",
			rec.IsError(), rec.ErrorCode, rec.Retries)
	}
	if rec.Retries != 0 {
		t.Fatalf("record Retries = %d, want 0: no attempt was absorbed", rec.Retries)
	}
}

// TestToolCallWithNoUsableIdIsStillACall pins the identity rule where it used to
// destroy real calls. A gateway that omits a tool call's id, sends it empty, or
// serializes a lone call as an object instead of an array produced a call that
// was never counted, so the classifier called the stream the empty_tool_call
// void and the relay REPLACED the call with that error - a call destroyed, and
// an error class clients treat as retryable, so a gateway that omits ids put the
// client in a permanent retry loop. A client keys a streamed tool call by its
// array index, so an entry carrying an index is a call whatever its id says.
func TestToolCallWithNoUsableIdIsStillACall(t *testing.T) {
	cases := []struct {
		name  string
		frame string
	}{
		{"empty id", `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n\n"},
		{"no id key", `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n\n"},
		{"tool_calls as an object", `data: {"id":"A","choices":[{"delta":{"tool_calls":{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}}}]}` + "\n\n"},
	}
	for _, c := range cases {
		// Terminated: the region is released verbatim, and the call survives.
		t.Run(c.name+" terminated", func(t *testing.T) {
			got, calls, rec := atomicStream(t, atomicRoleFrame+c.frame+atomicStopFrame+atomicDoneFrame)
			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: a terminated stream is never re-sent", calls)
			}
			if n := toolCallCount(got); n != 1 {
				t.Fatalf("client saw %d tool-call deltas, want 1: a counted call must never be destroyed: %s",
					n, truncateForMsg(got))
			}
			if strings.Contains(got, metrics.CodeEmptyToolCall) {
				t.Fatalf("a stream that DID call a tool must not be substituted with %s: %s",
					metrics.CodeEmptyToolCall, truncateForMsg(got))
			}
			if rec.ToolCalls != 1 {
				t.Fatalf("record ToolCalls = %d, want 1: the call's identity is its index, not its id", rec.ToolCalls)
			}
			if rec.IsError() {
				t.Fatalf("record must not be an error: %+v", rec)
			}
		})
		// Truncated: the same shape must also OPEN the withhold region, which is
		// the other half of "a real call". A shape that opens no region is
		// relayed uncommitted - the object value, in particular, is invisible to
		// the array-shaped opener the withholding keys on.
		t.Run(c.name+" truncated", func(t *testing.T) {
			got, _, rec := atomicStream(t, atomicRoleFrame+c.frame)
			if n := toolCallCount(got); n != 0 {
				t.Fatalf("client saw %d tool-call deltas, want 0: an unterminated stream's call must never be relayed: %s",
					n, truncateForMsg(got))
			}
			if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
				t.Fatalf("the truncation must surface in-band: %s", truncateForMsg(got))
			}
			if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record must carry the truncation: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
			}
			if rec.ToolCalls != 1 {
				t.Fatalf("record ToolCalls = %d, want 1: the provider did call a tool", rec.ToolCalls)
			}
		})
	}
}

// TestTwoCallsSharingOneIdAreTwoCalls pins the identity rule on the DEFAULT
// path, where no setting is involved and the count is purely informational. A
// gateway that reuses one id for two calls at different indices - Anthropic
// handing two tool_use blocks the same id, say - used to be de-duplicated to a
// single call, so Record.ToolCalls, the dashboard's `tools:` and
// llm_proxy_tool_calls_total all under-reported a stream the client executes
// twice. Measured against the shipped testing branch: ToolCalls=2. Against
// de-duplicating by id: 1.
func TestTwoCallsSharingOneIdAreTwoCalls(t *testing.T) {
	wire := `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"shared","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n" +
		`data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":1,"id":"shared","function":{"name":"ls","arguments":"{}"}}]}}]}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + wire + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(50)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()
	_, got := streamRequest(t, srv, upstream)

	if n := toolCallCount(got); n != 2 {
		t.Fatalf("client saw %d tool-call deltas, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].ToolCalls != 2 {
		t.Fatalf("record ToolCalls = %d, want 2: two indices are two calls even with one id", recs[0].ToolCalls)
	}
	if len(recs[0].ToolNames) != 2 || recs[0].ToolNames[0] != "bash" || recs[0].ToolNames[1] != "ls" {
		t.Fatalf("record ToolNames = %v, want [bash ls]", recs[0].ToolNames)
	}
	if recs[0].IsError() {
		t.Fatalf("record must not be an error: %+v", recs[0])
	}
}

// TestTrailingPartialToolCallIsWithheld pins the other path that never sees a
// newline: an upstream that closes the body mid-event, with the whole tool call
// still in the unterminated final line. That line is byte-scanned like any
// other fragment, so it can reveal the tool call - and when it does the region
// must open there. Released instead, the client received an uncommitted call
// from a stream that never terminated, which is the whole hazard.
func TestTrailingPartialToolCallIsWithheld(t *testing.T) {
	// No trailing newline: the body ends inside the tool call's data line.
	wire := atomicRoleFrame + `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}`
	got, calls, rec := atomicStream(t, wire)

	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1: nothing reached the wire, so at most the rescue budget decides", calls)
	}
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call deltas, want 0: an unterminated stream's tool call must never be relayed: %s",
			n, truncateForMsg(got))
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band: %s", truncateForMsg(got))
	}
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
	}
}

// TestGuardrailCommittedToolCallIsNeverRescued is the wire-safety half of the
// guardrail path, against BOTH budgets. A tool-call delta larger than the
// relay's newline-free line guardrail is relayed by the guardrail itself, in
// fragments - so the region opens THERE and commits, and the relay has to carry
// that as a fact of the wire. Neither rescue may then append a fresh attempt
// behind those bytes: the second attempt's frames would land behind a tool call
// the client already holds, which is the same-index argument merge the setting
// exists to prevent, and the analyzer cannot see the call well enough to stop it
// (a frame split at the guardrail belongs to neither producer).
//
// Measured before the fix, on the quality budget's in-band-error shape: upstream
// attempts 2, two tool-call frames, Retries=1.
func TestGuardrailCommittedToolCallIsNeverRescued(t *testing.T) {
	frame := atomicToolCallWithArgs(strings.Repeat("x", guardrailArgs), "call_1", "bash", false)
	if len(frame) <= sseScannerBufMax {
		t.Fatalf("the fixture must outgrow the relay's line guardrail, got %d bytes", len(frame))
	}
	reasoning := `data: {"id":"A","choices":[{"delta":{"reasoning":"weighing"}}]}` + "\n\n"
	cases := []struct {
		name    string
		wire    string
		wantErr string
	}{
		{
			// A retryable server-availability error as its own event's first
			// data line: the shape the quality budget drops when it believes
			// nothing content-bearing was seen.
			name:    "quality budget, retryable in-band error",
			wire:    frame + `data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}` + "\n\n",
			wantErr: "overloaded_error",
		},
		{
			// Reasoning-only output before the call, then a truncation: the
			// shape the thinking budget re-sends behind.
			name:    "thinking budget, reasoning-only truncation",
			wire:    reasoning + frame,
			wantErr: metrics.CodeTruncated,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, calls, rec := atomicStream(t, atomicRoleFrame+c.wire)

			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: bytes the guardrail already relayed must block every rescue", calls)
			}
			if n := toolCallCount(got); n != 1 {
				t.Fatalf("client saw %d tool-call deltas, want the one the guardrail relayed", n)
			}
			if !strings.Contains(got, c.wantErr) {
				t.Fatalf("with no rescue available the stream's own failure must reach the client: %s", truncateForMsg(got))
			}
			if rec.Retries != 0 {
				t.Fatalf("record Retries = %d, want 0: no attempt was absorbed", rec.Retries)
			}
		})
	}
}

// TestAtomicWithholdingAppliesToANonChatlikeFeed pins that the withhold
// decision does not depend on the chatlike gate the degenerate classifier uses.
// A feed that is not recognizably chat-completions classifies as healthy no
// matter what it carried, so the withheld region used to be RELEASED verbatim on
// the way out: the client received the uncommitted tool call and the record was
// a clean success, byte-identical to a deployment with the setting off. The
// setting has to mean one thing, so a tool-call region that never saw a
// terminator is a truncation whatever the feed looks like.
func TestAtomicWithholdingAppliesToANonChatlikeFeed(t *testing.T) {
	wire := `data: {"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}` + "\n\n"
	got, calls, rec := atomicStream(t, wire)

	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", calls)
	}
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call deltas, want 0: an uncommitted call must never be relayed: %s",
			n, truncateForMsg(got))
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the unterminated stream must surface in-band: %s", truncateForMsg(got))
	}
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation, not a clean success: IsError=%v ErrorCode=%q",
			rec.IsError(), rec.ErrorCode)
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1: the provider did call a tool", rec.ToolCalls)
	}
	// The contrast that makes it non-vacuous: the SAME feed with the setting off
	// keeps the pre-feature behavior, the compatibility cost the setting is
	// opt-in for.
	offWire, offCalls, offRec := streamWithAtomic(t, wire, false)
	if offCalls != 1 {
		t.Fatalf("default upstream attempts = %d, want 1", offCalls)
	}
	if n := toolCallCount(offWire); n != 1 {
		t.Fatalf("with the setting off the call must still reach the client, saw %d frames: %s", n, truncateForMsg(offWire))
	}
	// The request shape still seeds the canonical classifier: an unterminated
	// chat request is a truncation with or without atomic withholding. The
	// setting only decides whether the uncommitted call reaches the wire.
	if !offRec.IsError() || offRec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("with the setting off the record keeps its canonical classification: IsError=%v ErrorCode=%q",
			offRec.IsError(), offRec.ErrorCode)
	}
}

// streamWithAtomic serves wire once with atomic tool calls explicitly on or off
// and returns what the client received, the upstream attempt count and the
// record. The one owner of this fixture's shape.
func streamWithAtomic(t *testing.T, wire string, atomicOn bool) (string, int32, *metrics.Record) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(wire))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.AtomicToolCalls = atomicOn
	buf := metrics.NewBuffer(50)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()
	_, got := streamRequest(t, srv, upstream)
	return got, calls.Load(), waitForRecord(t, buf, 1)[0]
}
