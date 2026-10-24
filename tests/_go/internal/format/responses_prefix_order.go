package format

// Round 4 batch 1 regression (C1), strengthened for round 5 H1: a terminal
// document that only extends the concatenated streamed bytes must preserve text
// order, including a tail after the source part is closed. The target may
// partition the text differently when a closed part becomes a new block, so the
// assertion is concatenated text order and a start before every delta, not an
// exact partition.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesContentFixture(chunks, terminal []string) string {
	var b strings.Builder
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

`)
	itemParts := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		fmt.Fprintf(&b, `event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":%d,"part":{"type":"output_text","text":""}}

`, i)
		fmt.Fprintf(&b, `event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":%d,"delta":%q}

`, i, chunk)
		fmt.Fprintf(&b, `event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":%d,"text":%q}

`, i, chunk)
		fmt.Fprintf(&b, `event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":%d,"part":{"type":"output_text","text":%q}}

`, i, chunk)
		itemParts = append(itemParts, fmt.Sprintf(`{"type":"output_text","text":%q}`, chunk))
	}
	fmt.Fprintf(&b, `event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[%s]}}

`, strings.Join(itemParts, ","))
	parts := make([]string, 0, len(terminal))
	for _, text := range terminal {
		parts = append(parts, fmt.Sprintf(`{"type":"output_text","text":%q}`, text))
	}
	fmt.Fprintf(&b, `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[%s]}]}}

`, strings.Join(parts, ","))
	return b.String()
}

// anthropicStreamText returns the concatenated visible text in block order and
// enforces that no delta ever arrives for an index without a preceding start.
func anthropicStreamText(t *testing.T, out string) string {
	t.Helper()
	started := map[int]bool{}
	var text strings.Builder
	for _, payload := range mustAnthropicPayloads(t, out) {
		switch payload["type"] {
		case "content_block_start":
			index := int(payload["index"].(float64))
			started[index] = true
			block, _ := payload["content_block"].(map[string]any)
			if value, _ := block["text"].(string); value != "" {
				text.WriteString(value)
			}
		case "content_block_delta":
			index := int(payload["index"].(float64))
			if !started[index] {
				t.Fatalf("delta for index %d arrived before its start: %s", index, out)
			}
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "text_delta" {
				if value, _ := delta["text"].(string); value != "" {
					text.WriteString(value)
				}
			}
		}
	}
	return text.String()
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
	}{
		{
			name:     "prefix crossing after a closed part",
			chunks:   []string{"ab", "c"},
			terminal: []string{"abcX"},
		},
		{
			name:     "aligned partition is unchanged",
			chunks:   []string{"ab", "cX"},
			terminal: []string{"ab", "cX"},
		},
		{
			name:     "new tail part keeps its own key and order",
			chunks:   []string{"ab"},
			terminal: []string{"ab", "cd"},
		},
		{
			name:     "closed part tail becomes a new block",
			chunks:   []string{"ab"},
			terminal: []string{"abc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := responsesContentFixture(tc.chunks, tc.terminal)
			wantText := strings.Join(tc.terminal, "")
			var anthropic bytes.Buffer
			if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &anthropic, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := anthropicStreamText(t, anthropic.String()); got != wantText {
				t.Fatalf("anthropic concatenated text = %q, want %q", got, wantText)
			}
			var responses bytes.Buffer
			if err := TranslateStreamBetween(ir.Responses, ir.Responses, &responses, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := responsesTerminalText(t, responses.String()); got != wantText {
				t.Fatalf("responses terminal text = %q, want %q", got, wantText)
			}
		})
	}
}
