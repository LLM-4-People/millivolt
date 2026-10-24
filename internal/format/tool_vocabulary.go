package format

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// Target tool vocabularies. Chat function names are 1-64 characters of
// [a-zA-Z0-9_-]; Anthropic tool names are 1-128 of the same class, and
// Anthropic tool_use ids / tool_use_id references are non-empty of that class.
// A cross-protocol encoder validates the whole request before emitting a wire
// the target would reject.
var (
	chatToolNamePattern      = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	anthropicToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	anthropicToolIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

// toolVocabularyEchoMax bounds the rejected value echoed in an encoder error.
const toolVocabularyEchoMax = 128

// boundedToolValue keeps an error echo printable and bounded; a body-sized
// name must not become a body-sized error.
func boundedToolValue(value string) string {
	if len(value) <= toolVocabularyEchoMax {
		return value
	}
	cut := toolVocabularyEchoMax
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "..."
}

func toolVocabularyError(target, field, value, pattern string) error {
	return fmt.Errorf("%s %s %q does not match %s", target, field, boundedToolValue(value), pattern)
}

// validateChatToolVocabulary enforces the Chat target's name class on every
// tool and on a named tool choice.
func validateChatToolVocabulary(req *ir.Request) error {
	for _, tool := range req.Tools {
		if !chatToolNamePattern.MatchString(tool.Name) {
			return toolVocabularyError("chat", "tool name", tool.Name, "^[a-zA-Z0-9_-]{1,64}$")
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.Mode == ir.ToolChoiceTool &&
		!chatToolNamePattern.MatchString(req.ToolChoice.Name) {
		return toolVocabularyError("chat", "tool_choice.name", req.ToolChoice.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	return nil
}

// validateAnthropicToolVocabulary enforces the Anthropic target's name and id
// classes on declared tools, named choices, tool calls and tool results.
func validateAnthropicToolVocabulary(req *ir.Request) error {
	for _, tool := range req.Tools {
		if !anthropicToolNamePattern.MatchString(tool.Name) {
			return toolVocabularyError("anthropic", "tool name", tool.Name, "^[a-zA-Z0-9_-]{1,128}$")
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.Mode == ir.ToolChoiceTool &&
		!anthropicToolNamePattern.MatchString(req.ToolChoice.Name) {
		return toolVocabularyError("anthropic", "tool_choice.name", req.ToolChoice.Name, "^[a-zA-Z0-9_-]{1,128}$")
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case ir.BlockToolCall:
				if block.ToolCall == nil {
					continue
				}
				if !anthropicToolNamePattern.MatchString(block.ToolCall.Name) {
					return toolVocabularyError("anthropic", "tool name", block.ToolCall.Name, "^[a-zA-Z0-9_-]{1,128}$")
				}
				if !anthropicToolIDPattern.MatchString(block.ToolCall.ID) {
					return toolVocabularyError("anthropic", "tool_use.id", block.ToolCall.ID, "^[a-zA-Z0-9_-]+$")
				}
			case ir.BlockToolResult:
				if block.ToolResult == nil {
					continue
				}
				if !anthropicToolIDPattern.MatchString(block.ToolResult.CallID) {
					return toolVocabularyError("anthropic", "tool_use_id", block.ToolResult.CallID, "^[a-zA-Z0-9_-]+$")
				}
			}
		}
	}
	return nil
}
