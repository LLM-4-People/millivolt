package sse

// Round 5 F-U-b regression: when the chunk fallback restores output tokens that
// a truncated Anthropic message_start reported as zero, a non-zero provider
// total must include that restored output. A zero total stays zero (settlement
// derives input+output), and a total already at or above the sum is unchanged.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTruncatedAnthropicUsageTotalIncludesRestoredOutput(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
	}, "\n")
	var a Analyzer
	feedAnthropicLines(t, &a, stream)
	var rec metrics.Record
	a.Fill(&rec)
	if want := (metrics.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11}); rec.Usage != want {
		t.Fatalf("usage = %+v, want %+v", rec.Usage, want)
	}
}

func TestProviderTotalAboveRestoredSumIsKept(t *testing.T) {
	_, rec := analyzeLines([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":50}}`,
	})
	if want := (metrics.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 50}); rec.Usage != want {
		t.Fatalf("usage = %+v, want %+v", rec.Usage, want)
	}
}

func TestNormalAnthropicUsageTotalUnchanged(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10,"output_tokens":0}}}`,
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
	var a Analyzer
	feedAnthropicLines(t, &a, stream)
	var rec metrics.Record
	a.Fill(&rec)
	if want := (metrics.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}); rec.Usage != want {
		t.Fatalf("usage = %+v, want %+v", rec.Usage, want)
	}
}
