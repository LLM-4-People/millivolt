package format

// Round 2 TC1-8 regressions: Anthropic's TextBlockParam requires non-empty
// text, so an empty text part is dropped and an all-empty turn is omitted.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicRequestMessages(t *testing.T, out []byte) []any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("body %q: %v", out, err)
	}
	messages, _ := root["messages"].([]any)
	return messages
}

func TestAnthropicSkipsEmptyTextBlock(t *testing.T) {
	maxTokens := 16
	req := &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: ""},
			{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "AAAA"}},
		}}},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	messages := anthropicRequestMessages(t, out)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want one", messages)
	}
	content, _ := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want only the image block: %s", content, out)
	}
	if block, _ := content[0].(map[string]any); block["type"] != "image" {
		t.Fatalf("content[0] = %v, want image: %s", block, out)
	}
}

func TestAnthropicAllEmptyTurnOmitted(t *testing.T) {
	maxTokens := 16
	req := &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: ""},
		}}},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if messages := anthropicRequestMessages(t, out); len(messages) != 0 {
		t.Fatalf("messages = %v, want none: %s", messages, out)
	}
}
