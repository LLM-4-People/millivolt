package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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
		Messages   json.RawMessage `json:"messages"`
		Tools      json.RawMessage `json:"tools"`
		Functions  json.RawMessage `json:"functions"`
		Stop       json.RawMessage `json:"stop"`
		Modalities json.RawMessage `json:"modalities"`
		Metadata   json.RawMessage `json:"metadata"`
		LogitBias  json.RawMessage `json:"logit_bias"`
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
		{bounds.Modalities, "modalities"},
	} {
		if err := checkWireArray(b.raw, b.field, WireCollectionMax); err != nil {
			return nil, errParseOpenAIBody(err)
		}
	}
	if err := checkStringArray(bounds.Stop, "stop"); err != nil {
		return nil, errParseOpenAIBody(err)
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
	maxCompletionTokens, err := jsonnum.OptionalInt(in.MaxCompletionTokens, "max_completion_tokens")
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}
	maxTokens, err := jsonnum.OptionalInt(in.MaxTokens, "max_tokens")
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}
	seed, err := jsonnum.OptionalInt64(in.Seed, "seed")
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}
	topLogprobs, err := jsonnum.OptionalInt(in.TopLogprobs, "top_logprobs")
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}
	n, err := jsonnum.OptionalInt(in.N, "n")
	if err != nil {
		return nil, errParseOpenAIBody(err)
	}

	var warns ir.Warnings
	req := &ir.Request{
		Model:                in.Model,
		MaxOutputTokens:      MergeTokenCaps(maxCompletionTokens, maxTokens),
		Temperature:          in.Temperature,
		TopP:                 in.TopP,
		Seed:                 seed,
		PresencePenalty:      in.PresencePenalty,
		FrequencyPenalty:     in.FrequencyPenalty,
		LogitBias:            in.LogitBias,
		Logprobs:             in.Logprobs,
		TopLogprobs:          topLogprobs,
		ParallelToolCalls:    in.ParallelToolCalls,
		Stream:               in.Stream,
		ServiceTier:          in.ServiceTier,
		Store:                in.Store,
		User:                 in.User,
		Verbosity:            in.Verbosity,
		SafetyIdentifier:     in.SafetyIdentifier,
		PromptCacheKey:       in.PromptCacheKey,
		Moderation:           in.Moderation,
		PromptCacheOptions:   in.PromptCacheOptions,
		PromptCacheRetention: in.PromptCacheRetention,
	}
	if in.StreamOptions != nil {
		req.StreamIncludeUsage = in.StreamOptions.IncludeUsage
		if in.StreamOptions.IncludeObfuscation {
			req.Warnings.Add(ir.WarningDropped, "stream_options.include_obfuscation",
				"provider-generated stream obfuscation cannot be translated")
		}
	}
	if len(in.Metadata) > 0 {
		// Keep these warnings in the shared accumulator so the single final
		// append below emits each one once; metadata is decoded before the
		// message and parameter warnings share the same slice.
		req.Metadata, warns = decodeChatMetadata(in.Metadata)
	}
	stops, err := parseStop(in.Stop)
	if err != nil {
		return nil, err
	}
	if len(stops) > 0 {
		req.Stop = stops
	}
	if n != nil && (*n < 1 || *n > 128) {
		// The Chat schema documents 1..128. Zero, a negative value and an
		// out-of-range count have no representable one-choice translation,
		// so fail closed instead of silently emitting one choice.
		return nil, errParseOpenAIBody(fmt.Errorf("n must be between 1 and 128"))
	}
	if n != nil && *n > 1 {
		req.Warnings.Add(ir.WarningDropped, "n", "only one choice can be translated")
	}
	if in.ReasoningEffort != "" {
		req.Reasoning = &ir.ReasoningConfig{Effort: in.ReasoningEffort}
	}
	rf, formatWarn, err := decodeChatResponseFormat(in.ResponseFormat)
	if err != nil {
		return nil, err
	}
	if rf != nil {
		req.ResponseFormat = rf
	}
	if formatWarn != "" {
		req.Warnings.Add(ir.WarningDropped, "response_format", formatWarn)
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

	ids := newRequestToolIDs()
	if err := preReserveChatToolIDs(ids, in.Messages); err != nil {
		return nil, err
	}
	msgs, msgWarns, err := decodeChatMessages(in.Messages, ids)
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

// parseStop normalizes the Chat stop field's string or array form. Absent,
// null, a string and an array of strings are the accepted shapes; anything else
// is a client error rather than a silently dropped stop filter. The single
// empty string and the empty array still mean no stops.
func parseStop(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		return arr, nil
	}
	return nil, errors.New("stop must be a string, an array of strings, or null")
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

// preReserveChatToolIDs claims every explicit modern tool-call id before any
// message is decoded. A later id-less call then synthesizes against the whole
// request instead of colliding with an explicit id that appears after it. The
// deprecated per-assistant function_call carries no id and stays outside this
// namespace; it keeps its function name as the documented exception.
func preReserveChatToolIDs(ids *requestToolIDs, messages []chatWireMessage) error {
	for i, m := range messages {
		// Only an assistant message's tool_calls become IR blocks. A tool_calls
		// member on another role is ignored by the decoder and must not consume
		// the request namespace.
		if m.Role != "assistant" {
			continue
		}
		for j, tc := range m.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if err := ids.preReserve(tc.ID); err != nil {
				return errParseOpenAIBody(fmt.Errorf("message %d tool call %d: %w", i, j, err))
			}
		}
	}
	return nil
}

// decodeChatMessages converts Chat messages into the canonical message list.
// Consecutive role:"tool" messages (and a following user turn) are merged into
// one user message whose tool_result blocks come first: that is the shape
// Anthropic requires, and the Chat encoder splits it back out losslessly.
func decodeChatMessages(in []chatWireMessage, ids *requestToolIDs) ([]ir.Message, ir.Warnings, error) {
	var (
		out       []ir.Message
		warns     ir.Warnings
		pending   *ir.Message
		toolCalls chatToolCalls
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
				// A user turn merged behind tool results keeps its participant
				// name so a target that can express it (Chat) does, and the
				// target encoders own any loss. Only conflicting names are a
				// decoder-level loss: the first one wins.
				if m.Name != "" {
					if pending.Name == "" {
						pending.Name = m.Name
					} else if pending.Name != m.Name {
						warns.Add(ir.WarningDropped, "messages.name", "merged user turns carried conflicting names; kept the first")
					}
				}
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
			if len(m.Annotations) > 0 {
				// A client replaying a prior Chat response carries its
				// message-level annotations. Attach them exactly like the
				// response decoder so the extension is preserved.
				annotated, w, aerr := attachChatAnnotations(blocks, m.Annotations)
				if aerr != nil {
					return nil, nil, fmt.Errorf("message %d: %w", i, aerr)
				}
				blocks = annotated
				warns = append(warns, w...)
			}
			if m.Audio != nil {
				warns.Add(ir.WarningDropped, "messages.audio", "Chat audio output has no cross-protocol equivalent")
			}
			if reasoning := m.reasoningText(&warns); reasoning != "" {
				// Reasoning precedes the visible answer in canonical order.
				blocks = append([]ir.Block{{
					Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: reasoning, Provider: ir.Chat},
				}}, blocks...)
			}
			if m.Refusal != nil && *m.Refusal != "" {
				blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Refusal: *m.Refusal})
			}
			for j, tc := range m.ToolCalls {
				call, generated, w, err := decodeChatToolCall(tc, i, j, false, ids)
				if err != nil {
					return nil, nil, err
				}
				warns = append(warns, w...)
				toolCalls.add(call.ID, call.Name, generated)
				blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
			}
			if rawPresent(m.FunctionCall) {
				if len(m.ToolCalls) > 0 {
					warns.Add(ir.WarningDropped, "messages.function_call", "ignored because tool_calls is present")
				} else if call, err := decodeLegacyMessageFunctionCall(m.FunctionCall, false); err != nil {
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
			if m.Role == "tool" {
				// A modern tool reply preserves which reference the client
				// used: tool_call_id is an id, the message name is a function
				// name, and neither one resolves an ambiguous remaining call.
				// A legacy role:"function" reply pairs with function_call by
				// name and stays outside this state.
				var err error
				switch {
				case m.ToolCallID != "":
					callID, err = toolCalls.consumeID(m.ToolCallID)
				case m.Name != "":
					callID, err = toolCalls.consumeName(m.Name)
				default:
					callID, err = toolCalls.consumeEmpty()
				}
				if err != nil {
					return nil, nil, errParseOpenAIBody(fmt.Errorf("message %d: %w", i, err))
				}
			} else if callID == "" {
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
// reasoning instead, and a compatibility fallback accepts reasoning_text.
// The two primary spellings stay strict, matching the rest of the wire model.
// reasoning_text is an extension, so a wrong-typed value is reported and
// ignored instead of aborting a response the baseline decoded.
func (m chatWireMessage) reasoningText(warns *ir.Warnings) string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	if m.Reasoning != "" {
		return m.Reasoning
	}
	if len(m.ReasoningText) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(m.ReasoningText, &text); err == nil {
		return text
	}
	if warns != nil {
		warns.AddOnce(ir.WarningDropped, "reasoning_text", "reasoning_text is not a string")
	}
	return ""
}

// rawPresent reports whether a JSON field was supplied with a non-null value.
func rawPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// rawNonEmpty reports whether a JSON field carries any content. Absent, null,
// an empty array, an empty object and an empty string are empty; every other
// value is content. It is the predicate for a loss report, so an empty
// compatibility extension never claims something was dropped.
func rawNonEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	switch trimmed[0] {
	case '[', '{':
		return len(trimmed) > 2 && len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) > 0
	case '"':
		var s string
		return json.Unmarshal(trimmed, &s) == nil && s != ""
	default:
		return true
	}
}
