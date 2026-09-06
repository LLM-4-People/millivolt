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
