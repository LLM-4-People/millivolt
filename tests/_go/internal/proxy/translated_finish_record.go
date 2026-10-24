package proxy

// Round 3 batch 3 regression (B1): the record keeps the canonical Anthropic
// reason even though the Chat wire mapper approximates it.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTranslatedRecordKeepsRawFinish(t *testing.T) {
	for _, reason := range []ir.FinishReason{"pause_turn", "refusal", ir.FinishError} {
		resp := &ir.Response{ID: "r1", Model: "m", Finish: reason}
		rec := &metrics.Record{}
		applyTranslatedStructure(rec, resp, false)
		if rec.FinishReason != string(reason) {
			t.Fatalf("record finish = %q, want raw %q", rec.FinishReason, reason)
		}
	}
}
