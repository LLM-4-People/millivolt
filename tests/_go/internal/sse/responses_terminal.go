package sse

// Batch 4 regression: a Responses terminal event maps its own status and
// incomplete reason onto the record's finish vocabulary, so a short
// max_output_tokens answer is not reclassified as an empty completion and a
// completed tool call keeps a reason.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesTerminalMapsFinishReason(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	cases := []struct {
		name   string
		lines  []string
		finish string
		tools  int
	}{
		{
			name: "completed text",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.output_text.delta","delta":"hi","output_index":0,"content_index":0}`,
				`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
			},
			finish: "stop",
		},
		{
			name: "completed tool call",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"lookup"}}`,
				`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
			},
			finish: "tool_calls",
			tools:  1,
		},
		{
			name: "incomplete max output",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
			},
			finish: "length",
		},
		{
			name: "incomplete max messages",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_messages"}}}`,
			},
			finish: "other",
		},
		{
			name: "cancelled",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.cancelled","response":{"id":"r1","status":"cancelled"}}`,
			},
			finish: "cancelled",
		},
		{
			name: "failed",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
			},
			finish: "error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			for _, line := range tc.lines {
				a.Feed([]byte(line), now)
			}
			var rec metrics.Record
			a.Fill(&rec)
			if a.finishReason != tc.finish {
				t.Fatalf("analyzer finish = %q, want %q", a.finishReason, tc.finish)
			}
			if rec.FinishReason != tc.finish {
				t.Fatalf("record finish = %q, want %q", rec.FinishReason, tc.finish)
			}
			if a.toolCalls != tc.tools {
				t.Fatalf("tool calls = %d, want %d", a.toolCalls, tc.tools)
			}
			if tc.name == "incomplete max output" && a.OutcomeCode() == metrics.CodeEmptyCompletion {
				t.Fatal("a legitimate incomplete max_output_tokens answer was classified as an empty completion")
			}
		})
	}
}
