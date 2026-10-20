package sse

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Tool-call identity, its bound, and the classification rules that depend on
// them. The identity rule is the load-bearing one: a client keys a streamed
// tool call by its array index, so the index - not the id - is what the record,
// the degenerate classifier and the atomic withhold region follow.

// TestToolCallIdentityIsTheIndex pins the identity rule in both directions.
// De-duplicating by id under-reports two distinct calls a gateway gave the same
// id (and does so on the DEFAULT path, where no setting is involved), while
// keying by index keeps an argument-only continuation at a seen index counted
// once. An entry whose id is empty or absent is still a call whenever it
// carries an index: dropping it made the classifier call the stream the
// empty_tool_call void, which the relay answers by replacing a real tool call
// with a retryable error.
func TestToolCallIdentityIsTheIndex(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  int
	}{
		{"one call with an id", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"bash"}}]}}]}`,
		}, 1},
		{"two calls sharing one id are two calls", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"same","function":{"name":"bash"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"same","function":{"name":"ls"}}]}}]}`,
		}, 2},
		{"an empty id is still a call", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"bash","arguments":"{}"}}]}}]}`,
		}, 1},
		{"an absent id is still a call", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"{}"}}]}}]}`,
		}, 1},
		{"tool_calls as an object is one call", []string{
			`data: {"choices":[{"delta":{"tool_calls":{"index":0,"id":"c0","function":{"name":"bash","arguments":"{}"}}}}]}`,
		}, 1},
		{"an argument-only continuation is the same call", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"bash","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		}, 1},
		{"two objects in one event are two calls", []string{
			`data: {"choices":[{"delta":{"tool_calls":{"index":0,"id":"c0","function":{"name":"bash"}}}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":{"index":1,"id":"c1","function":{"name":"ls"}}}}]}`,
		}, 2},
		{"an entry with neither index nor id contributes nothing", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"x"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"x"}}]}}]}`,
		}, 0},
		{"an entry with no index falls back to its id", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"c0","function":{"name":"bash"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"ls"}}]}}]}`,
		}, 2},
		{"an out-of-range index falls back to its id", []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":100000,"id":"c0","function":{"name":"bash"}}]}}]}`,
		}, 1},
		{"an empty array is no call", []string{
			`data: {"choices":[{"delta":{"tool_calls":[]}}]}`,
		}, 0},
		{"the deprecated function_call shape is one call", []string{
			`data: {"choices":[{"delta":{"function_call":{"name":"lookup","arguments":"{}"}}]}`,
			`data: {"choices":[{"delta":{"function_call":{"arguments":"}"}}]}`,
		}, 1},
		{"a function_call null decoy is no call", []string{
			`data: {"choices":[{"delta":{"role":"assistant","function_call":null}}]}`,
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a Analyzer
			feedLines(&a, c.lines...)
			a.flushEvent()
			if got := a.toolCalls(); got != c.want {
				t.Fatalf("toolCalls = %d, want %d", got, c.want)
			}
		})
	}
}

// TestTrackedToolCallsAreBounded pins the bound the proxy-stream path needs.
// max_concurrent defaults to UNLIMITED, so nothing else caps what one stream's
// tool-call accounting may hold: past the limit the count stops growing, no
// further name is recorded, and the array is not decoded at all. A stream
// carrying more distinct calls than the limit is a pathological upstream, and
// reporting a saturated count beats allocating without limit.
func TestTrackedToolCallsAreBounded(t *testing.T) {
	var a Analyzer
	// One data line per call, so nothing rides on the array decode; two
	// argument-only continuations per call prove the bound is on DISTINCT calls.
	for i := 0; i < maxTrackedToolCalls+500; i++ {
		a.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":`+strconv.Itoa(i)+
			`,"id":"c`+strconv.Itoa(i)+`","function":{"name":"f`+strconv.Itoa(i%4)+`","arguments":""}}]}}]}`), time.Now())
		a.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":`+strconv.Itoa(i)+`,"function":{"arguments":"x"}}]}}]}`), time.Now())
		a.Feed(nil, time.Now())
	}
	if got := a.toolCalls(); got != maxTrackedToolCalls {
		t.Fatalf("toolCalls = %d, want the bound %d", got, maxTrackedToolCalls)
	}
	if got := len(a.toolNames); got != 4 {
		t.Fatalf("tracked tool names = %d, want the 4 distinct ones: the bound must cap names too", got)
	}
	// The deprecated shape is a call too, and is refused once the set is full.
	var b Analyzer
	for i := 0; i < maxTrackedToolCalls+10; i++ {
		b.Feed([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":`+strconv.Itoa(i)+`,"function":{"arguments":"x"}}]}}]}`), time.Now())
	}
	b.Feed([]byte(`data: {"choices":[{"delta":{"function_call":{"name":"late","arguments":"{}"}}]}`), time.Now())
	b.flushEvent()
	if got := b.toolCalls(); got != maxTrackedToolCalls {
		t.Fatalf("toolCalls after a late function_call = %d, want the bound %d", got, maxTrackedToolCalls)
	}
}

// TestUncountableToolCallIsNeverAVoid pins the classification rule for a stream
// whose tool-call bytes were seen but whose call neither producer could count -
// an array larger than the event buffer and larger than one relay line. The
// void classes assert a COUNT ("the model declared a tool and never called
// it", "nothing was produced"), and a call in flight disproves both. A
// truncation still applies: it asserts only that the stream stopped.
func TestUncountableToolCallIsNeverAVoid(t *testing.T) {
	// An array split across two data lines whose reassembly neither producer can
	// complete: the event buffer sees both halves, but the joined bytes are not
	// a valid array, so no call is countable while the bytes were plainly seen.
	uncountable := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,`,
		`data: {"function":{"name":"bash","arguments":"{}"}}]}}]}`,
	}
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"a declared tool with no countable call is not the empty_tool_call void", append(append([]string{}, uncountable...),
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `data: [DONE]`), ""},
		{"nor the empty-completion void", append(append([]string{}, uncountable...),
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, `data: [DONE]`), ""},
		{"a truncation still applies", uncountable, metrics.CodeTruncated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a Analyzer
			feedLines(&a, c.lines...)
			if got := a.OutcomeCode(); got != c.want {
				t.Fatalf("OutcomeCode = %q, want %q", got, c.want)
			}
		})
	}
	// The contrast that makes the rule non-vacuous: a stream with tool-call
	// bytes seen AND a countable call is classified by the canonical rules.
	var ok Analyzer
	feedLines(&ok,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"bash","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	if got := ok.OutcomeCode(); got != "" {
		t.Fatalf("a countable tool call classified %q, want healthy", got)
	}
}

// TestOutcomeCodeAssemblesThePendingEvent pins the flush inside OutcomeCode. A
// tool_calls array is legal SSE spread across two data lines of one event, and
// the relay classifies the stream the moment the terminator line arrives - which
// can be before the blank line that would end the pending event. Without the
// flush the reassembled array is invisible, the classifier reads zero calls and
// the relay replaces a real tool call with the empty_completion void.
func TestOutcomeCodeAssemblesThePendingEvent(t *testing.T) {
	var a Analyzer
	feedLines(&a,
		`data: {"id":"1","choices":[{"delta":{"tool_calls":`,
		`data: [{"index":0,"id":"c0","function":{"name":"bash","arguments":"{}"}}]}}]}`,
		// No blank line: the event is still pending when the terminator lands.
		`data: [DONE]`,
	)
	if got := a.OutcomeCode(); got != "" {
		t.Fatalf("OutcomeCode = %q, want healthy: the pending event's tool call must be counted before classifying", got)
	}
	if got := a.toolCalls(); got != 1 {
		t.Fatalf("toolCalls = %d, want 1", got)
	}
}

// TestRetryableStopFlushesThePendingEvent pins the flush inside
// RetryableReasoningStop: it is an exported predicate, so it must not depend on
// the caller having classified first. Without the flush a reasoning-only stop
// whose last event carried a tool call looks tool-free and would re-send behind
// a call the client received.
func TestRetryableStopFlushesThePendingEvent(t *testing.T) {
	var a Analyzer
	feedLines(&a,
		`data: {"choices":[{"delta":{"reasoning":"weighing"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":`,
		`data: [{"index":0,"id":"c0","function":{"name":"bash","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
	if a.RetryableReasoningStop() {
		t.Fatal("a pending event carrying a tool call must disqualify the stop rescue without a caller-side flush")
	}
}

// BenchmarkAnalyzerToolCallArray measures the per-stream tool-call accounting
// on one data line carrying n calls - the shape a parallel-tool-calling model
// produces. It is the guard on the proxy-stream bound: past
// maxTrackedToolCalls the set is full, so the array is not decoded at all and
// the cost stops tracking n. The count asserted is therefore min(n, bound).
func BenchmarkAnalyzerToolCallArray(b *testing.B) {
	for _, n := range []int{8, 64, 1024, 4096, 16384} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			line := toolCallArrayLine(n)
			want := min(n, maxTrackedToolCalls)
			b.SetBytes(int64(len(line)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var a Analyzer
				a.Feed(line, time.Now())
				a.Feed(nil, time.Now()) // event boundary: flush the reassembled event
				if got := a.toolCalls(); got != want {
					b.Fatalf("toolCalls = %d, want %d", got, want)
				}
			}
		})
	}
}

// toolCallArrayLine builds the one-line tool_calls frame of n calls, each
// keyed by its own index - exactly what a model emitting parallel tool calls
// streams.
func toolCallArrayLine(n int) []byte {
	var b []byte
	b = append(b, `data: {"id":"1","choices":[{"delta":{"tool_calls":[`...)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"index":`...)
		b = append(b, strconv.Itoa(i)...)
		b = append(b, `,"id":"call_`...)
		b = append(b, strconv.Itoa(i)...)
		b = append(b, `","type":"function","function":{"name":"bash","arguments":"{}"}}`...)
	}
	return append(b, `]}}]}`...)
}

// TestToolCallShapeInAnyByteRun pins the fragment producer, the third way a
// tool-call shape can reach the analyzer. Feed owns a whole `data:` line and
// returns immediately for anything else, so a call whose key sits past the
// relay's 1 MiB newline-free line guardrail - or in the trailing partial such a
// split leaves behind - was invisible: the relay then believed the client held
// no completion-contract bytes and re-sent the request behind the call it had
// already written. FeedFragment scans the raw run with the same key vocabulary,
// so the same logical call is recognized split or whole, and it must recognize
// nothing else: a null or empty decoy is what a provider's role chunk carries,
// and a fragment that over-reports one would withhold a region for a stream that
// called no tool and suppress its void class.
func TestToolCallShapeInAnyByteRun(t *testing.T) {
	const call = `"tool_calls":[{"index":0,"id":"c0","function":{"name":"bash","arguments":"{}"}}]`
	cases := []struct {
		name     string
		run      string
		wantOpen bool
		wantCall int
	}{
		{name: "whole array inside a fragment", run: `ll",` + call + `}}]}`, wantOpen: true, wantCall: 1},
		{name: "an array this run does not close", run: `ll","tool_calls":[{"index":0,"id":"c0"`, wantOpen: true},
		{name: "a lone call serialized as an object", run: `,"tool_calls":{"index":0,"id":"c0","function":{"name":"bash"}}`,
			wantOpen: true, wantCall: 1},
		{name: "a deprecated function_call", run: `,"function_call":{"name":"lookup","arguments":"{}"}}]}`,
			wantOpen: true, wantCall: 1},
		{name: "a null decoy", run: `,"tool_calls":null}}]}`},
		{name: "an empty array decoy", run: `,"tool_calls":[]}}]}`},
		{name: "an empty function_call decoy", run: `,"function_call":""}}]}`},
		{name: "content that only mentions the key in a string", run: `,"content":"see \"tool_calls\": here"}]}`},
		{name: "no tool-call key at all", run: `,"content":"hello"}}]}`},
		{
			// Feed owns a whole data line; running both producers over it would
			// decode one array twice.
			name: "a whole data line is left to Feed",
			run:  `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"bash"}}]}}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a Analyzer
			a.FeedFragment([]byte(c.run))
			if got := a.ToolCallRegionOpened(); got != c.wantOpen {
				t.Fatalf("ToolCallRegionOpened = %v, want %v for run %q", got, c.wantOpen, c.run)
			}
			rec := new(metrics.Record)
			a.Fill(rec)
			if rec.ToolCalls != c.wantCall {
				t.Fatalf("ToolCalls = %d, want %d for run %q", rec.ToolCalls, c.wantCall, c.run)
			}
			if c.wantCall == 0 && a.OutcomeCode() == metrics.CodeEmptyToolCall {
				t.Fatalf("a run with no real call must not be classified as a call: %q", c.run)
			}
		})
	}
}
