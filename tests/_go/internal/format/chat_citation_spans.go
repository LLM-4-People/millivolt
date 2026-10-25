package format

// Round 7 B1 regression: Chat url_citation spans are relative to the whole
// message content, but an IR citation is relative to its own text block. The
// encoder must add the running rune length of preceding text blocks.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatCitation(text string, start, end int) ir.Block {
	return ir.Block{
		Type: ir.BlockText,
		Text: text,
		Citations: []ir.Citation{{
			Type: "url_citation", URL: "http://example.test/" + text,
			Title: text, StartIndex: start, EndIndex: end,
		}},
	}
}

func chatAnnotationSpans(t *testing.T, blocks []ir.Block) (string, [][2]int) {
	t.Helper()
	resp := &ir.Response{ID: "c1", Model: "m", Created: 1, Blocks: blocks, Finish: ir.FinishStop}
	body, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := decodeJSONMap(t, body)["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v, want one", choices)
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	content, _ := message["content"].(string)
	rawAnnotations, _ := message["annotations"].([]any)
	spans := make([][2]int, 0, len(rawAnnotations))
	for i, raw := range rawAnnotations {
		annotation, _ := raw.(map[string]any)["url_citation"].(map[string]any)
		if annotation == nil {
			t.Fatalf("annotation %d has no url_citation: %v", i, raw)
		}
		start, _ := annotation["start_index"].(float64)
		end, _ := annotation["end_index"].(float64)
		spans = append(spans, [2]int{int(start), int(end)})
	}
	return content, spans
}

func TestChatAnnotationSpansRebasedAcrossTextBlocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []ir.Block
		want   string
		spans  [][2]int
	}{
		{
			name:   "single block unchanged",
			blocks: []ir.Block{chatCitation("AAA", 0, 3)},
			want:   "AAA",
			spans:  [][2]int{{0, 3}},
		},
		{
			name:   "second block rebased",
			blocks: []ir.Block{chatCitation("AAA", 0, 3), chatCitation("BBB", 0, 3)},
			want:   "AAABBB",
			spans:  [][2]int{{0, 3}, {3, 6}},
		},
		{
			name:   "uncited prefix contributes",
			blocks: []ir.Block{ir.TextBlock("AAA"), chatCitation("BBB", 0, 3)},
			want:   "AAABBB",
			spans:  [][2]int{{3, 6}},
		},
		{
			name: "non-text block contributes no offset",
			blocks: []ir.Block{
				chatCitation("AAA", 0, 3),
				{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: "RR"}},
				chatCitation("BBB", 0, 3),
			},
			want:  "AAABBB",
			spans: [][2]int{{0, 3}, {3, 6}},
		},
		{
			name:   "rune offset not byte offset",
			blocks: []ir.Block{ir.TextBlock("éé"), chatCitation("BBB", 0, 3)},
			want:   "ééBBB",
			spans:  [][2]int{{2, 5}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, spans := chatAnnotationSpans(t, tc.blocks)
			if content != tc.want {
				t.Fatalf("content = %q, want %q", content, tc.want)
			}
			if len(spans) != len(tc.spans) {
				t.Fatalf("spans = %v, want %v", spans, tc.spans)
			}
			for i := range spans {
				if spans[i] != tc.spans[i] {
					t.Fatalf("spans = %v, want %v", spans, tc.spans)
				}
			}
		})
	}
}
