package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Chat Completions wire codec. DecodeChatRequest/EncodeChatRequest and
// DecodeChatResponse/EncodeChatResponse are the only owners of the Chat wire
// shape; every other codec speaks ir. The decoder is deliberately strict about
// structural features it cannot represent (non-function tools, invalid tool
// arguments) and records a warning for optional knobs a target protocol may
// not support.

// chatWireRequest is the request shape decoded from the Chat wire.
type chatWireRequest struct {
	Model               string             `json:"model"`
	System              string             `json:"system"`
	Messages            []chatWireMessage  `json:"messages"`
	MaxTokens           *int               `json:"max_tokens"`
	MaxCompletionTokens *int               `json:"max_completion_tokens"`
	Temperature         *float64           `json:"temperature"`
	TopP                *float64           `json:"top_p"`
	Stop                json.RawMessage    `json:"stop"`
	Seed                *int64             `json:"seed"`
	PresencePenalty     *float64           `json:"presence_penalty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty"`
	LogitBias           map[string]float64 `json:"logit_bias"`
	Logprobs            *bool              `json:"logprobs"`
	TopLogprobs         *int               `json:"top_logprobs"`
	Tools               []chatWireTool     `json:"tools"`
	ToolChoice          json.RawMessage    `json:"tool_choice"`
	ParallelToolCalls   *bool              `json:"parallel_tool_calls"`
	ResponseFormat      json.RawMessage    `json:"response_format"`
	ReasoningEffort     string             `json:"reasoning_effort"`
	Stream              bool               `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Metadata         map[string]json.RawMessage `json:"metadata"`
	ServiceTier      string                     `json:"service_tier"`
	User             string                     `json:"user"`
	Verbosity        string                     `json:"verbosity"`
	SafetyIdentifier string                     `json:"safety_identifier"`
	PromptCacheKey   string                     `json:"prompt_cache_key"`
	Store            *bool                      `json:"store"`
	N                *int                       `json:"n"`

	// Legacy and provider-specific request fields that the canonical model
	// does not carry. Present values are reported or converted below.
	Functions        json.RawMessage `json:"functions"`
	FunctionCall     json.RawMessage `json:"function_call"`
	Prediction       json.RawMessage `json:"prediction"`
	Audio            json.RawMessage `json:"audio"`
	Modalities       []string        `json:"modalities"`
	WebSearchOptions json.RawMessage `json:"web_search_options"`
}

type chatWireMessage struct {
	Role             string               `json:"role"`
	Content          json.RawMessage      `json:"content"`
	Name             string               `json:"name"`
	ToolCalls        []chatWireToolCall   `json:"tool_calls"`
	ToolCallID       string               `json:"tool_call_id"`
	Refusal          *string              `json:"refusal"`
	FunctionCall     json.RawMessage      `json:"function_call"`
	ReasoningContent string               `json:"reasoning_content"`
	Reasoning        string               `json:"reasoning"`
	Annotations      []chatWireAnnotation `json:"annotations"`
}

// chatWireAnnotation is the Chat message-level url_citation wrapper. Responses
// spells the same fields flat on the output_text part.
type chatWireAnnotation struct {
	Type        string `json:"type"`
	URLCitation *struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		StartIndex int    `json:"start_index"`
		EndIndex   int    `json:"end_index"`
	} `json:"url_citation"`
}

// chatLegacyFunction is one deprecated Chat "functions" entry: the function
// object without the modern tools wrapper.
type chatLegacyFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatWireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatWireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
}

// DecodeChatRequest converts one Chat Completions request body into the
// canonical request. The whole body must be valid UTF-8: a raw invalid byte is
// rejected before the typed decode so the decoder's replacement rune cannot
// expand a body-sized string.
func DecodeChatRequest(body []byte) (*ir.Request, error) {
	if !utf8.Valid(body) {
		return nil, errParseOpenAIBody(errors.New("request body is not valid UTF-8"))
	}
	// Bound the collections before the typed decode materializes them. The
	// proxy preflights the wire body too, but the codec is reachable on its
	// own (and the preflight deliberately skips malformed documents).
	var bounds struct {
		Messages  json.RawMessage `json:"messages"`
		Tools     json.RawMessage `json:"tools"`
		Functions json.RawMessage `json:"functions"`
		Stop      json.RawMessage `json:"stop"`
		Metadata  json.RawMessage `json:"metadata"`
		LogitBias json.RawMessage `json:"logit_bias"`
	}
	if err := json.Unmarshal(body, &bounds); err != nil {
		return nil, errParseOpenAIBody(err)
	}
	for _, b := range []struct {
		raw   json.RawMessage
		field string
	}{
		{bounds.Messages, "messages"},
		{bounds.Tools, "tools"},
		{bounds.Functions, "functions"},
		{bounds.Stop, "stop"},
	} {
		if err := checkWireArray(b.raw, b.field, WireCollectionMax); err != nil {
			return nil, errParseOpenAIBody(err)
		}
	}
	for _, b := range []struct {
		raw   json.RawMessage
		field string
	}{
		{bounds.Metadata, "metadata"},
		{bounds.LogitBias, "logit_bias"},
	} {
		if err := checkWireObject(b.raw, b.field, WireCollectionMax); err != nil {
			return nil, errParseOpenAIBody(err)
		}
	}
	// Bound the aggregate tool-call members of every message. Each member
	// expands into a typed call and, on a translated wire, a content block; the
	// proxy preflights the same aggregate so both paths accept the same body.
	if len(bounds.Messages) > 0 {
		var messages []struct {
			ToolCalls json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(bounds.Messages, &messages); err != nil {
			return nil, errParseOpenAIBody(err)
		}
		total := 0
		for i, message := range messages {
			if err := addWireArray(&total, message.ToolCalls, fmt.Sprintf("messages[%d].tool_calls", i), WireToolCallsMax); err != nil {
				return nil, errParseOpenAIBody(err)
			}
		}
	}
	// Message-level citation annotations are decoded into IR citations, so
	// every array keeps ContentPartMax and all arrays share one aggregate.
	if err := checkChatMessageAnnotations(bounds.Messages, "messages"); err != nil {
		return nil, errParseOpenAIBody(err)
	}
	var in chatWireRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, errParseOpenAIBody(err)
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, errParseOpenAIBody(errors.New("model is required"))
	}

	var warns ir.Warnings
	req := &ir.Request{
		Model:             in.Model,
		MaxOutputTokens:   MergeTokenCaps(in.MaxCompletionTokens, in.MaxTokens),
		Temperature:       in.Temperature,
		TopP:              in.TopP,
		Seed:              in.Seed,
		PresencePenalty:   in.PresencePenalty,
		FrequencyPenalty:  in.FrequencyPenalty,
		LogitBias:         in.LogitBias,
		Logprobs:          in.Logprobs,
		TopLogprobs:       in.TopLogprobs,
		ParallelToolCalls: in.ParallelToolCalls,
		Stream:            in.Stream,
		ServiceTier:       in.ServiceTier,
		Store:             in.Store,
		User:              in.User,
		Verbosity:         in.Verbosity,
		SafetyIdentifier:  in.SafetyIdentifier,
		PromptCacheKey:    in.PromptCacheKey,
	}
	if in.StreamOptions != nil {
		req.StreamIncludeUsage = in.StreamOptions.IncludeUsage
	}
	if len(in.Metadata) > 0 {
		// Keep these warnings in the shared accumulator so the single final
		// append below emits each one once; metadata is decoded before the
		// message and parameter warnings share the same slice.
		req.Metadata, warns = decodeChatMetadata(in.Metadata)
	}
	if stops := parseStop(in.Stop); len(stops) > 0 {
		req.Stop = stops
	}
	if in.N != nil && *in.N > 1 {
		req.Warnings.Add(ir.WarningDropped, "n", "only one choice can be translated")
	}
	if in.ReasoningEffort != "" {
		req.Reasoning = &ir.ReasoningConfig{Effort: in.ReasoningEffort}
	}
	if rf, warn, err := decodeChatResponseFormat(in.ResponseFormat); err != nil {
		return nil, err
	} else if rf != nil {
		req.ResponseFormat = rf
		if warn != "" {
			req.Warnings.Add(ir.WarningDropped, "response_format", warn)
		}
	}
	if len(in.Tools) > 0 {
		tools, err := decodeChatTools(in.Tools)
		if err != nil {
			return nil, err
		}
		req.Tools = tools
	}
	if rawPresent(in.Functions) {
		tools, err := decodeLegacyChatFunctions(in.Functions)
		if err != nil {
			return nil, err
		}
		req.Tools = append(req.Tools, tools...)
	}
	if tc, err := decodeChatToolChoice(in.ToolChoice); err != nil {
		return nil, err
	} else if tc != nil {
		req.ToolChoice = tc
	}
	if rawPresent(in.FunctionCall) {
		if req.ToolChoice != nil {
			req.Warnings.Add(ir.WarningDropped, "function_call", "ignored because tool_choice is present")
		} else if tc, err := decodeLegacyFunctionCall(in.FunctionCall); err != nil {
			return nil, err
		} else {
			req.ToolChoice = tc
		}
	}
	if rawPresent(in.Prediction) {
		req.Warnings.Add(ir.WarningDropped, "prediction", "predicted output has no cross-protocol equivalent")
	}
	if rawPresent(in.Audio) {
		req.Warnings.Add(ir.WarningDropped, "audio", "audio output has no cross-protocol equivalent")
	}
	if len(in.Modalities) > 0 {
		for _, m := range in.Modalities {
			if m != "text" {
				req.Warnings.Add(ir.WarningDropped, "modalities", "only text output has a cross-protocol equivalent")
				break
			}
		}
	}
	if rawPresent(in.WebSearchOptions) {
		req.Warnings.Add(ir.WarningDropped, "web_search_options", "provider-hosted web search has no cross-protocol equivalent")
	}

	msgs, msgWarns, err := decodeChatMessages(in.Messages)
	if err != nil {
		return nil, err
	}
	warns = append(warns, msgWarns...)
	if in.System != "" {
		msgs = append([]ir.Message{{Role: ir.RoleSystem, Content: []ir.Block{ir.TextBlock(in.System)}}}, msgs...)
	}
	req.Messages = msgs
	req.Warnings = append(req.Warnings, warns...)
	if err := ir.ValidateRequest(req); err != nil {
		return nil, errParseOpenAIBody(err)
	}
	return req, nil
}

// ContentString returns the message content as a single string when the
// content is a plain string (the common case).
func (m chatWireMessage) ContentString() (string, bool) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, true
	}
	return "", false
}

// ContentText accepts both the string form and the OpenAI content-array form,
// joining text parts in order. Other part types are ignored rather than
// stringified; callers that require structured content use the raw message.
// An array over metrics.ContentPartMax is skipped rather than decoded, so one
// hostile message cannot expand into a body-sized typed slice.
func (m chatWireMessage) ContentText() (string, bool) {
	if !metrics.ContentPartsWithinLimit(m.Content) {
		return "", false
	}
	if s, ok := m.ContentString(); ok {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(part.Text)
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// parseStop normalizes the Chat stop field's string or array form.
func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

// decodeChatMetadata keeps the string-valued metadata entries and warns about
// the rest; a non-string value is a client extension, not a reason to reject
// the whole request.
func decodeChatMetadata(in map[string]json.RawMessage) (map[string]string, ir.Warnings) {
	var warns ir.Warnings
	out := make(map[string]string, len(in))
	// Sorted keys make the warning order deterministic; a map ranging order
	// would make the client-visible warning header depend on the run.
	for _, k := range slices.Sorted(maps.Keys(in)) {
		raw := in[k]
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out[k] = s
			continue
		}
		warns.Add(ir.WarningDropped, "metadata."+k, "non-string metadata value")
	}
	if len(out) == 0 {
		return nil, warns
	}
	return out, warns
}

// decodeChatMessages converts Chat messages into the canonical message list.
// Consecutive role:"tool" messages (and a following user turn) are merged into
// one user message whose tool_result blocks come first: that is the shape
// Anthropic requires, and the Chat encoder splits it back out losslessly.
func decodeChatMessages(in []chatWireMessage) ([]ir.Message, ir.Warnings, error) {
	var (
		out     []ir.Message
		warns   ir.Warnings
		pending *ir.Message
	)
	flush := func() {
		if pending != nil {
			out = append(out, *pending)
			pending = nil
		}
	}
	for i, m := range in {
		switch m.Role {
		case "system", "developer":
			flush()
			blocks, w, err := decodeChatContent(m.Content, ir.RoleSystem)
			if err != nil {
				return nil, nil, fmt.Errorf("message %d: %w", i, err)
			}
			warns = append(warns, w...)
			role := ir.RoleSystem
			if m.Role == "developer" {
				role = ir.RoleDeveloper
			}
			out = append(out, ir.Message{Role: role, Content: blocks, Name: m.Name})
		case "user":
			blocks, w, err := decodeChatContent(m.Content, ir.RoleUser)
			if err != nil {
				return nil, nil, fmt.Errorf("message %d: %w", i, err)
			}
			warns = append(warns, w...)
			if pending != nil {
				pending.Content = append(pending.Content, blocks...)
				continue
			}
			out = append(out, ir.Message{Role: ir.RoleUser, Content: blocks, Name: m.Name})
		case "assistant":
			flush()
			blocks, w, err := decodeChatContent(m.Content, ir.RoleAssistant)
			if err != nil {
				return nil, nil, fmt.Errorf("message %d: %w", i, err)
			}
			warns = append(warns, w...)
			if reasoning := m.reasoningText(); reasoning != "" {
				// Reasoning precedes the visible answer in canonical order.
				blocks = append([]ir.Block{{
					Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: reasoning, Provider: ir.Chat},
				}}, blocks...)
			}
			if m.Refusal != nil && *m.Refusal != "" {
				blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Refusal: *m.Refusal})
			}
			for j, tc := range m.ToolCalls {
				call, w, err := decodeChatToolCall(tc, i, j)
				if err != nil {
					return nil, nil, err
				}
				warns = append(warns, w...)
				blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
			}
			if rawPresent(m.FunctionCall) {
				if len(m.ToolCalls) > 0 {
					warns.Add(ir.WarningDropped, "messages.function_call", "ignored because tool_calls is present")
				} else if call, err := decodeLegacyMessageFunctionCall(m.FunctionCall); err != nil {
					return nil, nil, fmt.Errorf("message %d: %w", i, err)
				} else {
					blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
				}
			}
			out = append(out, ir.Message{Role: ir.RoleAssistant, Content: blocks, Name: m.Name})
		case "tool", "function":
			blocks, w, err := decodeChatContent(m.Content, ir.RoleUser)
			if err != nil {
				return nil, nil, fmt.Errorf("message %d: %w", i, err)
			}
			warns = append(warns, w...)
			callID := m.ToolCallID
			if callID == "" {
				callID = m.Name
			}
			if callID == "" {
				return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d: tool message has no tool_call_id", i))
			}
			if pending == nil {
				pending = &ir.Message{Role: ir.RoleUser}
			}
			pending.Content = append(pending.Content, ir.Block{
				Type:       ir.BlockToolResult,
				ToolResult: &ir.ToolResult{CallID: callID, Content: blocks},
			})
		default:
			return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d: unsupported role %q", i, m.Role))
		}
	}
	flush()
	return out, warns, nil
}

// reasoningText returns the provider's hidden chain-of-thought spelling.
// reasoning_content is the Chat-compatible convention; some gateways use
// reasoning instead.
func (m chatWireMessage) reasoningText() string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

// rawPresent reports whether a JSON field was supplied with a non-null value.
func rawPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

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
// name is the id that pairs the two turns.
func decodeLegacyMessageFunctionCall(raw json.RawMessage) (*ir.ToolCall, error) {
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
	args, err := parseToolArguments(obj.Arguments)
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}
	return &ir.ToolCall{ID: obj.Name, Name: obj.Name, Arguments: args}, nil
}

func decodeChatToolCall(tc chatWireToolCall, msgIndex, callIndex int) (*ir.ToolCall, ir.Warnings, error) {
	var warns ir.Warnings
	args, err := parseToolArguments(tc.Function.Arguments)
	if err != nil {
		return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
	}
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("call_%d_%d", msgIndex, callIndex)
		warns.Add(ir.WarningApproximated, "tool_calls.id", "generated a missing call id")
	}
	if err := checkNativeIdentity("tool id", id); err != nil {
		return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
	}
	if tc.Function.Name == "" {
		return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: function name is required", msgIndex, callIndex))
	}
	if err := checkNativeIdentity("tool name", tc.Function.Name); err != nil {
		return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", msgIndex, callIndex, err))
	}
	return &ir.ToolCall{ID: id, Name: tc.Function.Name, Arguments: args}, warns, nil
}

// parseToolArguments normalizes a Chat tool-call argument string into a JSON
// object. Empty and null become {}, which is what every target protocol needs
// for a zero-argument call.
func parseToolArguments(raw string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, errors.New("tool call arguments must be valid JSON")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("tool call arguments must be a JSON object")
	}
	return json.RawMessage(trimmed), nil
}

// decodeChatContent converts a Chat content value (string, null, or part
// array) into canonical blocks.
func decodeChatContent(raw json.RawMessage, role ir.Role) ([]ir.Block, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			if role == ir.RoleSystem || role == ir.RoleDeveloper {
				// An empty system fragment still contributes its separator
				// when fragments are joined.
				return []ir.Block{ir.TextBlock("")}, nil, nil
			}
			return nil, nil, nil
		}
		return []ir.Block{ir.TextBlock(text)}, nil, nil
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		var warns ir.Warnings
		warns.Add(ir.WarningDropped, "content", "content part array exceeds the translation limit")
		return nil, warns, nil
	}
	var parts []struct {
		Type       string          `json:"type"`
		Text       string          `json:"text"`
		Refusal    string          `json:"refusal"`
		ImageURL   json.RawMessage `json:"image_url"`
		InputAudio *struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
		File *struct {
			FileID   string `json:"file_id"`
			FileData string `json:"file_data"`
			Filename string `json:"filename"`
		} `json:"file"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, err
	}
	var (
		out   []ir.Block
		warns ir.Warnings
	)
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, ir.TextBlock(p.Text))
		case "refusal":
			out = append(out, ir.Block{Type: ir.BlockRefusal, Refusal: p.Refusal})
		case "image_url":
			if role == ir.RoleSystem || role == ir.RoleDeveloper {
				warns.Add(ir.WarningDropped, "content.image_url", "system messages are text-only")
				continue
			}
			img, err := decodeChatImage(p.ImageURL)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, ir.Block{Type: ir.BlockImage, Image: img})
		case "input_audio":
			if role == ir.RoleSystem || role == ir.RoleDeveloper {
				warns.Add(ir.WarningDropped, "content.input_audio", "system messages are text-only")
				continue
			}
			if p.InputAudio == nil {
				warns.Add(ir.WarningDropped, "content.input_audio", "missing payload")
				continue
			}
			out = append(out, ir.Block{Type: ir.BlockAudio, Audio: &ir.Audio{Data: p.InputAudio.Data, Format: p.InputAudio.Format}})
		case "file":
			if role == ir.RoleSystem || role == ir.RoleDeveloper {
				warns.Add(ir.WarningDropped, "content.file", "system messages are text-only")
				continue
			}
			if p.File == nil {
				warns.Add(ir.WarningDropped, "content.file", "missing payload")
				continue
			}
			file := &ir.File{FileID: p.File.FileID, FileData: p.File.FileData, Filename: p.File.Filename}
			normalizeFileData(file)
			// Chat documents inline file_data as raw base64 and supports PDF
			// file parts only, so a value with no data-URL media type is a PDF.
			if file.FileData != "" && file.MediaType == "" {
				file.MediaType = "application/pdf"
			}
			out = append(out, ir.Block{Type: ir.BlockFile, File: file})
		default:
			warns.Add(ir.WarningDropped, "content."+p.Type, "unsupported content part")
		}
	}
	return out, warns, nil
}

// decodeChatAnnotations maps the message-level URL citation wrapper onto the
// IR citation fields. Other annotation types have no IR representation.
func decodeChatAnnotations(in []chatWireAnnotation) ([]ir.Citation, ir.Warnings) {
	var (
		out   []ir.Citation
		warns ir.Warnings
	)
	for _, a := range in {
		if a.Type != "url_citation" || a.URLCitation == nil {
			field := "annotations"
			if a.Type != "" {
				field += "." + a.Type
			}
			warns.Add(ir.WarningDropped, field, "annotation has no cross-protocol equivalent")
			continue
		}
		if a.URLCitation.URL == "" {
			warns.Add(ir.WarningDropped, "annotations.url_citation", "url_citation has no url")
			continue
		}
		out = append(out, ir.Citation{
			Type: "url_citation", URL: a.URLCitation.URL, Title: a.URLCitation.Title,
			StartIndex: a.URLCitation.StartIndex, EndIndex: a.URLCitation.EndIndex,
		})
	}
	return out, warns
}

func decodeChatImage(raw json.RawMessage) (*ir.Image, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("image_url part has no image_url")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		img := &ir.Image{URL: s}
		normalizeImageURL(img)
		return img, nil
	}
	var obj struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj.URL == "" {
		return nil, errors.New("image_url part has no url")
	}
	img := &ir.Image{URL: obj.URL, Detail: obj.Detail}
	normalizeImageURL(img)
	return img, nil
}

func decodeChatTools(in []chatWireTool) ([]ir.Tool, error) {
	out := make([]ir.Tool, 0, len(in))
	for i, t := range in {
		if t.Type != "" && t.Type != "function" {
			return nil, errParseOpenAIBody(fmt.Errorf("tool %d: unsupported type %q", i, t.Type))
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			return nil, errParseOpenAIBody(fmt.Errorf("tool %d: function name is required", i))
		}
		params := t.Function.Parameters
		if len(params) == 0 || bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
			params = json.RawMessage(`{}`)
		}
		out = append(out, ir.Tool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
			Strict:      t.Function.Strict,
		})
	}
	return out, nil
}

func decodeChatToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var shorthand string
	if err := json.Unmarshal(raw, &shorthand); err == nil {
		switch shorthand {
		case "auto":
			return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}, nil
		case "none":
			return &ir.ToolChoice{Mode: ir.ToolChoiceNone}, nil
		case "required":
			return &ir.ToolChoice{Mode: ir.ToolChoiceRequired}, nil
		case "":
			return nil, nil
		}
		return nil, errParseOpenAIBody(fmt.Errorf("unsupported tool_choice %q", shorthand))
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name json.RawMessage `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errParseOpenAIBody(err)
	}
	if obj.Type != "" && obj.Type != "function" {
		// allowed_tools names an explicit subset of tools. Dropping it would
		// let the model call tools the client excluded, so refuse instead.
		return nil, errParseOpenAIBody(fmt.Errorf(
			"tool_choice type %q cannot be translated without broadening tool access", obj.Type))
	}
	if obj.Function == nil {
		return nil, errParseOpenAIBody(errors.New("tool_choice object has no function"))
	}
	tc := &ir.ToolChoice{Mode: ir.ToolChoiceTool}
	if len(obj.Function.Name) == 0 || !json.Valid(obj.Function.Name) {
		return nil, errParseOpenAIBody(errors.New("tool_choice.function.name must be a string"))
	}
	var name string
	if err := json.Unmarshal(obj.Function.Name, &name); err != nil || strings.TrimSpace(name) == "" {
		return nil, errParseOpenAIBody(errors.New("tool_choice.function.name must be a non-empty string"))
	}
	tc.Name = name
	return tc, nil
}

func decodeChatResponseFormat(raw json.RawMessage) (*ir.ResponseFormat, string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, "", nil
	}
	var obj struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "", errParseOpenAIBody(err)
	}
	switch obj.Type {
	case "", "text":
		return &ir.ResponseFormat{Type: "text"}, "", nil
	case "json_object":
		return &ir.ResponseFormat{Type: "json_object"}, "", nil
	case "json_schema":
		if obj.JSONSchema == nil || len(obj.JSONSchema.Schema) == 0 {
			return nil, "", errParseOpenAIBody(errors.New("response_format.json_schema.schema is required"))
		}
		return &ir.ResponseFormat{
			Type:        "json_schema",
			Name:        obj.JSONSchema.Name,
			Description: obj.JSONSchema.Description,
			Schema:      obj.JSONSchema.Schema,
			Strict:      obj.JSONSchema.Strict,
		}, "", nil
	default:
		return nil, "unsupported response_format type " + obj.Type, nil
	}
}

// EncodeChatRequest renders the canonical request as a Chat Completions body.
func EncodeChatRequest(req *ir.Request) ([]byte, error) {
	if err := ir.ValidateRequest(req); err != nil {
		return nil, err
	}
	if err := validateChatToolVocabulary(req); err != nil {
		return nil, err
	}
	if req.PreviousResponseID != "" {
		return nil, errors.New("chat target cannot resolve previous_response_id")
	}
	messages := req.Messages
	if req.Instructions != nil && *req.Instructions != "" {
		messages = append([]ir.Message{{
			Role: ir.RoleSystem, Content: []ir.Block{ir.TextBlock(*req.Instructions)},
		}}, messages...)
	}
	if req.TopK != nil {
		req.Warnings.Add(ir.WarningDropped, "top_k", "Chat Completions has no top_k")
	}
	if req.CacheControl != nil {
		req.Warnings.Add(ir.WarningDropped, "cache_control", "Chat Completions has no request-level cache breakpoint")
	}
	if req.Reasoning != nil {
		if req.Reasoning.Summary != "" {
			req.Warnings.Add(ir.WarningDropped, "reasoning.summary", "Chat Completions has no reasoning summary")
		}
		if req.Reasoning.BudgetTokens != nil {
			req.Warnings.Add(ir.WarningApproximated, "reasoning.budget_tokens", "Chat Completions carries only reasoning_effort")
		}
		if req.Reasoning.Display != "" {
			req.Warnings.Add(ir.WarningDropped, "reasoning.display", "Chat Completions has no reasoning display mode")
		}
		if req.Reasoning.Context != "" {
			req.Warnings.Add(ir.WarningDropped, "reasoning.context", "Chat Completions has no reasoning context")
		}
		if req.Reasoning.Mode != "" {
			req.Warnings.Add(ir.WarningDropped, "reasoning.mode", "Chat Completions has no reasoning mode")
		}
	}
	out := map[string]any{
		"model":    req.Model,
		"messages": encodeChatMessages(messages, &req.Warnings),
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		stops := req.Stop
		if len(stops) > ChatMaxStopSequences {
			// Chat documents at most four stop sequences.
			stops = stops[:ChatMaxStopSequences]
			req.Warnings.Add(ir.WarningApproximated, "stop", "Chat Completions accepts at most four stop sequences")
		}
		out["stop"] = stops
	}
	if req.Seed != nil {
		out["seed"] = *req.Seed
	}
	if req.PresencePenalty != nil {
		out["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		out["frequency_penalty"] = *req.FrequencyPenalty
	}
	if len(req.LogitBias) > 0 {
		out["logit_bias"] = req.LogitBias
	}
	if req.Logprobs != nil {
		out["logprobs"] = *req.Logprobs
		if *req.Logprobs {
			// The request side maps, but the canonical response does not retain
			// provider logprob details, so the translated output cannot carry
			// the item the client asked for.
			req.Warnings.Add(ir.WarningApproximated, "logprobs", "translated responses do not carry logprob details")
		}
	}
	if req.TopLogprobs != nil {
		if req.Logprobs != nil && *req.Logprobs {
			out["top_logprobs"] = *req.TopLogprobs
		} else {
			// Chat documents top_logprobs only with logprobs=true. A
			// Responses source can request the count without the include, so
			// forwarding it alone would produce an invalid Chat request.
			req.Warnings.Add(ir.WarningDropped, "top_logprobs", "Chat requires logprobs=true")
		}
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.Tools) > 0 {
		out["tools"] = encodeChatTools(req.Tools, &req.Warnings)
	}
	if req.ToolChoice != nil {
		out["tool_choice"] = encodeChatToolChoice(req.ToolChoice)
	}
	if req.ResponseFormat != nil {
		out["response_format"] = encodeChatResponseFormat(req.ResponseFormat)
	}
	if req.Reasoning != nil {
		effort := req.Reasoning.Effort
		if req.Reasoning.Enabled != nil && !*req.Reasoning.Enabled {
			effort = "none"
			if req.Reasoning.Effort != "" {
				req.Warnings.Add(ir.WarningApproximated, "reasoning.effort", "disabled thinking supersedes the requested effort")
			}
		} else if effort == "" && req.Reasoning.BudgetTokens != nil {
			effort = effortFromBudget(*req.Reasoning.BudgetTokens)
			req.Warnings.Add(ir.WarningApproximated, "reasoning.budget_tokens", "mapped to reasoning_effort")
		}
		if effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	if req.Stream {
		out["stream"] = true
		if req.StreamIncludeUsage {
			out["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	if len(req.Metadata) > 0 {
		out["metadata"] = req.Metadata
	}
	if req.Store != nil {
		out["store"] = *req.Store
	}
	tier, tierWarns := mapServiceTier(ir.Chat, req.ServiceTier)
	req.Warnings = append(req.Warnings, tierWarns...)
	if tier != "" {
		out["service_tier"] = tier
	}
	if req.User != "" {
		out["user"] = req.User
	}
	if req.Verbosity != "" {
		out["verbosity"] = req.Verbosity
	}
	if req.SafetyIdentifier != "" {
		out["safety_identifier"] = req.SafetyIdentifier
	}
	if req.PromptCacheKey != "" {
		out["prompt_cache_key"] = req.PromptCacheKey
	}
	return sse.MarshalWireJSON(out)
}

func encodeChatMessages(in []ir.Message, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, m := range in {
		// Tool results are separate role:"tool" messages in Chat. A user
		// message that carries both results and text splits into the tool
		// messages followed by the remaining user content.
		var toolResults []ir.Block
		var rest []ir.Block
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				toolResults = append(toolResults, b)
			} else {
				rest = append(rest, b)
			}
		}
		for _, b := range toolResults {
			tr := b.ToolResult
			if b.CacheControl != nil && warns != nil {
				warns.Add(ir.WarningDropped, "cache_control", "Chat Completions has no cache breakpoints")
			}
			if len(b.Citations) > 0 && warns != nil {
				warns.Add(ir.WarningDropped, "citations", "Chat Completions input has no citations")
			}
			if tr.IsError && warns != nil {
				warns.Add(ir.WarningDropped, "tool_result.is_error", "Chat Completions tool messages have no is_error flag")
			}
			msg := map[string]any{
				"role":         "tool",
				"tool_call_id": tr.CallID,
				"content":      encodeChatToolResultContent(tr.Content, warns),
			}
			out = append(out, msg)
		}
		if len(rest) == 0 && len(toolResults) > 0 {
			continue
		}
		msg := map[string]any{"role": string(m.Role)}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		for _, b := range rest {
			if len(b.Citations) > 0 && warns != nil {
				warns.Add(ir.WarningDropped, "citations", "Chat Completions input has no citations")
			}
		}
		content, toolCalls, refusal, reasoning := encodeChatContent(rest, m.Role, warns)
		if m.Role == ir.RoleAssistant {
			msg["content"] = content
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			if refusal != "" {
				msg["refusal"] = refusal
			}
			if reasoning != "" {
				msg["reasoning_content"] = reasoning
			}
		} else {
			msg["content"] = content
		}
		out = append(out, msg)
	}
	return out
}

// chatReasoningText is the one owner of the visible reasoning text Chat can
// carry: the raw reasoning text followed by its summaries, shared by the
// non-streaming and streaming encoders.
func chatReasoningText(r *ir.Reasoning) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(r.Text)
	for _, s := range r.Summary {
		b.WriteString(s)
	}
	return b.String()
}

// encodeChatContent returns the Chat content value (string or part array),
// tool calls, refusal text and reasoning text from canonical blocks. Each Chat
// role accepts a documented subset of content parts: system and developer
// messages are text-only, assistant messages carry text and refusal parts, and
// user messages carry the full input vocabulary. A block the role cannot
// represent is dropped with a warning rather than emitted as an invalid part.
func encodeChatContent(blocks []ir.Block, role ir.Role, warns *ir.Warnings) (any, []map[string]any, string, string) {
	var (
		texts     []string
		parts     []map[string]any
		toolCalls []map[string]any
		refusal   string
		reasoning strings.Builder
		hasPart   bool
	)
	roleDetail := "this Chat role has no such content part"
	switch role {
	case ir.RoleSystem:
		roleDetail = "system messages are text-only"
	case ir.RoleDeveloper:
		roleDetail = "developer messages are text-only"
	case ir.RoleAssistant:
		roleDetail = "assistant messages carry text and refusal content only"
	}
	for _, b := range blocks {
		if b.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "cache_control", "Chat Completions has no cache breakpoints")
		}
		switch b.Type {
		case ir.BlockText:
			texts = append(texts, b.Text)
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case ir.BlockImage:
			if role != ir.RoleUser {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.image_url", roleDetail)
				}
				continue
			}
			url := imageDataURL(b.Image)
			if url == "" {
				if warns != nil {
					warns.Add(ir.WarningDropped, "image", "Chat Completions cannot reference a provider image file id")
				}
				continue
			}
			hasPart = true
			part := map[string]any{"type": "image_url"}
			img := map[string]any{"url": url}
			if b.Image.Detail != "" {
				img["detail"] = b.Image.Detail
			}
			part["image_url"] = img
			parts = append(parts, part)
		case ir.BlockFile:
			if role != ir.RoleUser {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.file", roleDetail)
				}
				continue
			}
			if b.File == nil {
				if warns != nil {
					warns.Add(ir.WarningDropped, "file", "file block has no payload")
				}
				continue
			}
			if b.File.Detail != "" && warns != nil {
				warns.Add(ir.WarningDropped, "file.detail", "Chat Completions file parts have no detail level")
			}
			file := map[string]any{}
			if b.File.FileID != "" {
				file["file_id"] = b.File.FileID
			}
			data := fileDataURL(b.File)
			if data != "" {
				// Chat Completions file parts accept PDF data only.
				if fileMediaType(b.File) != "application/pdf" {
					if warns != nil {
						warns.Add(ir.WarningDropped, "file.media_type", "Chat Completions accepts only PDF file data")
					}
				} else {
					file["file_data"] = data
				}
			}
			if _, ok := file["file_id"]; !ok {
				if _, ok := file["file_data"]; !ok {
					if warns != nil {
						if b.File.FileURL != "" {
							warns.Add(ir.WarningDropped, "file", "Chat Completions has no file URL part")
						} else if data == "" {
							warns.Add(ir.WarningDropped, "file", "file block has no payload")
						}
					}
					continue
				}
			}
			if b.File.Filename != "" {
				file["filename"] = b.File.Filename
			}
			hasPart = true
			parts = append(parts, map[string]any{"type": "file", "file": file})
		case ir.BlockAudio:
			if role != ir.RoleUser {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.input_audio", roleDetail)
				}
				continue
			}
			hasPart = true
			parts = append(parts, map[string]any{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": b.Audio.Data, "format": b.Audio.Format},
			})
		case ir.BlockToolCall:
			if role != ir.RoleAssistant {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.tool_call", "Chat tool calls belong to assistant messages")
				}
				continue
			}
			tc := b.ToolCall
			toolCalls = append(toolCalls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(tc.Arguments),
				},
			})
		case ir.BlockRefusal:
			if role != ir.RoleAssistant {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.refusal", roleDetail)
				}
				continue
			}
			refusal = b.Refusal
		case ir.BlockReasoning:
			if role != ir.RoleAssistant {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.reasoning", roleDetail)
				}
				continue
			}
			reasoning.WriteString(chatReasoningText(b.Reasoning))
			if warns != nil && (b.Reasoning.Signature != "" || b.Reasoning.Encrypted != "") {
				warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning artifact cannot be replayed on Chat Completions")
			}
		case ir.BlockRedactedReasoning:
			if role != ir.RoleAssistant {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.reasoning", roleDetail)
				}
				continue
			}
			if warns != nil {
				warns.Add(ir.WarningOpaque, "reasoning", "redacted reasoning cannot be replayed on Chat Completions")
			}
		}
	}
	if hasPart {
		return parts, toolCalls, refusal, reasoning.String()
	}
	return strings.Join(texts, ""), toolCalls, refusal, reasoning.String()
}

// encodeChatToolResultContent flattens a tool result to the text the Chat tool
// role accepts. Tool messages have a text-only content vocabulary, so images,
// files and audio are dropped with a warning instead of becoming invalid
// content parts.
func encodeChatToolResultContent(blocks []ir.Block, warns *ir.Warnings) any {
	var texts []string
	for _, b := range blocks {
		if b.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "cache_control", "Chat Completions has no cache breakpoints")
		}
		if len(b.Citations) > 0 && warns != nil {
			warns.Add(ir.WarningDropped, "citations", "Chat Completions input has no citations")
		}
		switch b.Type {
		case ir.BlockText:
			texts = append(texts, b.Text)
		case ir.BlockImage:
			if warns != nil {
				warns.Add(ir.WarningDropped, "tool_result.image_url", "Chat Completions tool messages are text-only")
			}
		case ir.BlockFile:
			if warns != nil {
				warns.Add(ir.WarningDropped, "tool_result.file", "Chat Completions tool messages are text-only")
			}
		case ir.BlockAudio:
			if warns != nil {
				warns.Add(ir.WarningDropped, "tool_result.audio", "Chat Completions tool messages are text-only")
			}
		default:
			if warns != nil {
				warns.Add(ir.WarningDropped, "tool_result."+string(b.Type), "block has no Chat tool-result equivalent")
			}
		}
	}
	return strings.Join(texts, "")
}

// chatMessageAnnotations maps IR url citations to the Chat message-level
// annotation wrapper. Chat accepts url_citation only, so a citation without a
// URL has no representation and is reported.
func chatMessageAnnotations(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	for _, b := range blocks {
		if len(b.Citations) == 0 {
			continue
		}
		if b.Type != ir.BlockText {
			if warns != nil {
				warns.Add(ir.WarningDropped, "citations", "citation is not attached to a text block")
			}
			continue
		}
		for _, c := range b.Citations {
			if c.URL == "" {
				if warns != nil {
					warns.Add(ir.WarningDropped, "citations", "citation has no Chat url_citation equivalent")
				}
				continue
			}
			out = append(out, map[string]any{
				"type": "url_citation",
				"url_citation": map[string]any{
					"url": c.URL, "title": c.Title,
					"start_index": c.StartIndex, "end_index": c.EndIndex,
				},
			})
		}
	}
	return out
}

func encodeChatTools(in []ir.Tool, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, t := range in {
		if t.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "tools.cache_control", "Chat Completions has no tool cache breakpoints")
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		fn["parameters"] = params
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func encodeChatToolChoice(tc *ir.ToolChoice) any {
	switch tc.Mode {
	case ir.ToolChoiceAuto:
		return "auto"
	case ir.ToolChoiceNone:
		return "none"
	case ir.ToolChoiceRequired:
		return "required"
	case ir.ToolChoiceTool:
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	}
	return "auto"
}

// ChatMaxStopSequences is the documented Chat Completions stop-list ceiling.
const ChatMaxStopSequences = 4

// defaultResponseFormatName is the stable valid name synthesized when a
// source structured-output format carries none. Chat and Responses document
// name as required for json_schema, while Anthropic has no name field.
const defaultResponseFormatName = "response"

// responseFormatName is the single owner of the json_schema name both Chat and
// Responses encoders must emit.
func responseFormatName(rf *ir.ResponseFormat) string {
	if rf != nil && rf.Name != "" {
		return rf.Name
	}
	return defaultResponseFormatName
}

func encodeChatResponseFormat(rf *ir.ResponseFormat) any {
	switch rf.Type {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{"schema": rf.Schema, "name": responseFormatName(rf)}
		if rf.Description != "" {
			js["description"] = rf.Description
		}
		if rf.Strict != nil {
			js["strict"] = *rf.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	default:
		return map[string]any{"type": "text"}
	}
}

// chatWireResponse is the non-streaming Chat response shape. Choices stay raw
// so only the first (the only translated one) is materialized: a body with many
// bounded choices each carrying a large tool_calls array must not expand every
// choice before the first is chosen.
type chatWireResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []json.RawMessage `json:"choices"`
	Usage   *chatWireUsage    `json:"usage"`
}

type chatWireChoice struct {
	Index   int             `json:"index"`
	Message chatWireMessage `json:"message"`
	Finish  string          `json:"finish_reason"`
}

type chatWireUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// DecodeChatResponse converts one non-streaming Chat response into the
// canonical response.
func DecodeChatResponse(body []byte) (*ir.Response, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("chat response body is not valid UTF-8")
	}
	// Bound the choices array before the typed decode materializes it. Only
	// the first choice is translated, and a body of tiny objects must not
	// expand into an unbounded slice.
	var probe struct {
		Choices json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}
	if !metrics.ContentPartsWithinLimit(probe.Choices) {
		return nil, fmt.Errorf("chat response choices exceed %d items", metrics.ContentPartMax)
	}
	// The first choice is the one translated; decode it alone from its raw
	// bytes and bound its tool_calls array so the other choices' arrays are
	// never materialized.
	var in chatWireResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}
	var choice chatWireChoice
	if len(in.Choices) > 0 {
		var choiceBounds struct {
			Message struct {
				ToolCalls   json.RawMessage `json:"tool_calls"`
				Annotations json.RawMessage `json:"annotations"`
			} `json:"message"`
		}
		if err := json.Unmarshal(in.Choices[0], &choiceBounds); err != nil {
			return nil, fmt.Errorf("parse chat response: %w", err)
		}
		if err := checkWireArray(choiceBounds.Message.ToolCalls, "message.tool_calls", metrics.ContentPartMax); err != nil {
			return nil, err
		}
		if err := checkWireArray(choiceBounds.Message.Annotations, "message.annotations", metrics.ContentPartMax); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(in.Choices[0], &choice); err != nil {
			return nil, fmt.Errorf("parse chat response: %w", err)
		}
	}
	if err := checkNativeIdentity("id", in.ID); err != nil {
		return nil, err
	}
	if err := checkNativeIdentity("model", in.Model); err != nil {
		return nil, err
	}
	if typ, code, msg := metrics.ParseErrorEnvelope(body); typ != "" {
		// A gateway can return an error envelope on HTTP 200. ParseErrorEnvelope
		// owns the shared decoy/null and string/object rules, so the canonical
		// response carries the provider's real failure instead of a generic
		// decode error from the missing choices.
		resp := &ir.Response{
			ID: in.ID, Model: in.Model, Created: in.Created,
			Error:  &ir.Error{Type: typ, Code: code, Message: msg},
			Finish: ir.FinishError,
		}
		if in.Usage != nil {
			resp.Usage = decodeChatUsage(in.Usage)
			if err := validateUsage(resp.Usage); err != nil {
				return nil, err
			}
		}
		return resp, nil
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("chat response has no choices")
	}
	blocks, warns, err := decodeChatContent(choice.Message.Content, ir.RoleAssistant)
	if err != nil {
		return nil, err
	}
	if len(choice.Message.Annotations) > 0 {
		citations, w := decodeChatAnnotations(choice.Message.Annotations)
		warns = append(warns, w...)
		if len(citations) > 0 {
			attached := false
			for i := len(blocks) - 1; i >= 0; i-- {
				if blocks[i].Type == ir.BlockText {
					blocks[i].Citations = append(blocks[i].Citations, citations...)
					attached = true
					break
				}
			}
			if !attached {
				warns.Add(ir.WarningDropped, "annotations", "Chat annotations have no text block to attach to")
			}
		}
	}
	if reasoning := choice.Message.reasoningText(); reasoning != "" {
		blocks = append([]ir.Block{{
			Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: reasoning, Provider: ir.Chat},
		}}, blocks...)
	}
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Refusal: *choice.Message.Refusal})
	}
	for j, tc := range choice.Message.ToolCalls {
		call, w, err := decodeChatToolCall(tc, 0, j)
		if err != nil {
			return nil, err
		}
		warns = append(warns, w...)
		blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
	}
	if rawPresent(choice.Message.FunctionCall) {
		if len(choice.Message.ToolCalls) > 0 {
			warns.Add(ir.WarningDropped, "message.function_call", "ignored because tool_calls is present")
		} else if call, err := decodeLegacyMessageFunctionCall(choice.Message.FunctionCall); err != nil {
			return nil, err
		} else {
			blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
		}
	}
	resp := &ir.Response{
		ID:       in.ID,
		Model:    in.Model,
		Created:  in.Created,
		Blocks:   blocks,
		Finish:   irFinishReason(choice.Finish),
		Warnings: warns,
	}
	if in.Usage != nil {
		resp.Usage = decodeChatUsage(in.Usage)
		if err := validateUsage(resp.Usage); err != nil {
			return nil, err
		}
	}
	if len(in.Choices) > 1 {
		resp.Warnings.Add(ir.WarningDropped, "choices", "only the first choice was translated")
	}
	if choice.Finish == "" {
		resp.Warnings.Add(ir.WarningApproximated, "finish_reason", "provider omitted the completion marker")
	}
	return resp, nil
}

func decodeChatUsage(in *chatWireUsage) ir.Usage {
	u := ir.Usage{
		InputTokens:  in.PromptTokens,
		OutputTokens: in.CompletionTokens,
		TotalTokens:  in.TotalTokens,
	}
	if in.PromptTokensDetails != nil {
		u.CacheReadTokens = in.PromptTokensDetails.CachedTokens
		u.CacheWriteTokens = in.PromptTokensDetails.CacheWriteTokens
	}
	if in.CompletionTokensDetails != nil {
		u.ReasoningTokens = in.CompletionTokensDetails.ReasoningTokens
	}
	return u
}

// EncodeChatResponse renders the canonical response as a non-streaming Chat
// completion.
func EncodeChatResponse(resp *ir.Response) ([]byte, error) {
	if err := ir.ValidateResponse(resp); err != nil {
		return nil, err
	}
	if err := validateUsage(resp.Usage); err != nil {
		return nil, err
	}
	content, toolCalls, refusal, reasoning := encodeChatContent(resp.Blocks, ir.RoleAssistant, &resp.Warnings)
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if annotations := chatMessageAnnotations(resp.Blocks, &resp.Warnings); len(annotations) > 0 {
		message["annotations"] = annotations
	}
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := map[string]any{
		"id":      resp.ID,
		"object":  "chat.completion",
		"created": created,
		"model":   resp.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": ChatFinishReason(resp.Finish),
		}},
	}
	if resp.Usage != (ir.Usage{}) {
		usage, err := encodeChatUsage(resp.Usage)
		if err != nil {
			return nil, err
		}
		out["usage"] = usage
	}
	if resp.StopSequence != "" {
		resp.Warnings.Add(ir.WarningDropped, "stop_sequence", "Chat Completions has no matched stop sequence")
	}
	return sse.MarshalWireJSON(out)
}

func encodeChatUsage(u ir.Usage) (map[string]any, error) {
	total := u.TotalTokens
	if total == 0 {
		sum, err := metrics.SumCounts(u.InputTokens, u.OutputTokens)
		if err != nil {
			return nil, fmt.Errorf("usage: %w", err)
		}
		total = sum
	}
	out := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      total,
	}
	out["prompt_tokens_details"] = map[string]any{
		"cached_tokens":      u.CacheReadTokens,
		"cache_write_tokens": u.CacheWriteTokens,
	}
	if u.ReasoningTokens != 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	return out, nil
}

func ChatFinishReason(r ir.FinishReason) string {
	switch r {
	case ir.FinishStop, ir.FinishPause:
		return "stop"
	case ir.FinishLength:
		return "length"
	case ir.FinishToolCalls:
		return "tool_calls"
	case ir.FinishContentFilter:
		return "content_filter"
	case ir.FinishError:
		return "error"
	case ir.FinishOther:
		// No source spelling was available. Chat Completions accepts an
		// arbitrary finish_reason string, so "other" keeps the terminal
		// non-clean instead of fabricating a stop.
		return "other"
	default:
		// A raw provider reason (for example Anthropic's pause_turn or
		// refusal) has no canonical equivalent. Chat Completions clients
		// accept an arbitrary finish_reason string, so echoing the source
		// spelling preserves the signal instead of fabricating a clean stop.
		return string(r)
	}
}

func irFinishReason(s string) ir.FinishReason {
	switch s {
	case "stop":
		return ir.FinishStop
	case "length":
		return ir.FinishLength
	case "tool_calls", "function_call":
		return ir.FinishToolCalls
	case "content_filter":
		return ir.FinishContentFilter
	case "cancelled":
		return ir.FinishCancelled
	case "":
		// A missing completion marker is not a clean stop.
		return ir.FinishOther
	default:
		return ir.FinishOther
	}
}
