package metrics

import (
	"testing"
	"time"
)

// gen_tps measures GENERATION tokens (reasoning + answer content) over the
// first-token→last-token window. Two bugs this pins:
//  1. The numerator used to be completion_tokens, which counts tool-call
//     argument tokens (OpenAI bills them; a content:null tool_calls response
//     still reports completion_tokens) - inflating the rate.
//  2. The window used to end at End (past the last token, after usage/[DONE]
//     handling) instead of at the last generation token.
func TestGenThroughputToolCallsAndWindow(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Generation window: first token +1s → last token +3s (2s). The request
	// doesn't fully end until +5s (usage/[DONE]/close overhead) - that tail
	// must NOT stretch the window.
	win := func(gen int64) *Record {
		return &Record{
			Start:        base,
			FirstTokenAt: base.Add(1 * time.Second),
			LastTokenAt:  base.Add(3 * time.Second),
			End:          base.Add(5 * time.Second),
			GenTokens:    gen, // analyzer's content+reasoning count (no usage blob)
		}
	}

	// 60 generation tokens over a 2s window = 30 tok/s (NOT over the 4s
	// first→end window, which would read 15).
	r := win(60)
	FinalizeRecord(r)
	if r.DecodeTPS != 30 {
		t.Errorf("gen_tps = %v, want 30 (60 gen tokens / 2s first→last window)", r.DecodeTPS)
	}

	// Provider-usage path, no tool calls: numerator = reasoning + answer.
	r2 := &Record{
		FirstTokenAt: base,
		LastTokenAt:  base.Add(2 * time.Second),
		End:          base.Add(2 * time.Second),
	}
	r2.Usage.OutputTokens = 80
	r2.Usage.ReasoningTokens = 30
	FinalizeRecord(r2)
	if r2.GenTokens != 80 {
		t.Errorf("GenTokens = %d, want 80 (reasoning 30 + answer 50)", r2.GenTokens)
	}
	if r2.DecodeTPS != 40 {
		t.Errorf("gen_tps = %v, want 40 (80 gen tokens / 2s)", r2.DecodeTPS)
	}

	// Tool-call-only response (finish_reason=tool_calls, content:null - the
	// provider's 17 completion_tokens are all tool args). Generation content is
	// reasoning only (0 here), so gen_tps is 0, not "17 / window".
	r3 := &Record{
		FirstTokenAt: base,
		LastTokenAt:  base.Add(1 * time.Second),
		End:          base.Add(1 * time.Second),
		FinishReason: "tool_calls",
		// HadAnswerContent unset: no answer content streamed (content:null).
	}
	r3.Usage.OutputTokens = 17
	r3.Usage.ReasoningTokens = 0
	FinalizeRecord(r3)
	if r3.GenTokens != 0 || r3.DecodeTPS != 0 {
		t.Errorf("tool-only response: GenTokens=%d gen_tps=%v, want 0/0", r3.GenTokens, r3.DecodeTPS)
	}

	// Mixed response: content AND a tool call (finish_reason=tool_calls but the
	// stream carried answer content). The answer tokens are real generation and
	// must NOT be zeroed; the (inseparable) tool-arg tokens in completion_tokens
	// remain - a documented floor, not a miscount of visible content.
	//
	// HadAnswerContent (not FirstAnswerAt) is the gate: the NON-streaming path
	// never sets FirstAnswerAt, so a non-streaming mixed response must still keep
	// its answer tokens (the regression the quality gate caught).
	for _, streaming := range []bool{true, false} {
		r4 := &Record{
			FirstTokenAt:     base,
			LastTokenAt:      base.Add(2 * time.Second),
			End:              base.Add(2 * time.Second),
			FinishReason:     "tool_calls",
			HadAnswerContent: true, // content arrived (both paths set this)
		}
		if streaming {
			r4.FirstAnswerAt = base.Add(500 * time.Millisecond)
		}
		r4.Usage.OutputTokens = 50
		r4.Usage.ReasoningTokens = 10
		FinalizeRecord(r4)
		if r4.AnswerTokens != 40 {
			t.Errorf("mixed content+tool (streaming=%v): AnswerTokens = %d, want 40 (50 - 10 reasoning)", streaming, r4.AnswerTokens)
		}
		if r4.GenTokens != 50 {
			t.Errorf("mixed content+tool (streaming=%v): GenTokens = %d, want 50 (reasoning 10 + answer 40)", streaming, r4.GenTokens)
		}
	}
}
