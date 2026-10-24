package sse

// Round 5 NEW-1 regression: a native Anthropic stream that ends with
// stop_reason "error" and no explicit error event is a provider failure. The
// record must synthesize upstream_error, matching the non-stream decoder and
// the translated EventError path. An explicit error event keeps its own type.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicErrorStopStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"error"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamStopReasonErrorRecordsFailure(t *testing.T) {
	var a Analyzer
	feedAnthropicLines(t, &a, anthropicErrorStopStream)
	if !a.Terminated() {
		t.Fatal("message_stop did not terminate")
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.FinishReason != "error" {
		t.Fatalf("finish = %q, want error", rec.FinishReason)
	}
	if rec.ErrorType != TypeUpstreamError {
		t.Fatalf("error type = %q, want %q", rec.ErrorType, TypeUpstreamError)
	}
	if rec.ErrorMsg == "" {
		t.Fatal("bare error stop reason recorded no message")
	}
	if !rec.IsError() {
		t.Fatalf("record is not an error: %+v", rec)
	}
	if !a.HasInBandError() {
		t.Fatal("analyzer did not expose the failure statement")
	}
}

func TestAnthropicExplicitErrorEventKeepsProviderType(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
		``,
	}, "\n")
	var a Analyzer
	feedAnthropicLines(t, &a, stream)
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ErrorType != "overloaded_error" || rec.ErrorMsg != "busy" {
		t.Fatalf("error = %q/%q, want the explicit provider error", rec.ErrorType, rec.ErrorMsg)
	}
}

func TestAnthropicStreamCleanStopStaysHealthy(t *testing.T) {
	stream := strings.ReplaceAll(anthropicErrorStopStream, `"stop_reason":"error"`, `"stop_reason":"end_turn"`)
	var a Analyzer
	feedAnthropicLines(t, &a, stream)
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ErrorType != "" || rec.IsError() {
		t.Fatalf("clean stream recorded an error: %+v", rec)
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("finish = %q, want stop", rec.FinishReason)
	}
}
