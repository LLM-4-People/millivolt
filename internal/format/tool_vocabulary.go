package format

import (
	"fmt"
	"regexp"

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

func toolVocabularyError(target, field, value, pattern string) error {
	return fmt.Errorf("%s %s %q does not match %s", target, field, boundedEcho(value), pattern)
}

// validateChatToolVocabulary enforces the Chat target's name class on every
// declared tool, a named tool choice, and every history tool call. Chat tool
// ids are not pattern-constrained, so only the name is checked.
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
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == ir.BlockToolCall {
				if err := validateChatToolCall(block.ToolCall); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateChatToolCall enforces the Chat target's name class on one tool call.
// The request and response encoders share it so a call the target would reject
// fails at the same owner instead of reaching the wire. Chat tool ids have no
// pattern, so only the name is validated.
func validateChatToolCall(tc *ir.ToolCall) error {
	if tc == nil {
		return nil
	}
	if !chatToolNamePattern.MatchString(tc.Name) {
		return toolVocabularyError("chat", "tool name", tc.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	return nil
}

// validateAnthropicToolCall enforces the Anthropic target's name and id
// classes on one tool call. The request encoders and the response encoders
// share it so a call the target would reject fails at the same owner instead
// of reaching the wire.
func validateAnthropicToolCall(tc *ir.ToolCall) error {
	if tc == nil {
		return nil
	}
	if !anthropicToolNamePattern.MatchString(tc.Name) {
		return toolVocabularyError("anthropic", "tool name", tc.Name, "^[a-zA-Z0-9_-]{1,128}$")
	}
	if !anthropicToolIDPattern.MatchString(tc.ID) {
		return toolVocabularyError("anthropic", "tool_use.id", tc.ID, "^[a-zA-Z0-9_-]+$")
	}
	return nil
}

// validateAnthropicToolResult enforces the Anthropic target's id class on one
// tool result reference.
func validateAnthropicToolResult(tr *ir.ToolResult) error {
	if tr == nil {
		return nil
	}
	if !anthropicToolIDPattern.MatchString(tr.CallID) {
		return toolVocabularyError("anthropic", "tool_use_id", tr.CallID, "^[a-zA-Z0-9_-]+$")
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
				if err := validateAnthropicToolCall(block.ToolCall); err != nil {
					return err
				}
			case ir.BlockToolResult:
				if err := validateAnthropicToolResult(block.ToolResult); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateAnthropicResponseToolVocabulary applies the same tool_use vocabulary
// to a decoded assistant response before any target wire is emitted.
func validateAnthropicResponseToolVocabulary(resp *ir.Response) error {
	for _, block := range resp.Blocks {
		if block.Type != ir.BlockToolCall {
			continue
		}
		if err := validateAnthropicToolCall(block.ToolCall); err != nil {
			return err
		}
	}
	return nil
}
