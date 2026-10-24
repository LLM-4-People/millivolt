package proxy

// Round 3 batch 3 regression (B1) and round 4 batch 14 strength (H-U1): the
// record keeps the canonical Anthropic reason even though the Chat wire mapper
// approximates it. ir.FinishPause is the discriminating case: the parent
// mapper returned stop for it, while the record owner must keep pause.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTranslatedRecordKeepsRawFinish(t *testing.T) {
	for _, reason := range []ir.FinishReason{ir.FinishPause, "refusal", ir.FinishError} {
		resp := &ir.Response{ID: "r1", Model: "m", Finish: reason}
		rec := &metrics.Record{}
		applyTranslatedStructure(rec, resp, false)
		if rec.FinishReason != string(reason) {
			t.Fatalf("record finish = %q, want raw %q", rec.FinishReason, reason)
		}
	}
}

// TestTranslatedFinishSplit pins the two vocabularies together: the record
// keeps the source spelling while the Chat wire receives a valid enum.
func TestTranslatedFinishSplit(t *testing.T) {
	for _, tc := range []struct {
		reason ir.FinishReason
		wire   string
	}{
		{ir.FinishPause, "stop"},
		{"refusal", "content_filter"},
		{ir.FinishError, "stop"},
	} {
		rec := &metrics.Record{}
		applyTranslatedStructure(rec, &ir.Response{Finish: tc.reason}, false)
		if rec.FinishReason != string(tc.reason) {
			t.Fatalf("record finish = %q, want raw %q", rec.FinishReason, tc.reason)
		}
		if got := format.ChatFinishReason(tc.reason); got != tc.wire {
			t.Fatalf("Chat wire finish for %q = %q, want %q", tc.reason, got, tc.wire)
		}
	}
}
