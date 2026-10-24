package sse

// Round 3 batch 2 regressions (F1): a completed Responses terminal carrying a
// recognized tool call or any other official output item is never replaced by
// the empty-completion failure.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesCustomToolCallCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_1","name":"custom"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if rec.ToolCalls != 1 {
		t.Fatalf("custom tool calls = %d, want 1", rec.ToolCalls)
	}
	if rec.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
	}
	if a.OutcomeCode() != "" {
		t.Fatalf("custom tool call outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesLocalShellCallCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"local_shell_call","call_id":"call_2","name":"shell"}]}}`,
	})
	if rec.ToolCalls != 1 {
		t.Fatalf("local shell calls = %d, want 1", rec.ToolCalls)
	}
	if a.OutcomeCode() != "" {
		t.Fatalf("local shell outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesEncryptedReasoningOnlyCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]}]}}`,
	})
	if a.OutcomeCode() == metrics.CodeEmptyCompletion {
		t.Fatalf("encrypted reasoning was classified as an empty completion (record %+v)", rec)
	}
}

func TestResponsesWebSearchCallCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed"}]}}`,
	})
	if a.OutcomeCode() != "" {
		t.Fatalf("web search outcome = %q, want healthy", a.OutcomeCode())
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("web search finish = %q, want stop", rec.FinishReason)
	}
}

func TestResponsesUnknownOutputItemCompletes(t *testing.T) {
	a, _ := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"provider_specific_item","id":"x_1"}]}}`,
	})
	if a.OutcomeCode() != "" {
		t.Fatalf("unknown output item outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesEmptyMessageStaysEmptyCompletion(t *testing.T) {
	a, _ := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
	})
	if a.OutcomeCode() != metrics.CodeEmptyCompletion {
		t.Fatalf("empty message outcome = %q, want empty_completion", a.OutcomeCode())
	}
}
