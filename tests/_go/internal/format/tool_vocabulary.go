package format

// Round 2 TC1-7 regressions: the Chat and Anthropic encoders fail closed on
// tool names and ids outside the target's documented vocabulary.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func toolVocabularyRequest(name, id string) *ir.Request {
	maxTokens := 16
	return &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
		Tools:    []ir.Tool{{Name: name, Type: "function"}},
	}
}

func TestChatRejectsOverlongToolName(t *testing.T) {
	if _, err := EncodeChatRequest(toolVocabularyRequest(strings.Repeat("a", 64), "")); err != nil {
		t.Fatalf("64-character chat tool name rejected: %v", err)
	}
	_, err := EncodeChatRequest(toolVocabularyRequest(strings.Repeat("a", 65), ""))
	if err == nil || !strings.Contains(err.Error(), "tool name") {
		t.Fatalf("65-character chat tool name error = %v", err)
	}
	req := toolVocabularyRequest("lookup", "")
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "bad.name"}
	if _, err := EncodeChatRequest(req); err == nil || !strings.Contains(err.Error(), "tool_choice.name") {
		t.Fatalf("invalid chat tool_choice.name error = %v", err)
	}
}

func TestAnthropicRejectsInvalidToolID(t *testing.T) {
	req := toolVocabularyRequest("lookup", "")
	req.Messages[0].Content = append(req.Messages[0].Content, ir.Block{
		Type:     ir.BlockToolCall,
		ToolCall: &ir.ToolCall{ID: "call.1", Name: "lookup", Arguments: []byte(`{}`)},
	})
	if _, err := EncodeAnthropicRequest(req, 16, ""); err == nil || !strings.Contains(err.Error(), "tool_use.id") {
		t.Fatalf("invalid anthropic tool id error = %v", err)
	}

	result := toolVocabularyRequest("lookup", "")
	result.Messages[0].Content = append(result.Messages[0].Content, ir.Block{
		Type:       ir.BlockToolResult,
		ToolResult: &ir.ToolResult{CallID: "call:1"},
	})
	if _, err := EncodeAnthropicRequest(result, 16, ""); err == nil || !strings.Contains(err.Error(), "tool_use_id") {
		t.Fatalf("invalid anthropic tool_use_id error = %v", err)
	}
}

func TestAnthropicToolNameVocabulary(t *testing.T) {
	if _, err := EncodeAnthropicRequest(toolVocabularyRequest(strings.Repeat("a", 128), ""), 16, ""); err != nil {
		t.Fatalf("128-character anthropic tool name rejected: %v", err)
	}
	_, err := EncodeAnthropicRequest(toolVocabularyRequest(strings.Repeat("a", 129), ""), 16, "")
	if err == nil || !strings.Contains(err.Error(), "tool name") {
		t.Fatalf("129-character anthropic tool name error = %v", err)
	}
	req := toolVocabularyRequest("bad name", "")
	if _, err := EncodeAnthropicRequest(req, 16, ""); err == nil || !strings.Contains(err.Error(), "tool name") {
		t.Fatalf("invalid anthropic tool name error = %v", err)
	}
	choice := toolVocabularyRequest("lookup", "")
	choice.ToolChoice = &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: "bad.name"}
	if _, err := EncodeAnthropicRequest(choice, 16, ""); err == nil || !strings.Contains(err.Error(), "tool_choice.name") {
		t.Fatalf("invalid anthropic tool_choice.name error = %v", err)
	}
}
