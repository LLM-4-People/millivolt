package proxy

// Round 2 TC6-5 regression: a translated refusal is answer-shaped but not
// answer content, exactly like the native paths, so a refusal+tool response
// settles as tool-only.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTranslatedRefusalWithToolIsToolOnly(t *testing.T) {
	rec := &metrics.Record{}
	resp := &ir.Response{
		Finish: ir.FinishToolCalls,
		Usage:  ir.Usage{InputTokens: 1, OutputTokens: 5, TotalTokens: 6},
		Blocks: []ir.Block{
			{Type: ir.BlockRefusal, Refusal: "no"},
			{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: "call_1", Name: "lookup"}},
		},
	}
	applyTranslatedStructure(rec, resp, true)
	if rec.HadAnswerContent {
		t.Fatalf("refusal set HadAnswerContent: %+v", rec)
	}
	if rec.ToolCalls != 1 || rec.ResponsePreview != "no" {
		t.Fatalf("tool calls/preview = %d/%q, want 1/no", rec.ToolCalls, rec.ResponsePreview)
	}
	metrics.FinalizeRecord(rec)
	if rec.FinishReason != "tool_calls" || rec.AnswerTokens != 0 || rec.GenTokens != 0 {
		t.Fatalf("finalized refusal+tool = %+v, want tool-only with zero answer/gen tokens", rec)
	}
}
