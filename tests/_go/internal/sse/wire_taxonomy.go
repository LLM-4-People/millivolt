package sse

// Batch 5 regressions: the analyzer records a standalone Responses error, does
// not count an empty delta as a token, captures previews for every wire, and
// counts an Anthropic tool-only stream as generated output.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesStandaloneErrorRecorded(t *testing.T) {
	_, rec := analyzeLines([]string{
		`data: {"type":"error","code":"server_error","message":"boom"}`,
	})
	if rec.ErrorType == "" {
		t.Fatalf("standalone Responses error recorded no type: %+v", rec)
	}
	if rec.ErrorCode != "server_error" || rec.ErrorMsg != "boom" {
		t.Fatalf("recorded error = %s/%s, want server_error/boom", rec.ErrorCode, rec.ErrorMsg)
	}
}

func TestEmptyDeltaDoesNotCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{
			name: "anthropic empty thinking",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "anthropic empty text",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_stop"}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rec := analyzeLines(tc.lines)
			if rec.Chunks != 0 {
				t.Fatalf("chunks = %d, want 0 for empty deltas", rec.Chunks)
			}
			if !rec.FirstTokenAt.IsZero() {
				t.Fatalf("first token set by an empty delta: %v", rec.FirstTokenAt)
			}
			if !a.firstReasoningAt.IsZero() {
				t.Fatalf("first reasoning set by an empty thinking delta: %v", a.firstReasoningAt)
			}
		})
	}
}

func TestResponsesAndAnthropicPreviewCaptured(t *testing.T) {
	responses := []string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello from responses"}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	}
	anthropic := []string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from anthropic"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"responses", responses, "hello from responses"},
		{"anthropic", anthropic, "hello from anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			a.CapturePreview = true
			for _, line := range tc.lines {
				a.Feed([]byte(line), time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC))
			}
			var rec metrics.Record
			a.Fill(&rec)
			if rec.ResponsePreview != tc.want {
				t.Fatalf("preview = %q, want %q", rec.ResponsePreview, tc.want)
			}
		})
	}
}

func TestAnthropicToolOnlyStreamSetsFirstToken(t *testing.T) {
	_, rec := analyzeLines([]string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"lookup","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":1}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	})
	if rec.FirstTokenAt.IsZero() {
		t.Fatal("tool-only Anthropic stream recorded no first token")
	}
	if rec.Chunks == 0 {
		t.Fatal("tool-only Anthropic stream recorded no chunks")
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", rec.ToolCalls)
	}
	if rec.HadAnswerContent {
		t.Fatal("tool-only Anthropic stream was classified as answer content")
	}
	if rec.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
	}
}
