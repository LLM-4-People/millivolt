package sse

import (
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func feedAnthropicLines(t *testing.T, a *Analyzer, stream string) {
	t.Helper()
	now := time.Unix(100, 0)
	for _, line := range strings.Split(stream, "\n") {
		a.Feed([]byte(line), now)
		now = now.Add(time.Millisecond)
	}
}

func TestAnalyzerAnthropicTextAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
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
	if !a.Terminated() {
		t.Fatal("message_stop did not terminate")
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.FirstTokenAt.IsZero() || rec.FirstAnswerAt.IsZero() {
		t.Fatalf("token timestamps not set: %+v", rec)
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", rec.FinishReason)
	}
	if rec.Usage.InputTokens != 15 || rec.Usage.OutputTokens != 5 || rec.Usage.TotalTokens != 20 {
		t.Fatalf("usage = %+v", rec.Usage)
	}
	if rec.Usage.CacheReadTokens != 3 || rec.Usage.CacheWrite != 2 {
		t.Fatalf("cache usage = %+v", rec.Usage)
	}
	if !rec.HadAnswerContent || rec.ProviderRequestID != "msg_1" || rec.ProviderModel != "claude" {
		t.Fatalf("record = %+v", rec)
	}
	if code := a.OutcomeCode(); code != "" {
		t.Fatalf("outcome = %q, want healthy", code)
	}
}

func TestAnalyzerAnthropicToolAndThinking(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	var a Analyzer
	feedAnthropicLines(t, &a, stream)
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ToolCalls != 1 || len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
		t.Fatalf("tool accounting = %+v", rec)
	}
	if rec.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", rec.FinishReason)
	}
	if rec.FirstReasoningAt == 0 {
		t.Fatal("thinking delta did not set the reasoning timestamp")
	}
	if rec.HadAnswerContent {
		t.Fatal("thinking-only turn was marked as answer content")
	}
}

func TestAnalyzerAnthropicTruncationAndError(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		stream := `data: {"type":"message_start","message":{"id":"m","model":"c"}}` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`
		var a Analyzer
		feedAnthropicLines(t, &a, stream)
		if a.Terminated() {
			t.Fatal("a stream without message_stop or stop_reason was marked terminated")
		}
		if code := a.OutcomeCode(); code != metrics.CodeTruncated {
			t.Fatalf("outcome = %q, want %q", code, metrics.CodeTruncated)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		stream := `data: {"type":"message_start","message":{"id":"m","model":"c"}}` + "\n" +
			`data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`
		var a Analyzer
		feedAnthropicLines(t, &a, stream)
		var rec metrics.Record
		a.Fill(&rec)
		if rec.ErrorType != "overloaded_error" || rec.ErrorMsg != "busy" {
			t.Fatalf("error = %q/%q", rec.ErrorType, rec.ErrorMsg)
		}
	})
}

func TestAnalyzerAnthropicDoesNotClassifyChat(t *testing.T) {
	var a Analyzer
	a.Feed([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}`), time.Unix(1, 0))
	if !a.chatlike {
		t.Fatal("chat chunk was not classified")
	}
	if a.terminatorSeen {
		t.Fatal("chat chunk was misread as an Anthropic terminal")
	}
}
