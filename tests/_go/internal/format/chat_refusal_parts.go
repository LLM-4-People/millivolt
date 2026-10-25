package format

// Round 8 B2 regression: a Chat response has one refusal field, but the IR can
// carry several refusal parts. The encoder kept only the last part, so a split
// refusal lost every earlier text or vanished when the last part was empty.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatRefusalPartsConcatenate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []ir.Block
		want   string
	}{
		{
			name: "two parts concatenate",
			blocks: []ir.Block{
				{Type: ir.BlockRefusal, Refusal: "I cannot "},
				{Type: ir.BlockRefusal, Refusal: "help with that."},
			},
			want: "I cannot help with that.",
		},
		{
			name: "empty last part keeps the first",
			blocks: []ir.Block{
				{Type: ir.BlockRefusal, Refusal: "still no"},
				{Type: ir.BlockRefusal, Refusal: ""},
			},
			want: "still no",
		},
		{
			name:   "one part unchanged",
			blocks: []ir.Block{{Type: ir.BlockRefusal, Refusal: "no"}},
			want:   "no",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &ir.Response{
				ID: "c1", Model: "m", Finish: ir.FinishStop, Blocks: tc.blocks,
			}
			out, err := EncodeChatResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			choice := decodeJSONMap(t, out)["choices"].([]any)[0].(map[string]any)
			message := choice["message"].(map[string]any)
			if got := message["refusal"]; got != tc.want {
				t.Fatalf("refusal = %#v, want %q: %s", got, tc.want, out)
			}
		})
	}
}
