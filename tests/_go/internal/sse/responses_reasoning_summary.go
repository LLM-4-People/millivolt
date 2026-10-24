package sse

// Round 4 batch 8 regression (H3): a reasoning summary part announcement with
// visible text counts as reasoning presence, so an otherwise empty terminal is
// reasoning_only instead of empty_completion.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesReasoningSummaryPartCountsAsReasoning(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"step"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
	})
	if got := a.OutcomeCode(); got != metrics.CodeReasoningOnly {
		t.Fatalf("summary-part-only outcome = %q, want %q (record %+v)", got, metrics.CodeReasoningOnly, rec)
	}
	if rec.HadAnswerContent {
		t.Fatalf("reasoning-only record reported answer content: %+v", rec)
	}
}

func TestResponsesEmptySummaryPartStaysEmptyCompletion(t *testing.T) {
	a, _ := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
	})
	if got := a.OutcomeCode(); got != metrics.CodeEmptyCompletion {
		t.Fatalf("empty summary part outcome = %q, want %q", got, metrics.CodeEmptyCompletion)
	}
}

func TestResponsesReasoningSummaryWithAnswerStaysHealthy(t *testing.T) {
	a, _ := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"step"}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"answer"}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
	})
	if got := a.OutcomeCode(); got != "" {
		t.Fatalf("answer-bearing summary stream outcome = %q, want healthy", got)
	}
}
