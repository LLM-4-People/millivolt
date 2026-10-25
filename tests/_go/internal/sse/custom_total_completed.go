package sse

// Round 11 F1 regression: an Anthropic custom total captured at message_start
// is stale once a completed message_delta reports output. The consistency
// guard ran only for a restored output count, so Fill recorded the start total.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func anthropicCustomTotalStream(custom string) string {
	start := `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":100,"output_tokens":0`
	if custom != "" {
		start += `,"my_total":` + custom
	}
	start += `}}}`
	return strings.Join([]string{
		`event: message_start`,
		`data: ` + start,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
}

func TestAnthropicCustomTotalIncludesCompletedOutput(t *testing.T) {
	cases := []struct {
		name   string
		custom string
		want   int64
	}{
		{name: "stale custom total is raised", custom: "100", want: 105},
		{name: "higher custom total stays authoritative", custom: "150", want: 150},
		{name: "no custom total derives the sum", custom: "", want: 105},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			a.UsageKeys = map[string]string{"total_tokens": "my_total"}
			feedAnthropicLines(t, &a, anthropicCustomTotalStream(tc.custom))
			var rec metrics.Record
			a.Fill(&rec)
			if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 5 {
				t.Fatalf("usage = %+v, want 100 input and 5 output", rec.Usage)
			}
			if rec.Usage.TotalTokens != tc.want {
				t.Fatalf("total = %d, want %d", rec.Usage.TotalTokens, tc.want)
			}
		})
	}

	// No usage at all stays zero; settlement derives the counts it has.
	var a Analyzer
	feedAnthropicLines(t, &a, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	var rec metrics.Record
	a.Fill(&rec)
	if rec.Usage.TotalTokens != 0 {
		t.Fatalf("total = %d without usage, want 0", rec.Usage.TotalTokens)
	}
}
