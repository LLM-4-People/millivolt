package format

import (
	"encoding/json"
	"errors"
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
	// openAIIdentifierPattern is the shared OpenAI identifier class: Chat
	// function names and the json_schema response_format name both document
	// 1-64 characters of this set.
	openAIIdentifierPattern  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	anthropicToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	anthropicToolIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func toolVocabularyError(target, field, value, pattern string) error {
	return fmt.Errorf("%s %s %q does not match %s", target, field, boundedEcho(value), pattern)
}

// checkToolCallName enforces the IR's non-empty tool-name invariant at the
// streaming decoder owners. Each target encoder still owns its name-class
// check; this owner keeps an empty streamed name from reaching any target
// wire, where a per-target encoder might only notice it after bytes were
// already emitted.
func checkToolCallName(name string) error {
	if name == "" {
		return errors.New("tool name is required")
	}
	return nil
}

// validateChatToolVocabulary enforces the Chat target's name class on every
// declared tool, a named tool choice, and every history tool call. Chat tool
// ids are not pattern-constrained, so only the name is checked.
func validateChatToolVocabulary(req *ir.Request) error {
	for _, tool := range req.Tools {
		if !openAIIdentifierPattern.MatchString(tool.Name) {
			return toolVocabularyError("chat", "tool name", tool.Name, "^[a-zA-Z0-9_-]{1,64}$")
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.Mode == ir.ToolChoiceTool &&
		!openAIIdentifierPattern.MatchString(req.ToolChoice.Name) {
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
	if !openAIIdentifierPattern.MatchString(tc.Name) {
		return toolVocabularyError("chat", "tool name", tc.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	return nil
}

// validateResponsesToolVocabulary enforces the Responses request-input bound on
// history tool references. Responses FunctionCallOutputItemParam.call_id is
// 1..64 characters; the output FunctionToolCall.call_id has no such bound, so
// this is a request-only rule. Chat has no id length limit, and a longer id
// cannot be shortened without changing the client-visible identity, so the
// request fails closed instead of emitting a wire the target rejects.
func validateResponsesToolVocabulary(req *ir.Request) error {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case ir.BlockToolCall:
				if block.ToolCall != nil {
					if err := validateResponsesToolReference("tool call id", block.ToolCall.ID); err != nil {
						return err
					}
				}
			case ir.BlockToolResult:
				if block.ToolResult != nil {
					if err := validateResponsesToolReference("tool result call id", block.ToolResult.CallID); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateResponsesToolReference(field, id string) error {
	// JSON Schema maxLength counts characters (RFC 8259 code points), not
	// UTF-8 bytes. A 64-rune multibyte id is valid even though it is longer
	// than 64 bytes.
	if n := utf8.RuneCountInString(id); n >= 1 && n <= 64 {
		return nil
	}
	return fmt.Errorf("responses %s %q must be 1..64 characters", field, boundedEcho(id))
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

// toolAllowedCallers validates the allowed_callers knob shared by the
// Responses and Anthropic tool decoders. The IR and every target can invoke a
// tool only directly, so a supplied list must still permit "direct"; a
// programmatic-only list would otherwise become directly callable when the
// knob is dropped. present reports a non-null value so the caller records the
// loss. A malformed list fails closed instead of silently widening the tool.
func toolAllowedCallers(raw json.RawMessage) (present bool, err error) {
	if !wireValuePresent(raw) {
		return false, nil
	}
	// The outer tools array is already bounded, but one tool can carry this
	// array alone at the request-body cap. Bound it lexically before the
	// typed []string decode materializes it.
	if err := checkWireArray(raw, "allowed_callers", WireCollectionMax); err != nil {
		return true, err
	}
	var callers []string
	if err := json.Unmarshal(raw, &callers); err != nil {
		return true, fmt.Errorf("allowed_callers: %w", err)
	}
	for _, caller := range callers {
		if caller == "direct" {
			return true, nil
		}
	}
	return true, errors.New("allowed_callers excludes direct calls, which the target requires")
}

// validateResponseFormatName enforces the OpenAI identifier class on a
// json_schema response-format name. An empty name is legal: both encoders
// synthesize the documented "response" default. A name the target would
// reject fails closed at the request boundary.
func validateResponseFormatName(target string, rf *ir.ResponseFormat) error {
	if rf == nil || rf.Type != "json_schema" || rf.Name == "" {
		return nil
	}
	if !openAIIdentifierPattern.MatchString(rf.Name) {
		return toolVocabularyError(target, "response_format.name", rf.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	return nil
}
