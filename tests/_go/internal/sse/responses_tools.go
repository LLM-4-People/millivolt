package sse

// Batch 5 regression (F1): Responses function calls are counted from
// response.output_item.added, their argument deltas are tool activity rather
// than answer content, and an empty-argument call is a healthy tool turn.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func analyzeLines(lines []string) (*Analyzer, *metrics.Record) {
	var a Analyzer
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, line := range lines {
		a.Feed([]byte(line), now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	return &a, &rec
}

func TestResponsesToolCallsCountedAndArgumentsNotAnswer(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}`,
		`data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"answer"}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if rec.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
		t.Fatalf("tool names = %v, want [lookup]", rec.ToolNames)
	}
	if !rec.HadAnswerContent {
		t.Fatal("answer text did not set HadAnswerContent")
	}
	if rec.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
	}
	if rec.Usage.OutputTokens != rec.Chunks {
		t.Fatalf("output fallback = %d, chunks = %d", rec.Usage.OutputTokens, rec.Chunks)
	}
	if a.OutcomeCode() == metrics.CodeEmptyCompletion {
		t.Fatal("mixed tool and answer turn classified as an empty completion")
	}
}

func TestResponsesArgumentsOnlyIsNotAnswerContent(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":1}"}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if rec.HadAnswerContent {
		t.Fatal("function-call arguments were classified as answer content")
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", rec.ToolCalls)
	}
	if rec.FirstTokenAt.IsZero() {
		t.Fatal("a tool-only stream recorded no first token")
	}
	if a.OutcomeCode() != "" {
		t.Fatalf("empty-argument tool call outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesToolCallCountedOnce(t *testing.T) {
	_, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if rec.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1 (same call_id must count once)", rec.ToolCalls)
	}
}
