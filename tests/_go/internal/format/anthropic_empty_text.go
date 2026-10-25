package format

// Round 2 TC1-8 regressions: Anthropic's TextBlockParam requires non-empty
// text, so an empty text part is dropped and an all-empty turn is omitted.

import (
	"encoding/json"
	"strings"
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

func TestAnthropicAllEmptyTurnRejected(t *testing.T) {
	maxTokens := 16
	req := &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: ""},
		}}},
	}
	if _, err := EncodeAnthropicRequest(req, 16, ""); err == nil ||
		!strings.Contains(err.Error(), "at least one non-empty message") {
		t.Fatalf("all-empty request error = %v, want the non-empty-message rejection", err)
	}
}

func TestAnthropicEmptyTurnSkippedBesideNonEmpty(t *testing.T) {
	maxTokens := 16
	req := &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: ""}}},
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
		},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	messages := anthropicRequestMessages(t, out)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want one", messages)
	}
	if messages[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("message = %v, want hi: %s", messages[0], out)
	}
}

// TestAnthropicEmptySystemBlockOmitted pins NEW-3: the top-level system field
// never carries an empty text block or an empty string.
func TestAnthropicEmptySystemBlockOmitted(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":""}]},` +
		`{"role":"user","content":"hi"}]}`
	out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONMap(t, out)
	if system, ok := root["system"]; ok {
		t.Fatalf("empty system block produced %v: %s", system, out)
	}

	// A later empty system message must not add an empty key either.
	body = `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text","text":""}]}]}`
	out, _, err = TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if system, ok := decodeJSONMap(t, out)["system"]; ok {
		t.Fatalf("late empty system block produced %v: %s", system, out)
	}
}

func anthropicToolResultRequest(t *testing.T, content []ir.Block) map[string]any {
	t.Helper()
	maxTokens := 16
	req := &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: "call_1", Name: "lookup", Arguments: []byte(`{}`)}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{CallID: "call_1", Content: content}},
			}},
		},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONMap(t, out)
	messages, _ := root["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v, want three", messages)
	}
	wrappers, _ := messages[2].(map[string]any)["content"].([]any)
	if len(wrappers) != 1 {
		t.Fatalf("tool result wrapper = %v", wrappers)
	}
	return wrappers[0].(map[string]any)
}

// TestAnthropicToolResultEmptyTextSkipped pins NEW-3: a mixed tool result
// keeps its image and drops the empty text block; a mixed result keeps its
// non-empty text.
func TestAnthropicToolResultEmptyTextSkipped(t *testing.T) {
	t.Run("image only", func(t *testing.T) {
		block := anthropicToolResultRequest(t, []ir.Block{
			{Type: ir.BlockText, Text: ""},
			{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "AAAA"}},
		})
		parts, _ := block["content"].([]any)
		if len(parts) != 1 {
			t.Fatalf("tool-result content = %v, want only the image", parts)
		}
		if part, _ := parts[0].(map[string]any); part["type"] != "image" {
			t.Fatalf("tool-result part = %v, want image", parts[0])
		}
	})

	t.Run("mixed text", func(t *testing.T) {
		block := anthropicToolResultRequest(t, []ir.Block{
			{Type: ir.BlockText, Text: ""},
			{Type: ir.BlockText, Text: "ok"},
			{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "AAAA"}},
		})
		parts, _ := block["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("tool-result content = %v, want text + image", parts)
		}
		if text, _ := parts[0].(map[string]any)["text"].(string); text != "ok" {
			t.Fatalf("tool-result text = %q, want ok", text)
		}
	})
}

// TestAnthropicResponsesEmptyToolResultTextSkipped pins the end-to-end r8h
// shape: a schema-valid Responses function-call output with an empty text and
// an image translates to an image-only Anthropic tool result.
func TestAnthropicResponsesEmptyToolResultTextSkipped(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"user","content":"hi"},` +
		`{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[` +
		`{"type":"input_text","text":""},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`
	out, _, err := TranslateRequestBetween(ir.Responses, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONMap(t, out)
	messages, _ := root["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v, want three", messages)
	}
	content, _ := messages[2].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("tool-result wrapper = %v", content)
	}
	parts, _ := content[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("tool-result content = %v, want only the image: %s", parts, out)
	}
	if part, _ := parts[0].(map[string]any); part["type"] != "image" {
		t.Fatalf("tool-result part = %v, want image", parts[0])
	}
}
