package sse

// Batch 13 regression: an overflowing Anthropic usage fold never installs a
// partial document; the record keeps the chunk fallback (or no usage).

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicUsageOverflowKeepsFallback(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var a Analyzer
	feedAnthropicLines(t, &a, stream)
	if !a.Terminated() {
		t.Fatal("message_stop did not terminate")
	}
	var rec metrics.Record
	a.Fill(&rec)
	if !a.UsageInvalid() {
		t.Fatal("overflow did not mark the stream usage invalid")
	}
	if rec.Usage.InputTokens != 0 || rec.Usage.CacheReadTokens != 0 || rec.Usage.CacheWrite != 0 || rec.Usage.TotalTokens != 0 {
		t.Fatalf("overflow installed partial input usage: %+v", rec.Usage)
	}
	// The chunk-count fallback owns the output count when no usage document
	// was installed.
	if rec.Usage.OutputTokens != 1 {
		t.Fatalf("chunk fallback output tokens = %d, want 1", rec.Usage.OutputTokens)
	}
}
