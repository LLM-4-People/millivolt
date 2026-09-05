package proxy

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// A Cursor tool-call turn with streamed text before the call is a MIXED
// response - FinalizeRecord must keep its answer tokens. Regression for the
// quality-gate finding: cursorTrackDelta is the only path feeding cursor
// records, and it never set HadAnswerContent, so a text+tool_call cursor turn
// was misclassified tool-only (answer/gen tokens zeroed).
func TestCursorTrackDeltaSetsHadAnswerContent(t *testing.T) {
	rec := &metrics.Record{}

	// A text delta arrives before the tool call parks.
	cursorTrackDelta(rec, map[string]any{"content": "Let me check that. "})
	if !rec.HadAnswerContent {
		t.Fatal("content delta must set HadAnswerContent")
	}
	// The tool-call delta itself does not carry content.
	cursorTrackDelta(rec, map[string]any{"tool_calls": []any{map[string]any{"id": "c1"}}})

	rec.FinishReason = "tool_calls"
	rec.FirstTokenAt = rec.LastTokenAt.Add(-100 * 1e6) // 100ms window
	rec.End = rec.LastTokenAt
	rec.Usage.OutputTokens = 40
	rec.Usage.ReasoningTokens = 5
	metrics.FinalizeRecord(rec)

	// Mixed turn: answer tokens kept (40 output - 5 reasoning = 35), not zeroed.
	if rec.AnswerTokens != 35 {
		t.Errorf("AnswerTokens = %d, want 35 (mixed content+tool keeps answer tokens)", rec.AnswerTokens)
	}
	if rec.GenTokens != 40 {
		t.Errorf("GenTokens = %d, want 40 (reasoning 5 + answer 35)", rec.GenTokens)
	}

	// Contrast: a tool-call-ONLY turn (no content deltas) stays zero-answer.
	rec2 := &metrics.Record{}
	cursorTrackDelta(rec2, map[string]any{"tool_calls": []any{map[string]any{"id": "c1"}}})
	rec2.FinishReason = "tool_calls"
	rec2.FirstTokenAt = rec2.LastTokenAt.Add(-50 * 1e6)
	rec2.End = rec2.LastTokenAt
	rec2.Usage.OutputTokens = 17 // all tool-arg tokens
	metrics.FinalizeRecord(rec2)
	if rec2.HadAnswerContent {
		t.Error("tool-call delta must not set HadAnswerContent")
	}
	if rec2.AnswerTokens != 0 || rec2.GenTokens != 0 {
		t.Errorf("tool-only turn: AnswerTokens=%d GenTokens=%d, want 0/0", rec2.AnswerTokens, rec2.GenTokens)
	}
}
