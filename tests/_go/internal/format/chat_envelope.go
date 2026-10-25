package format

// Round 3 batch 12 regression (B2): the Chat completion choice carries the
// nullable logprobs member the response schema declares.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatResponseChoiceHasLogprobs(t *testing.T) {
	resp := &ir.Response{
		ID:     "c1",
		Model:  "m",
		Blocks: []ir.Block{ir.TextBlock("hello")},
		Finish: ir.FinishStop,
	}
	body, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := decodeJSONMap(t, body)["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v, want one", choices)
	}
	choice := choices[0].(map[string]any)
	value, present := choice["logprobs"]
	if !present {
		t.Fatalf("choice omits logprobs: %v", choice)
	}
	if value != nil {
		t.Fatalf("logprobs = %v, want null", value)
	}
}
