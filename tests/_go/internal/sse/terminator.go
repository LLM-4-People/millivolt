package sse

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func feedLines(a *Analyzer, lines ...string) {
	for _, l := range lines {
		a.Feed([]byte(l), time.Now())
	}
}

func TestAnalyzerTerminatorSeen(t *testing.T) {
	a := &Analyzer{}
	feedLines(a, `data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`)
	if a.Terminated() {
		t.Fatal("terminated before any marker arrived")
	}
	feedLines(a, `data: [DONE]`)
	if !a.Terminated() {
		t.Fatal("[DONE] not recorded as the terminator")
	}
}

func TestAnalyzerFinishReasonIsTerminator(t *testing.T) {
	a := &Analyzer{}
	feedLines(a,
		`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	if !a.Terminated() {
		t.Fatal("a non-null finish_reason chunk is itself a terminator")
	}
}

func TestAnalyzerResponsesTerminalEvent(t *testing.T) {
	a := &Analyzer{}
	feedLines(a,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`data: {"type":"response.completed","response":{}}`,
	)
	if !a.Terminated() {
		t.Fatal("response.completed not recorded as the terminator")
	}
	// Content-bearing: the output_text delta must prevent a void
	// classification on a healthy Responses stream.
	if a.OutcomeCode() != "" {
		t.Fatalf("healthy responses stream classified %q", a.OutcomeCode())
	}
}

func TestAnalyzerOutcomeCodes(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"healthy content stream", []string{
			`data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, ""},
		{"void stop", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, metrics.CodeEmptyCompletion},
		{"void absent finish gated on done", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: [DONE]`,
		}, metrics.CodeEmptyCompletion},
		{"tool_calls finish with zero calls", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, metrics.CodeEmptyToolCall},
		{"real tool call", []string{
			`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"x","arguments":""}}]}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}, ""},
		{"truncated: clean close without terminator", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
		}, metrics.CodeTruncated},
		{"truncated with content relayed", []string{
			`data: {"id":"1","choices":[{"delta":{"content":"half answer"}}]}`,
		}, metrics.CodeTruncated},
		{"reasoning-only stop is its own degenerate class", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking hard"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, metrics.CodeReasoningOnly},
		{"reasoning-only interruption finish is the same class", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning":"hmm"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"insufficient_system_resource"}]}`,
			`data: [DONE]`,
		}, metrics.CodeReasoningOnly},
		{"reasoning then answer is healthy", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, ""},
		{"reasoning-only length is healthy (client cap)", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"ran out"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
		}, ""},
		{"foreign stream stays clean", []string{
			`data: {"event":"custom","payload":42}`,
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Analyzer{}
			feedLines(a, c.lines...)
			if got := a.OutcomeCode(); got != c.want {
				t.Fatalf("OutcomeCode = %q, want %q", got, c.want)
			}
		})
	}
}

// TestAnalyzerRetryableTruncation gates the proxy-side re-send of a truncated
// stream: only a chatlike stream truncated before ANY content-bearing chunk
// (content, reasoning, tool call) and without a provider in-band error is
// safe to re-send - anything already on the wire must stay client-retryable.
func TestAnalyzerRetryableTruncation(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"role-only truncation is retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
		}, true},
		{"empty chatlike stream is retryable", []string{
			`data: {"id":"1","choices":[{"delta":{}}]}`,
		}, true},
		{"relayed content is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"content":"half "}}]}`,
		}, false},
		{"relayed reasoning is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning":"hmm"}}]}`,
		}, false},
		{"relayed tool call is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"x"}}]}}]}`,
		}, false},
		{"provider in-band error is not retryable", []string{
			`data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}`,
		}, false},
		{"trailing multi-line error is not retryable", []string{
			`data: {"error":`,
			`data: {"message":"died","type":"overloaded_error","code":"overloaded"}}`,
		}, false},
		{"seen terminator is not a truncation", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: [DONE]`,
		}, false},
		{"non-chatlike stream stays out", []string{
			`data: {"event":"custom","payload":42}`,
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Analyzer{}
			feedLines(a, c.lines...)
			if got := a.RetryableTruncation(); got != c.want {
				t.Fatalf("RetryableTruncation = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAnalyzerRetryableReasoningTruncation gates the mid-thinking rescue: a
// truncation after reasoning-only output IS re-sendable (the reasoning the
// client saw is auxiliary display text, not completion contract), but any
// answer-shaped byte - content, a tool call, a refusal, a deprecated
// function_call - or a Responses-shaped feed (per-response sequence numbers
// would restart mid-stream), a provider in-band error, or a seen terminator
// disqualifies it. The plain empty truncation stays on the separate quality
// predicate (TestAnalyzerRetryableTruncation).
func TestAnalyzerRetryableReasoningTruncation(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"reasoning-only truncation is retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		}, true},
		{"every reasoning spelling is retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning":"hmm"}}]}`,
		}, true},
		{"reasoning then answer is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"id":"1","choices":[{"delta":{"content":"partial answer"}}]}`,
		}, false},
		{"reasoning then tool call is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"x"}}]}}]}`,
		}, false},
		{"reasoning then refusal is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"id":"1","choices":[{"delta":{"refusal":"I cannot help"}}]}`,
		}, false},
		{"reasoning then deprecated function_call is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"id":"1","choices":[{"delta":{"function_call":{"name":"f","arguments":"{}"}}}]}`,
		}, false},
		{"empty truncation is not the thinking rescue", []string{
			`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
		}, false},
		{"seen terminator is not a truncation", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		}, false},
		{"provider in-band error is not retryable", []string{
			`data: {"id":"1","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}`,
		}, false},
		{"responses-shaped reasoning is never appended to", []string{
			`data: {"type":"response.created","response":{"id":"r1"}}`,
			`data: {"type":"response.reasoning_text.delta","delta":"thinking"}`,
		}, false},
		{"non-chatlike stream stays out", []string{
			`data: {"event":"custom","payload":42}`,
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Analyzer{}
			feedLines(a, c.lines...)
			if got := a.RetryableReasoningTruncation(); got != c.want {
				t.Fatalf("RetryableReasoningTruncation = %v, want %v", got, c.want)
			}
		})
	}
}
