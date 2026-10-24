package sse

// Round 4 batch 8 regression (H3) plus round 7 NEW-2: a reasoning summary part
// announcement with visible text counts as reasoning presence, whether it
// arrives on one data line or is reassembled at the event boundary.

import (
	"fmt"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// summaryPartEvent renders the announcement as one data line (compact) or as a
// pretty-printed multi-data-line event (reassembled at the blank boundary).
func summaryPartEvent(text string, multiLine bool) []string {
	if !multiLine {
		return []string{fmt.Sprintf(`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":%q}}`, text)}
	}
	return []string{
		`data: {`,
		`data:   "type": "response.reasoning_summary_part.added",`,
		`data:   "output_index": 0,`,
		`data:   "summary_index": 0,`,
		`data:   "part": {`,
		`data:     "type": "summary_text",`,
		fmt.Sprintf(`data:     "text": %q`, text),
		`data:   }`,
		`data: }`,
	}
}

func analyzeSummaryPart(t *testing.T, text string, multiLine bool) (*Analyzer, *metrics.Record) {
	t.Helper()
	lines := []string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		``,
	}
	lines = append(lines, summaryPartEvent(text, multiLine)...)
	lines = append(lines,
		``,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
	)
	return analyzeLines(lines)
}

func TestResponsesReasoningSummaryPartCountsAsReasoning(t *testing.T) {
	for _, multiLine := range []bool{false, true} {
		name := "single-line"
		if multiLine {
			name = "pretty-printed"
		}
		t.Run(name, func(t *testing.T) {
			a, rec := analyzeSummaryPart(t, "step", multiLine)
			if got := a.OutcomeCode(); got != metrics.CodeReasoningOnly {
				t.Fatalf("summary-part-only outcome = %q, want %q (record %+v)", got, metrics.CodeReasoningOnly, rec)
			}
			if rec.Chunks != 1 {
				t.Fatalf("summary-part chunks = %d, want 1 (record %+v)", rec.Chunks, rec)
			}
			if rec.FirstTokenAt.IsZero() {
				t.Fatalf("summary part did not record TTFT: %+v", rec)
			}
			if rec.HadAnswerContent {
				t.Fatalf("reasoning-only record reported answer content: %+v", rec)
			}
		})
	}
}

func TestResponsesEmptySummaryPartStaysEmptyCompletion(t *testing.T) {
	for _, multiLine := range []bool{false, true} {
		name := "single-line"
		if multiLine {
			name = "pretty-printed"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := analyzeSummaryPart(t, "", multiLine)
			if got := a.OutcomeCode(); got != metrics.CodeEmptyCompletion {
				t.Fatalf("empty summary part outcome = %q, want %q", got, metrics.CodeEmptyCompletion)
			}
		})
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
