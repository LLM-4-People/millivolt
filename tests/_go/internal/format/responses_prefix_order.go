package format

// Round 4 batch 1 regression (C1): a terminal document that only extends the
// concatenated streamed bytes must append the tail to the last streamed part,
// never to an earlier part whose index happens to straddle the boundary.

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesContentFixture(chunks, terminal []string) string {
	var b strings.Builder
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

`)
	for i, chunk := range chunks {
		fmt.Fprintf(&b, `event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":%d,"part":{"type":"output_text","text":""}}

`, i)
		fmt.Fprintf(&b, `event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":%d,"delta":%q}

`, i, chunk)
	}
	parts := make([]string, 0, len(terminal))
	for _, text := range terminal {
		parts = append(parts, fmt.Sprintf(`{"type":"output_text","text":%q}`, text))
	}
	fmt.Fprintf(&b, `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[%s]}]}}

`, strings.Join(parts, ","))
	return b.String()
}

func anthropicTextBlocks(t *testing.T, out string) []string {
	t.Helper()
	texts := map[int]string{}
	for _, payload := range mustAnthropicPayloads(t, out) {
		switch payload["type"] {
		case "content_block_start":
			index := int(payload["index"].(float64))
			block, _ := payload["content_block"].(map[string]any)
			if text, _ := block["text"].(string); text != "" {
				texts[index] += text
			}
		case "content_block_delta":
			index := int(payload["index"].(float64))
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "text_delta" {
				if text, _ := delta["text"].(string); text != "" {
					texts[index] += text
				}
			}
		}
	}
	out2 := make([]string, 0, len(texts))
	for i := 0; i < len(texts); i++ {
		out2 = append(out2, texts[i])
	}
	return out2
}

func responsesTerminalText(t *testing.T, out string) string {
	t.Helper()
	for _, payload := range mustResponsePayloads(t, out) {
		if payload["type"] != "response.completed" {
			continue
		}
		response, _ := payload["response"].(map[string]any)
		output, _ := response["output"].([]any)
		var text strings.Builder
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part["type"] != "output_text" {
					continue
				}
				value, _ := part["text"].(string)
				text.WriteString(value)
			}
		}
		return text.String()
	}
	return ""
}

func TestResponsesTerminalPrefixExtensionPreservesTextOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chunks   []string
		terminal []string
		want     []string
	}{
		{
			name:     "prefix crossing maps to the last emitted part",
			chunks:   []string{"ab", "c"},
			terminal: []string{"abcX"},
			want:     []string{"ab", "cX"},
		},
		{
			name:     "aligned partition is unchanged",
			chunks:   []string{"ab", "cX"},
			terminal: []string{"ab", "cX"},
			want:     []string{"ab", "cX"},
		},
		{
			name:     "new tail part keeps its own key and order",
			chunks:   []string{"ab"},
			terminal: []string{"ab", "cd"},
			want:     []string{"ab", "cd"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := responsesContentFixture(tc.chunks, tc.terminal)
			var anthropic bytes.Buffer
			if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &anthropic, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := anthropicTextBlocks(t, anthropic.String()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("anthropic text by index = %v, want %v", got, tc.want)
			}
			var responses bytes.Buffer
			if err := TranslateStreamBetween(ir.Responses, ir.Responses, &responses, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := responsesTerminalText(t, responses.String()); got != strings.Join(tc.terminal, "") {
				t.Fatalf("responses terminal text = %q, want %q", got, strings.Join(tc.terminal, ""))
			}
		})
	}
}
