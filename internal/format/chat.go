package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Chat Completions wire codec. DecodeChatRequest/EncodeChatRequest and
// DecodeChatResponse/EncodeChatResponse are the only owners of the Chat wire
// shape; every other codec speaks ir. The decoder is deliberately strict about
// structural features it cannot represent (non-function tools, invalid tool
// arguments) and records a warning for optional knobs a target protocol may
// not support.

// chatWireResponse is the non-streaming Chat response shape. Choices stay raw
// so only the first (the only translated one) is materialized: a body with many
// bounded choices each carrying a large tool_calls array must not expand every
// choice before the first is chosen.
type chatWireResponse struct {
	ID          string            `json:"id"`
	Object      string            `json:"object"`
	Created     jsonnum.Int64     `json:"created"`
	Model       string            `json:"model"`
	Choices     []json.RawMessage `json:"choices"`
	Usage       *chatWireUsage    `json:"usage"`
	Metadata    boundedStringMap  `json:"metadata"`
	ServiceTier string            `json:"service_tier"`
}

type chatWireChoice struct {
	Message chatWireMessage `json:"message"`
	Finish  string          `json:"finish_reason"`
}

type chatWireUsage struct {
	PromptTokens        jsonnum.Int64 `json:"prompt_tokens"`
	CompletionTokens    jsonnum.Int64 `json:"completion_tokens"`
	TotalTokens         jsonnum.Int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     jsonnum.Int64 `json:"cached_tokens"`
		CacheWriteTokens jsonnum.Int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens jsonnum.Int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// DecodeChatResponse converts one non-streaming Chat response into the
// canonical response.
// chatResponseTerminalClean reports whether a Chat finish_reason denotes a
// completed turn. Length, content_filter, cancelled, unknown and missing values
// all mean the source stopped before the answer was complete, so an
// unparseable tool-call argument value is that truncation rather than a
// translator failure.
func chatResponseTerminalClean(finish string) bool {
	switch finish {
	case "stop", "tool_calls", "function_call":
		return true
	}
	return false
}

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
			ID: in.ID, Model: in.Model, Created: int64(in.Created),
			Error:       boundedProviderError(typ, code, msg),
			Finish:      ir.FinishError,
			Metadata:    in.Metadata.values,
			ServiceTier: in.ServiceTier,
		}
		if in.Usage != nil {
			resp.Usage = decodeChatUsage(in.Usage)
			if err := validateUsage(resp.Usage); err != nil {
				return nil, err
			}
		}
		return resp, nil
	}
	if in.Object != "" && in.Object != "chat.completion" {
		// A chunk, prompt completion or other object is not a completed Chat
		// message. Translating it produced a 200 with an empty target body
		// while the native path classified the same bytes as a void. A 200
		// error envelope is classified above, so an envelope that also names
		// object "error" keeps the provider's message.
		return nil, fmt.Errorf("chat response object %q is not a completion", boundedEcho(in.Object))
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("chat response has no choices")
	}
	var firstMessage struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(in.Choices[0], &firstMessage); err != nil {
		return nil, fmt.Errorf("parse chat response: %w", err)
	}
	if trimmed := bytes.TrimSpace(firstMessage.Message); len(trimmed) == 0 || trimmed[0] != '{' {
		// choices:[null] and a chunk-shaped choice decode to the zero message
		// and were accepted as an empty completion. A completed Chat choice
		// must carry a message object.
		return nil, errors.New("chat response choice has no message object")
	}
	if !metrics.ContentPartsWithinLimit(choice.Message.Content) {
		// A response is a completion artifact, not a request to best-effort
		// salvage: silently returning an empty translated body would publish
		// a 200 success for an answer the client never received.
		return nil, fmt.Errorf("chat response content exceeds %d parts", metrics.ContentPartMax)
	}
	// A length-truncated or otherwise non-clean turn can carry the prefix of a
	// tool-call argument value the model never finished. That is the source's
	// terminal class, not a translator failure, so only the unparseable call is
	// dropped; text, refusal and the source finish survive.
	cleanTerminal := chatResponseTerminalClean(choice.Finish)
	blocks, warns, err := decodeChatContent(choice.Message.Content, ir.RoleAssistant)
	if err != nil {
		return nil, err
	}
	if len(choice.Message.Annotations) > 0 {
		annotated, annotationWarns, aerr := attachChatAnnotations(blocks, choice.Message.Annotations)
		if aerr != nil {
			return nil, aerr
		}
		blocks = annotated
		warns = append(warns, annotationWarns...)
	}
	if reasoning := choice.Message.reasoningText(&warns); reasoning != "" {
		blocks = append([]ir.Block{{
			Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: reasoning, Provider: ir.Chat},
		}}, blocks...)
	}
	if rawNonEmpty(choice.Message.ReasoningDetails) {
		// Structured detail items are provider-specific and no documented
		// Chat field can carry them. Never synthesize reasoning text from
		// their members; report the loss instead. An empty array, object or
		// string carries no detail to drop.
		warns.AddOnce(ir.WarningDropped, "reasoning_details",
			"structured reasoning details have no canonical text representation")
	}
	if choice.Message.Refusal != nil && *choice.Message.Refusal != "" {
		blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Refusal: *choice.Message.Refusal})
	}
	for j, tc := range choice.Message.ToolCalls {
		call, _, w, err := decodeChatToolCall(tc, 0, j, !cleanTerminal, nil)
		if err != nil {
			if !cleanTerminal && isToolArgumentsError(err) {
				warns.AddOnce(ir.WarningDropped, "tool_calls.arguments",
					"the source stopped before the tool call arguments were complete")
				continue
			}
			return nil, err
		}
		warns = append(warns, w...)
		blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
	}
	if rawPresent(choice.Message.FunctionCall) {
		if len(choice.Message.ToolCalls) > 0 {
			warns.Add(ir.WarningDropped, "message.function_call", "ignored because tool_calls is present")
		} else {
			call, err := decodeLegacyMessageFunctionCall(choice.Message.FunctionCall, !cleanTerminal)
			switch {
			case err == nil:
				blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
			case !cleanTerminal && isToolArgumentsError(err):
				warns.AddOnce(ir.WarningDropped, "function_call.arguments",
					"the source stopped before the function call arguments were complete")
			default:
				return nil, err
			}
		}
	}
	finish, exactFinish := irFinishReason(choice.Finish)
	resp := &ir.Response{
		ID:          in.ID,
		Model:       in.Model,
		Created:     int64(in.Created),
		Blocks:      blocks,
		Finish:      finish,
		Warnings:    warns,
		Metadata:    in.Metadata.values,
		ServiceTier: in.ServiceTier,
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
	} else if !exactFinish {
		// The raw spelling has no canonical equivalent and is lost; report
		// it so a strict translation can reject instead of silently
		// publishing an incomplete result.
		resp.Warnings.Add(ir.WarningApproximated, "finish_reason",
			fmt.Sprintf("unknown finish reason %q has no canonical equivalent", boundedEcho(choice.Finish)))
	}
	return resp, nil
}

func decodeChatUsage(in *chatWireUsage) ir.Usage {
	u := ir.Usage{
		InputTokens:  int64(in.PromptTokens),
		OutputTokens: int64(in.CompletionTokens),
		TotalTokens:  int64(in.TotalTokens),
	}
	if in.PromptTokensDetails != nil {
		u.CacheReadTokens = int64(in.PromptTokensDetails.CachedTokens)
		u.CacheWriteTokens = int64(in.PromptTokensDetails.CacheWriteTokens)
	}
	if in.CompletionTokensDetails != nil {
		u.ReasoningTokens = int64(in.CompletionTokensDetails.ReasoningTokens)
	}
	return u
}

// EncodeChatResponse renders the canonical response as a non-streaming Chat
// completion.
func EncodeChatResponse(resp *ir.Response) ([]byte, error) {
	if err := ir.ValidateResponse(resp); err != nil {
		return nil, err
	}
	for _, block := range resp.Blocks {
		if block.Type == ir.BlockToolCall {
			if err := validateChatToolCall(block.ToolCall); err != nil {
				return nil, err
			}
		}
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
	if chatFinishReasonApproximated(resp.Finish) {
		resp.Warnings.Add(ir.WarningApproximated, "finish_reason",
			"Chat Completions has no equivalent terminal reason")
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
			// The response schema requires the nullable logprobs member;
			// this proxy never requests log probabilities, so it is null.
			"logprobs": nil,
		}},
	}
	if len(resp.Metadata) > 0 {
		out["metadata"] = resp.Metadata
	}
	if resp.ServiceTier != "" {
		tier, tierWarns := mapServiceTier(ir.Chat, resp.ServiceTier)
		resp.Warnings = append(resp.Warnings, tierWarns...)
		if tier != "" {
			out["service_tier"] = tier
		}
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

// ChatFinishReason maps a canonical finish reason onto the documented Chat
// Completions wire enum. A provider-only reason cannot be echoed: both the
// non-streaming and streaming schemas accept only stop, length, tool_calls,
// content_filter or function_call (plus null while streaming). A refusal keeps
// its own meaning as content_filter; every other non-enum reason becomes stop.
// The canonical IR and the record retain the source spelling.
func ChatFinishReason(r ir.FinishReason) string {
	switch r {
	case ir.FinishStop, ir.FinishLength, ir.FinishToolCalls, ir.FinishContentFilter:
		return string(r)
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// chatFinishReasonApproximated reports whether ChatFinishReason replaced a
// non-Chat source reason. The streaming encoder has no warning channel after
// its status line is committed; the non-streaming encoder records the loss.
func chatFinishReasonApproximated(r ir.FinishReason) bool {
	switch r {
	case ir.FinishStop, ir.FinishLength, ir.FinishToolCalls, ir.FinishContentFilter:
		return false
	}
	return true
}

// irFinishReason maps one Chat finish_reason onto the canonical reason and
// reports whether the source spelling is an exact canonical value. An empty or
// unknown non-empty value maps to FinishOther; the caller owns the separate
// missing-marker and unknown-value warnings.
func irFinishReason(s string) (ir.FinishReason, bool) {
	switch s {
	case "stop":
		return ir.FinishStop, true
	case "length":
		return ir.FinishLength, true
	case "tool_calls", "function_call":
		return ir.FinishToolCalls, true
	case "content_filter":
		return ir.FinishContentFilter, true
	case "cancelled":
		return ir.FinishCancelled, true
	default:
		// A missing completion marker and an unknown provider spelling are
		// not a clean stop.
		return ir.FinishOther, false
	}
}
