package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// DecodeAnthropicResponse converts one non-streaming Anthropic response into
// the canonical response.
func DecodeAnthropicResponse(body []byte) (*ir.Response, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("anthropic response body is not valid UTF-8")
	}
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}
	if len(probe.Content) > 0 && !metrics.ContentPartsWithinLimit(probe.Content) {
		return nil, fmt.Errorf("%w (limit %d parts)", errNativeContentParts, metrics.ContentPartMax)
	}
	if err := checkAnthropicCitations(probe.Content, "response content"); err != nil {
		return nil, err
	}
	var in anthropicWireResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}
	if in.Type != "" && in.Type != "message" {
		return nil, fmt.Errorf("not an anthropic message response (type %q)", in.Type)
	}
	if len(in.Content) == 0 && in.StopReason == "" {
		return nil, fmt.Errorf("not an anthropic response body")
	}
	if err := checkNativeIdentity("id", in.ID); err != nil {
		return nil, err
	}
	if err := checkNativeIdentity("model", in.Model); err != nil {
		return nil, err
	}
	for _, b := range in.Content {
		if b.Type != "tool_use" {
			continue
		}
		if err := checkNativeIdentity("tool id", b.ID); err != nil {
			return nil, err
		}
		if err := checkNativeIdentity("tool name", b.Name); err != nil {
			return nil, err
		}
	}
	resp := &ir.Response{
		ID:           in.ID,
		Model:        in.Model,
		Created:      time.Now().Unix(),
		Finish:       anthropicFinishReason(in.StopReason),
		StopSequence: in.StopSequence,
	}
	// The response accumulator spans every content block so AddOnce dedupes
	// a repeated block-level loss.
	blocks, err := decodeAnthropicBlocks(in.Content, &resp.Warnings, nil)
	if err != nil {
		return nil, err
	}
	resp.Blocks = blocks
	// A provider stop reason of "error" is a failure statement even though the
	// current Anthropic response type does not list it and the body carries no
	// error object. Keep the source finish and synthesize the canonical
	// failure so the record and a translated terminal are not empty.
	if resp.Finish == ir.FinishError && resp.Error == nil {
		resp.Error = syntheticAnthropicFailure()
	}
	if tier, tierWarns := anthropicUsageServiceTier(in.Usage.ServiceTier, in.Usage.Speed); tier != "" || len(tierWarns) > 0 {
		resp.ServiceTier = tier
		resp.Warnings = append(resp.Warnings, tierWarns...)
	}
	if in.StopReason == "model_context_window_exceeded" {
		resp.Warnings.Add(ir.WarningApproximated, "stop_reason",
			"model context window exhaustion is represented as a length finish")
	}
	if in.StopReason == "" {
		// Chat has the same warning on a missing finish_reason; Anthropic
		// omitted a completion marker, so a Responses target becomes
		// incomplete and the loss is explicit on every wire.
		resp.Warnings.Add(ir.WarningApproximated, "stop_reason", "provider omitted the completion marker")
	}
	usage, err := anthropicUsageToIR(in.Usage)
	if err != nil {
		return nil, err
	}
	resp.Usage = usage
	if err := validateUsage(resp.Usage); err != nil {
		return nil, err
	}
	if len(in.StopDetails) > 0 && !bytes.Equal(bytes.TrimSpace(in.StopDetails), []byte("null")) {
		resp.Warnings.Add(ir.WarningDropped, "stop_details",
			"the cross-protocol terminal has no stop-details equivalent")
	}
	warnAnthropicResponseOnly(in.Container, "container", &resp.Warnings)
	warnAnthropicResponseOnly(in.Diagnostics, "diagnostics", &resp.Warnings)
	warnAnthropicResponseOnly(in.Usage.CacheCreation, "usage.cache_creation", &resp.Warnings)
	warnAnthropicResponseOnly(in.Usage.InferenceGeo, "usage.inference_geo", &resp.Warnings)
	warnAnthropicResponseOnly(in.Usage.ServerToolUse, "usage.server_tool_use", &resp.Warnings)
	return resp, nil
}

// warnAnthropicResponseOnly reports one non-null Anthropic response member
// that has no Chat or Responses representation. Absent and explicit null are
// the schema's normal empty spellings, not a loss.
func warnAnthropicResponseOnly(raw json.RawMessage, field string, warns *ir.Warnings) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return
	}
	warns.Add(ir.WarningDropped, field, "Anthropic response member has no cross-protocol equivalent")
}

func anthropicUsageToIR(u anthropicWireUsage) (ir.Usage, error) {
	var cacheRead, cacheWrite int64
	if u.CacheReadInputTokens != nil {
		cacheRead = int64(*u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens != nil {
		cacheWrite = int64(*u.CacheCreationInputTokens)
	}
	out := ir.Usage{
		OutputTokens:     int64(u.OutputTokens),
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
	}
	input, err := metrics.SumCounts(int64(u.InputTokens), cacheRead, cacheWrite)
	if err != nil {
		return ir.Usage{}, fmt.Errorf("usage: %w", err)
	}
	total, err := metrics.SumCounts(input, int64(u.OutputTokens))
	if err != nil {
		return ir.Usage{}, fmt.Errorf("usage: %w", err)
	}
	out.InputTokens = input
	out.TotalTokens = total
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = int64(u.OutputTokensDetails.ThinkingTokens)
	}
	return out, nil
}

func anthropicFinishReason(reason string) ir.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence":
		return ir.FinishStop
	case "max_tokens", "model_context_window_exceeded":
		return ir.FinishLength
	case "tool_use":
		return ir.FinishToolCalls
	case "":
		// A missing stop_reason is not a clean end_turn.
		return ir.FinishOther
	default:
		// Keep the source spelling for reasons Chat Completions can echo
		// verbatim (pause_turn, refusal) and for unknown provider values,
		// rather than collapsing them into a clean stop.
		return ir.FinishReason(reason)
	}
}

// syntheticAnthropicFailure is the canonical failure for a provider stop
// reason that says the turn failed without an error object. The spelling
// "error" is not in the current Anthropic StopReason type, but compatible
// upstreams emit it; dropping the failure or mapping it to a clean finish
// would hide a provider statement.
func syntheticAnthropicFailure() *ir.Error {
	return &ir.Error{Type: sse.TypeUpstreamError, Message: "upstream response failed"}
}

// EncodeAnthropicResponse renders the canonical response as a non-streaming
// Anthropic message.
func EncodeAnthropicResponse(resp *ir.Response) ([]byte, error) {
	if err := ir.ValidateResponse(resp); err != nil {
		return nil, err
	}
	if err := validateAnthropicResponseToolVocabulary(resp); err != nil {
		return nil, err
	}
	if err := validateUsage(resp.Usage); err != nil {
		return nil, err
	}
	blocks := encodeAnthropicBlocks(encodeAnthropicResponseBlocks(resp.Blocks, &resp.Warnings), ir.RoleAssistant, &resp.Warnings)
	serviceTier, speed, tierWarns := anthropicResponseUsageTier(resp.ServiceTier)
	resp.Warnings = append(resp.Warnings, tierWarns...)
	stopReason, terminalApproximated := anthropicContentTerminal(
		resp.Finish, hasRefusalBlock(resp.Blocks), hasToolCallBlock(resp.Blocks))
	if terminalApproximated {
		resp.Warnings.Add(ir.WarningApproximated, "stop_reason",
			fmt.Sprintf("finish reason %q is represented as Anthropic stop reason %q", string(resp.Finish), stopReason))
	}
	out := map[string]any{
		"id":            resp.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"stop_details":  nil,
		"container":     nil,
		"diagnostics":   nil,
		"usage":         anthropicMessageUsage(resp.Usage, serviceTier, speed),
	}
	if resp.StopSequence != "" {
		out["stop_sequence"] = resp.StopSequence
	}
	if len(resp.Metadata) > 0 {
		// The Anthropic Message response object has no metadata property.
		// The field is unrepresentable, so report the loss instead of adding
		// a non-schema field.
		resp.Warnings.Add(ir.WarningDropped, "metadata", "Anthropic Messages responses have no metadata field")
	}
	if resp.Finish == ir.FinishOther || resp.Finish == ir.FinishCancelled {
		resp.Warnings.Add(ir.WarningApproximated, "finish_reason",
			"Anthropic Messages has no equivalent terminal reason")
	}
	return sse.MarshalWireJSON(out)
}

// anthropicMessageUsage renders the full Message.usage shape. The current
// Anthropic interface marks cache_creation, inference_geo, output_tokens_details,
// server_tool_use, service_tier and speed as required nullable members, so every
// one is present; the members with no canonical source stay null rather than
// disappearing. Reasoning maps to output_tokens_details.
func anthropicMessageUsage(u ir.Usage, serviceTier, speed string) map[string]any {
	out := map[string]any{
		"input_tokens":                nonNegative(u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens),
		"output_tokens":               u.OutputTokens,
		"cache_creation_input_tokens": u.CacheWriteTokens,
		"cache_read_input_tokens":     u.CacheReadTokens,
		"cache_creation":              nil,
		"inference_geo":               nil,
		"output_tokens_details":       nil,
		"server_tool_use":             nil,
		"service_tier":                nil,
		"speed":                       nil,
	}
	if u.ReasoningTokens != 0 {
		out["output_tokens_details"] = map[string]any{"thinking_tokens": u.ReasoningTokens}
	}
	if serviceTier != "" {
		out["service_tier"] = serviceTier
	}
	if speed != "" {
		out["speed"] = speed
	}
	return out
}

// anthropicDeltaUsage renders MessageDeltaUsage. Its type carries only the
// cumulative counters and the two detail objects; tier, speed and
// cache_creation are not members, so a message_delta must not gain them.
func anthropicDeltaUsage(u ir.Usage) map[string]any {
	out := map[string]any{
		"input_tokens":                nonNegative(u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens),
		"output_tokens":               u.OutputTokens,
		"cache_creation_input_tokens": u.CacheWriteTokens,
		"cache_read_input_tokens":     u.CacheReadTokens,
		"output_tokens_details":       nil,
		"server_tool_use":             nil,
	}
	if u.ReasoningTokens != 0 {
		out["output_tokens_details"] = map[string]any{"thinking_tokens": u.ReasoningTokens}
	}
	return out
}

// encodeAnthropicResponseBlocks filters assistant blocks that are valid only
// in a request MessageParam.Content list. Anthropic's response Message.content
// is a ContentBlock list, whose union has no image or document block, so the
// shared request encoder's block plus approximation warning would emit a wire
// the target rejects. Request encoding keeps its current block-and-warn
// behavior.
func encodeAnthropicResponseBlocks(blocks []ir.Block, warns *ir.Warnings) []ir.Block {
	filtered := make([]ir.Block, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockImage:
			if warns != nil {
				warns.Add(ir.WarningDropped, "response.assistant.image", "Anthropic Message.content has no image block")
			}
			continue
		case ir.BlockFile:
			if warns != nil {
				warns.Add(ir.WarningDropped, "response.assistant.document", "Anthropic Message.content has no document block")
			}
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered
}

// hasRefusalBlock reports whether the response content carries a refusal the
// Anthropic encoder represents. An empty refusal is dropped as invalid
// content, so it does not imply a refusal terminal.
func hasRefusalBlock(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type == ir.BlockRefusal && b.Refusal != "" {
			return true
		}
	}
	return false
}

// hasToolCallBlock reports whether the response content carries a representable
// tool call.
func hasToolCallBlock(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type == ir.BlockToolCall && b.ToolCall != nil {
			return true
		}
	}
	return false
}

// anthropicContentTerminal derives the Anthropic stop reason from the response
// content when the mapped reason is end_turn. Anthropic defines refusal and
// tool_use as distinct terminals and clients branch on the field: a tool call
// under end_turn is never executed, and a refusal under end_turn reads as a
// normal answer. A refusal wins when both appear. The bool reports that the
// stored reason was approximated, for the non-stream warning channel; a
// streaming response has no header to report it in.
func anthropicContentTerminal(finish ir.FinishReason, refusal, toolCall bool) (string, bool) {
	reason := anthropicStopReason(finish)
	if reason != "end_turn" {
		return reason, false
	}
	if refusal {
		return "refusal", true
	}
	if toolCall {
		return "tool_use", true
	}
	return reason, false
}

func anthropicStopReason(r ir.FinishReason) string {
	switch r {
	case ir.FinishStop:
		return "end_turn"
	case ir.FinishLength:
		return "max_tokens"
	case ir.FinishToolCalls:
		return "tool_use"
	case ir.FinishContentFilter:
		return "refusal"
	case ir.FinishPause:
		return "pause_turn"
	case ir.FinishOther:
		return "end_turn"
	default:
		// A raw Chat reason with no Anthropic enum equivalent; Anthropic has
		// no "other" value, so the caller keeps its most neutral terminal.
		return "end_turn"
	}
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
