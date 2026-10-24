package sse

// Round 8 F1 regression: a Chat tool call is identified by its required delta
// index, not only by an id. An id-less first fragment with a function name was
// ignored, so a valid tool turn became the empty_tool_call failure and a
// reasoning-only stop could still be retried.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnalyzerChatToolCallWithoutID(t *testing.T) {
	now := time.Now()
	feed := func(a *Analyzer, lines ...string) {
		t.Helper()
		for _, line := range lines {
			a.Feed([]byte(line), now)
		}
	}

	t.Run("id-less named call counts once", func(t *testing.T) {
		var a Analyzer
		feed(&a,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"search"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		)
		if a.toolCalls != 1 {
			t.Fatalf("toolCalls = %d, want 1", a.toolCalls)
		}
		if code := a.OutcomeCode(); code != "" {
			t.Fatalf("outcome = %q, want a delivered tool turn", code)
		}
		var rec metrics.Record
		a.Fill(&rec)
		if rec.ToolCalls != 1 || len(rec.ToolNames) != 1 || rec.ToolNames[0] != "search" {
			t.Fatalf("record calls/names = %d/%v, want 1/[search]", rec.ToolCalls, rec.ToolNames)
		}
	})

	t.Run("two id-less indexes count twice", func(t *testing.T) {
		var a Analyzer
		feed(&a, `data: {"choices":[{"delta":{"tool_calls":[`+
			`{"index":0,"function":{"name":"search"}},`+
			`{"index":1,"function":{"name":"lookup"}}]}}]}`)
		if a.toolCalls != 2 {
			t.Fatalf("toolCalls = %d, want 2", a.toolCalls)
		}
		var rec metrics.Record
		a.Fill(&rec)
		if rec.ToolCalls != 2 || len(rec.ToolNames) != 2 ||
			rec.ToolNames[0] != "search" || rec.ToolNames[1] != "lookup" {
			t.Fatalf("record calls/names = %d/%v, want 2/[search lookup]", rec.ToolCalls, rec.ToolNames)
		}
	})

	t.Run("id-bearing call plus continuation counts once", func(t *testing.T) {
		var a Analyzer
		feed(&a,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":1}"}}]}}]}`,
		)
		if a.toolCalls != 1 {
			t.Fatalf("toolCalls = %d, want 1 for one index", a.toolCalls)
		}
	})

	t.Run("no-index id fallback unchanged", func(t *testing.T) {
		var a Analyzer
		feed(&a, `data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"search"}}]}}]}`)
		if a.toolCalls != 1 {
			t.Fatalf("toolCalls = %d, want 1", a.toolCalls)
		}
	})

	t.Run("id-less call blocks the reasoning rescues", func(t *testing.T) {
		var truncation Analyzer
		feed(&truncation,
			`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"search"}}]}}]}`,
		)
		if truncation.RetryableReasoningTruncation() {
			t.Fatal("reasoning truncation must not be rescued after a counted tool call")
		}
		var stop Analyzer
		feed(&stop,
			`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"search"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
		if stop.RetryableReasoningStop() {
			t.Fatal("reasoning stop must not be rescued after a counted tool call")
		}
	})
}

// A malformed index in one tool-call element must not discard its healthy
// siblings. The bad element contributes no index and falls back to id
// presence; the valid sibling keeps its index count and name.
func TestAnalyzerChatToolCallMalformedIndexKeepsSiblings(t *testing.T) {
	now := time.Now()
	for _, index := range []string{`"0"`, `0.0`, `0.5`, `-1`, `1e20`} {
		t.Run(index, func(t *testing.T) {
			var a Analyzer
			chunk := `data: {"choices":[{"delta":{"tool_calls":[` +
				`{"index":` + index + `,"id":"bad","function":{"name":"broken"}},` +
				`{"index":1,"id":"good","function":{"name":"search"}}]}}]}`
			a.Feed([]byte(chunk), now)
			a.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`), now)
			a.Feed([]byte(`data: [DONE]`), now)
			if a.toolCalls != 2 {
				t.Fatalf("toolCalls = %d, want 2 for index %s", a.toolCalls, index)
			}
			if code := a.OutcomeCode(); code != "" {
				t.Fatalf("outcome = %q, want a delivered tool turn", code)
			}
			var rec metrics.Record
			a.Fill(&rec)
			if rec.ToolCalls != 2 || len(rec.ToolNames) != 2 ||
				rec.ToolNames[0] != "broken" || rec.ToolNames[1] != "search" {
				t.Fatalf("record calls/names = %d/%v, want 2/[broken search]", rec.ToolCalls, rec.ToolNames)
			}
		})
	}
}
