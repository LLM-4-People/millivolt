package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// decodeLegacyChatFunctions converts the deprecated top-level Chat "functions"
// array through the same validator as modern tools.
func decodeLegacyChatFunctions(raw json.RawMessage) ([]ir.Tool, error) {
	var fns []chatLegacyFunction
	if err := json.Unmarshal(raw, &fns); err != nil {
		return nil, errParseOpenAIBody(fmt.Errorf("parse functions: %w", err))
	}
	tools := make([]chatWireTool, 0, len(fns))
	for _, fn := range fns {
		t := chatWireTool{Type: "function"}
		t.Function.Name = fn.Name
		t.Function.Description = fn.Description
		t.Function.Parameters = fn.Parameters
		tools = append(tools, t)
	}
	return decodeChatTools(tools)
}

// decodeLegacyFunctionCall converts the deprecated top-level Chat
// "function_call" selector into the canonical tool choice.
func decodeLegacyFunctionCall(raw json.RawMessage) (*ir.ToolChoice, error) {
	var shorthand string
	if err := json.Unmarshal(raw, &shorthand); err == nil {
		switch shorthand {
		case "auto":
			return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}, nil
		case "none":
			return &ir.ToolChoice{Mode: ir.ToolChoiceNone}, nil
		case "required":
			return &ir.ToolChoice{Mode: ir.ToolChoiceRequired}, nil
		}
		return nil, errParseOpenAIBody(fmt.Errorf("unsupported function_call %q", shorthand))
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || strings.TrimSpace(obj.Name) == "" {
		return nil, errParseOpenAIBody(errors.New("function_call needs a name"))
	}
	return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Name}, nil
}

// decodeLegacyMessageFunctionCall converts the deprecated per-assistant
// "function_call" object. The legacy protocol carried no call id, and the
// matching role:"function" reply identifies the call by function name, so the
// name is the id that pairs the two turns. This is the one documented exception
// to the request-scoped id namespace: a source with no id cannot be checked for
// uniqueness, and repeated same-name legacy calls are inherently ambiguous.
func decodeLegacyMessageFunctionCall(raw json.RawMessage, requireEvidence bool) (*ir.ToolCall, error) {
	var obj struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errParseOpenAIBody(fmt.Errorf("parse function_call: %w", err))
	}
	if strings.TrimSpace(obj.Name) == "" {
		return nil, errParseOpenAIBody(errors.New("function_call needs a name"))
	}
	if requireEvidence && toolArgumentsMissing(obj.Arguments) {
		return nil, errParseOpenAIBody(errToolArgumentsMissing)
	}
	args, err := parseToolArguments(obj.Arguments)
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}
	return &ir.ToolCall{ID: obj.Name, Name: obj.Name, Arguments: args}, nil
}

// chatToolCalls owns the pairing state for Chat assistant tool calls. It tracks
// every call, explicit and generated, because a result id must never be matched
// against another call's function name, and an empty reference is ambiguous as
// soon as a second call is unresolved.
type chatToolCalls struct {
	calls []chatToolCallRef
}

type chatToolCallRef struct {
	id        string
	name      string
	generated bool
	answered  bool
}

func (c *chatToolCalls) add(id, name string, generated bool) {
	c.calls = append(c.calls, chatToolCallRef{id: id, name: name, generated: generated})
}

// consumeID resolves a result's tool_call_id. An exact id is authoritative: an
// unanswered call carrying that id is answered even when another call uses the
// same string as its function name. When no id matches, a generated call may
// still be addressed by the function name its client supplied in place of the
// missing id; only a unique such name resolves, and an explicit call's name is
// never reinterpreted as an id.
func (c *chatToolCalls) consumeID(reference string) (string, error) {
	knownID := false
	for i := range c.calls {
		if c.calls[i].id != reference {
			continue
		}
		if !c.calls[i].answered {
			c.calls[i].answered = true
			return reference, nil
		}
		knownID = true
	}
	if knownID {
		// The reference is an id this history already answered. A duplicate
		// result stays as supplied rather than falling back to a generated
		// call that happens to use the same string as its function name.
		return reference, nil
	}
	generated := -1
	named := 0
	for i := range c.calls {
		if c.calls[i].answered || c.calls[i].name != reference {
			continue
		}
		named++
		if c.calls[i].generated {
			generated = i
		}
	}
	switch {
	case named > 1:
		return "", fmt.Errorf("tool_call_id %q matches more than one unresolved tool call", reference)
	case generated >= 0:
		c.calls[generated].answered = true
		return c.calls[generated].id, nil
	default:
		// An explicit call is answered through its id, never through its
		// function name alone; an unknown id also stays as supplied.
		return reference, nil
	}
}

// consumeName resolves a result's message name. A name is a function name, not
// an id: only a generated call is name-addressable, and a name shared by more
// than one unresolved call fails closed. An explicit call is answered through
// its id, so its name alone leaves the reference unchanged.
func (c *chatToolCalls) consumeName(reference string) (string, error) {
	generated := -1
	named := 0
	for i := range c.calls {
		if c.calls[i].answered || c.calls[i].name != reference {
			continue
		}
		named++
		if c.calls[i].generated {
			generated = i
		}
	}
	switch {
	case named > 1:
		return "", fmt.Errorf("tool result name %q matches more than one unresolved tool call", reference)
	case generated >= 0:
		c.calls[generated].answered = true
		return c.calls[generated].id, nil
	default:
		return reference, nil
	}
}

// consumeEmpty resolves a result with no tool_call_id and no name. It can only
// answer the one remaining generated call: any second unresolved call, explicit
// or generated, makes the empty reference ambiguous. With no unresolved calls
// the caller keeps its existing "tool message has no tool_call_id" rejection.
func (c *chatToolCalls) consumeEmpty() (string, error) {
	unanswered := 0
	generated := -1
	for i := range c.calls {
		if c.calls[i].answered {
			continue
		}
		unanswered++
		if c.calls[i].generated {
			generated = i
		}
	}
	if unanswered == 0 {
		return "", nil
	}
	if unanswered == 1 && generated >= 0 {
		c.calls[generated].answered = true
		return c.calls[generated].id, nil
	}
	return "", errors.New("tool message has no tool_call_id and does not identify a unique generated tool call")
}

// decodeChatToolCall decodes one assistant tool call. The bool reports that the
// wire id was missing and synthesized, so the request decoder can reconcile a
// later tool reply that names the call instead of carrying that id.
// requireEvidence rejects a missing, null or whitespace argument value; the
// response decoder sets it for a non-clean terminal.
func decodeChatToolCall(tc chatWireToolCall, msgIndex, callIndex int, requireEvidence bool, ids *requestToolIDs) (*ir.ToolCall, bool, ir.Warnings, error) {
	var warns ir.Warnings
	id := tc.ID
	generated := false
	if id == "" {
		id = ids.synthesize(fmt.Sprintf("call_%d_%d", msgIndex, callIndex))
		generated = true
		warns.Add(ir.WarningApproximated, "tool_calls.id", "generated a missing call id")
	}
	if err := checkNativeIdentity("tool id", id); err != nil {
		return nil, false, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
	}
	if !generated {
		if err := ids.reserve(id); err != nil {
			return nil, false, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
		}
	}
	if tc.Function.Name == "" {
		return nil, false, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: function name is required", msgIndex, callIndex))
	}
	if err := checkNativeIdentity("tool name", tc.Function.Name); err != nil {
		return nil, false, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
	}
	// Structural identity failures stay fatal even when the source also
	// omitted the argument value; only a well-formed call can be dropped for
	// unusable arguments.
	if requireEvidence && toolArgumentsMissing(tc.Function.Arguments) {
		return nil, false, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, errToolArgumentsMissing))
	}
	args, err := parseToolArguments(tc.Function.Arguments)
	if err != nil {
		return nil, false, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
	}
	return &ir.ToolCall{ID: id, Name: tc.Function.Name, Arguments: args}, generated, warns, nil
}
