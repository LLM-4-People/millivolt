package sse

// Round 3 batch 9 regressions: provider usage key paths are relative to the
// usage object, a partial custom usage must not displace the chunk-derived
// generation count, and a later usage-less chunk must not erase the cost.

import (
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func feedChatLines(t *testing.T, a *Analyzer, lines ...string) {
	t.Helper()
	now := time.Unix(200, 0)
	for _, line := range lines {
		a.Feed([]byte(line), now)
		now = now.Add(time.Millisecond)
	}
}

func TestAnthropicStreamCustomUsageKeys(t *testing.T) {
	var a Analyzer
	a.UsageKeys = map[string]string{
		"input_tokens":  "prompt_toks",
		"output_tokens": "completion_toks",
	}
	a.CostKeys = []string{"x_cost"}
	feedAnthropicLines(t, &a, strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"prompt_toks":100}},"x_cost":"0.01"}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"completion_toks":50}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	var rec metrics.Record
	a.Fill(&rec)
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 50 || rec.Usage.TotalTokens != 150 {
		t.Fatalf("usage = %+v, want input 100 output 50 total 150", rec.Usage)
	}
	if rec.Cost != 0.01 {
		t.Fatalf("cost = %v, want 0.01 from the sibling of the message_start usage", rec.Cost)
	}
	if !a.UsageReported() {
		t.Fatal("custom usage was not reported")
	}
}

func TestPartialCustomUsageKeepsToolChunkExclusion(t *testing.T) {
	var a Analyzer
	a.UsageKeys = map[string]string{"input_tokens": "x_usage.prompt_toks"}
	feedChatLines(t, &a,
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"x_usage":{"prompt_toks":100}}`,
	)
	var rec metrics.Record
	a.Fill(&rec)
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want input 100 output 2", rec.Usage)
	}
	if rec.GenTokens != 1 {
		t.Fatalf("GenTokens = %d, want 1 (two chunks minus the tool chunk); an input-only custom path must not clear it", rec.GenTokens)
	}
}

func TestStreamCostSurvivesUsageLessFinish(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  float64
	}{
		{
			name: "cost beside usage then usage-less finish",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1},"x_pricing":{"cost":"0.01"}}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			want: 0.01,
		},
		{
			name: "cost on a later usage chunk then finish",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				`data: {"choices":[{"index":0,"delta":{"content":"!"}}],"usage":{"prompt_tokens":1,"completion_tokens":2},"x_pricing":{"cost":"0.02"}}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			want: 0.02,
		},
		{
			name: "second usage without cost does not erase it",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1},"x_pricing":{"cost":"0.01"}}`,
				`data: {"choices":[{"index":0,"delta":{"content":"!"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			want: 0.01,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			a.CostKeys = []string{"x_pricing.cost"}
			feedChatLines(t, &a, tc.lines...)
			var rec metrics.Record
			a.Fill(&rec)
			if rec.Cost != tc.want {
				t.Fatalf("cost = %v, want %v", rec.Cost, tc.want)
			}
		})
	}
}
