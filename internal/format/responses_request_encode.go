package format

import (
	"encoding/json"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// EncodeResponsesRequest renders the canonical request as a Responses body.
func EncodeResponsesRequest(req *ir.Request) ([]byte, error) {
	if err := ir.ValidateRequest(req); err != nil {
		return nil, err
	}
	if err := validateResponsesToolVocabulary(req); err != nil {
		return nil, err
	}
	if err := validateResponseFormatName("responses", req.ResponseFormat); err != nil {
		return nil, err
	}
	if len(req.Stop) > 0 {
		req.Warnings.Add(ir.WarningDropped, "stop", "Responses has no stop sequences")
	}
	if req.Seed != nil {
		req.Warnings.Add(ir.WarningDropped, "seed", "Responses has no seed")
	}
	if req.PresencePenalty != nil {
		req.Warnings.Add(ir.WarningDropped, "presence_penalty", "Responses has no presence penalty")
	}
	if req.FrequencyPenalty != nil {
		req.Warnings.Add(ir.WarningDropped, "frequency_penalty", "Responses has no frequency penalty")
	}
	if len(req.LogitBias) > 0 {
		req.Warnings.Add(ir.WarningDropped, "logit_bias", "Responses has no logit bias")
	}
	if req.TopK != nil {
		req.Warnings.Add(ir.WarningDropped, "top_k", "Responses has no top_k")
	}
	if req.CacheControl != nil {
		req.Warnings.Add(ir.WarningDropped, "cache_control", "Responses has no request-level cache breakpoint")
	}
	if req.Reasoning != nil {
		if req.Reasoning.Enabled != nil && !*req.Reasoning.Enabled {
			if req.Reasoning.Effort != "" {
				req.Warnings.Add(ir.WarningApproximated, "reasoning.effort", "disabled thinking supersedes the requested effort")
			}
			if req.Reasoning.BudgetTokens != nil {
				req.Warnings.Add(ir.WarningDropped, "reasoning.budget_tokens", "disabled thinking supersedes the requested budget")
			}
			req.Reasoning.Effort = "none"
		} else if req.Reasoning.BudgetTokens != nil {
			if req.Reasoning.Effort == "" {
				req.Reasoning.Effort = effortFromBudget(*req.Reasoning.BudgetTokens)
				req.Warnings.Add(ir.WarningApproximated, "reasoning.budget_tokens", "mapped to reasoning.effort")
			} else {
				req.Warnings.Add(ir.WarningDropped, "reasoning.budget_tokens", "reasoning.effort takes precedence over the budget")
			}
		}
		if req.Reasoning.Display != "" {
			req.Warnings.Add(ir.WarningDropped, "reasoning.display", "Responses has no display mode")
		}
	}
	out := map[string]any{
		"model": req.Model,
		"input": encodeResponsesInput(req),
	}
	instructions := req.Instructions
	if instructions == nil {
		if folded := foldLeadingSystem(req.Messages, &req.Warnings); folded != "" {
			instructions = &folded
		}
	}
	if instructions != nil && *instructions != "" {
		out["instructions"] = *instructions
	}
	if req.MaxOutputTokens != nil {
		// CreateResponse.max_output_tokens has a documented minimum of 16
		// while Chat max_tokens and Anthropic max_tokens allow smaller values
		// (including 0). Clamp up and report the approximation; reject-mode
		// translation turns the warning into a 400.
		limit := *req.MaxOutputTokens
		if limit < 16 {
			limit = 16
			req.Warnings.Add(ir.WarningApproximated, "max_output_tokens", "Responses requires at least 16")
		}
		out["max_output_tokens"] = limit
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.TopLogprobs != nil {
		out["top_logprobs"] = *req.TopLogprobs
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.Tools) > 0 {
		out["tools"] = encodeResponsesTools(req.Tools, &req.Warnings)
	}
	if req.ToolChoice != nil {
		out["tool_choice"] = encodeResponsesToolChoice(req.ToolChoice)
	}
	if req.Reasoning != nil {
		r := map[string]any{}
		if req.Reasoning.Effort != "" {
			r["effort"] = req.Reasoning.Effort
		}
		if req.Reasoning.Summary != "" {
			r["summary"] = req.Reasoning.Summary
		}
		if req.Reasoning.Context != "" {
			r["context"] = req.Reasoning.Context
		}
		if req.Reasoning.Mode != "" {
			r["mode"] = req.Reasoning.Mode
		}
		if len(r) > 0 {
			out["reasoning"] = r
		}
	}
	if req.ResponseFormat != nil || req.Verbosity != "" {
		text := map[string]any{}
		if req.ResponseFormat != nil {
			text["format"] = encodeResponsesFormat(req.ResponseFormat)
		}
		if req.Verbosity != "" {
			text["verbosity"] = req.Verbosity
		}
		out["text"] = text
	}
	if req.Stream {
		out["stream"] = true
	}
	if req.Store != nil {
		out["store"] = *req.Store
	}
	if req.PreviousResponseID != "" {
		out["previous_response_id"] = req.PreviousResponseID
	}
	if len(req.Metadata) > 0 {
		out["metadata"] = req.Metadata
	}
	tier, tierWarns := mapServiceTier(ir.Responses, req.ServiceTier)
	req.Warnings = append(req.Warnings, tierWarns...)
	if tier != "" {
		out["service_tier"] = tier
	}
	if req.User != "" {
		out["user"] = req.User
	}
	if req.SafetyIdentifier != "" {
		out["safety_identifier"] = req.SafetyIdentifier
	}
	if req.PromptCacheKey != "" {
		out["prompt_cache_key"] = req.PromptCacheKey
	}
	if req.Moderation != nil {
		out["moderation"] = req.Moderation
	}
	if req.PromptCacheOptions != nil {
		out["prompt_cache_options"] = req.PromptCacheOptions
	}
	if req.PromptCacheRetention != "" {
		out["prompt_cache_retention"] = req.PromptCacheRetention
	}
	if req.Logprobs != nil && *req.Logprobs {
		out["include"] = []string{"message.output_text.logprobs"}
		// The request side maps, but the canonical response does not retain
		// provider logprob details, so the translated output cannot carry the
		// item the client asked for.
		req.Warnings.Add(ir.WarningApproximated, "logprobs", "translated responses do not carry logprob details")
	}
	return sse.MarshalWireJSON(out)
}

// effortFromBudget maps an Anthropic thinking budget to a Responses effort
// level. The buckets are the documented approximation used by prior art; the
// caller records a WarningApproximated.
func effortFromBudget(budget int) string {
	switch {
	case budget >= 10000:
		return "high"
	case budget >= 5000:
		return "medium"
	case budget >= 2000:
		return "low"
	default:
		return "minimal"
	}
}

// foldLeadingSystem joins the leading system/developer text run for the
// Responses instructions field. Folding bypasses the normal content encoder,
// so this owner reports every block and message attribute that the
// instructions string cannot carry.
func foldLeadingSystem(messages []ir.Message, warns *ir.Warnings) string {
	var parts []string
	for _, m := range messages {
		if m.Role != ir.RoleSystem && m.Role != ir.RoleDeveloper {
			break
		}
		if m.Name != "" && warns != nil {
			warns.Add(ir.WarningDropped, "messages.name", "Responses instructions have no participant name")
		}
		for _, b := range m.Content {
			if b.CacheControl != nil && warns != nil {
				warns.Add(ir.WarningDropped, "system.cache_control", "Responses instructions have no cache breakpoint")
			}
			if len(b.Citations) > 0 && warns != nil {
				warns.Add(ir.WarningDropped, "system.citations", "Responses instructions have no citations")
			}
			if b.Type != ir.BlockText {
				if warns != nil {
					warns.Add(ir.WarningDropped, "system."+string(b.Type), "Responses instructions are text-only")
				}
				continue
			}
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func encodeResponsesInput(req *ir.Request) []map[string]any {
	start := 0
	if req.Instructions == nil {
		for start < len(req.Messages) && (req.Messages[start].Role == ir.RoleSystem || req.Messages[start].Role == ir.RoleDeveloper) {
			start++
		}
	}
	var out []map[string]any
	for _, m := range req.Messages[start:] {
		if m.Name != "" {
			req.Warnings.Add(ir.WarningDropped, "messages.name", "Responses input messages have no participant name")
		}
		switch m.Role {
		case ir.RoleAssistant:
			var textBlocks []ir.Block
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockReasoning, ir.BlockRedactedReasoning:
					out = append(out, encodeResponsesReasoning(b.Reasoning, &req.Warnings))
				case ir.BlockToolCall:
					if b.CacheControl != nil {
						req.Warnings.Add(ir.WarningDropped, "cache_control", "Responses has no cache breakpoints")
					}
					out = append(out, encodeResponsesFunctionCall(b.ToolCall, ""))
				case ir.BlockText, ir.BlockRefusal:
					textBlocks = append(textBlocks, b)
				default:
					// OutputMessageContent is text/refusal only, so an
					// assistant media block cannot ride the same message
					// without producing an invalid part. Report the drop
					// instead of losing it silently.
					req.Warnings.Add(ir.WarningDropped, "assistant."+string(b.Type),
						"the Responses assistant input message accepts text, refusal, reasoning and tool calls only")
				}
			}
			if len(textBlocks) > 0 {
				out = append(out, map[string]any{
					"type": "message", "role": "assistant",
					"content": encodeResponsesContent(textBlocks, ir.RoleAssistant, &req.Warnings),
				})
			}
		case ir.RoleUser:
			var results []ir.Block
			var rest []ir.Block
			for _, b := range m.Content {
				if b.Type == ir.BlockToolResult {
					results = append(results, b)
				} else {
					rest = append(rest, b)
				}
			}
			for _, b := range results {
				if b.CacheControl != nil {
					req.Warnings.Add(ir.WarningDropped, "cache_control", "Responses has no cache breakpoints")
				}
				if b.ToolResult.IsError {
					req.Warnings.Add(ir.WarningDropped, "tool_result.is_error", "Responses function_call_output has no is_error flag")
				}
				out = append(out, map[string]any{
					"type":    "function_call_output",
					"call_id": b.ToolResult.CallID,
					"output":  encodeResponsesContent(b.ToolResult.Content, ir.RoleUser, &req.Warnings),
				})
			}
			if len(rest) > 0 {
				out = append(out, map[string]any{
					"type": "message", "role": "user",
					"content": encodeResponsesContent(rest, ir.RoleUser, &req.Warnings),
				})
			}
		case ir.RoleSystem, ir.RoleDeveloper:
			out = append(out, map[string]any{
				"type": "message", "role": string(m.Role),
				"content": encodeResponsesContent(m.Content, m.Role, &req.Warnings),
			})
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func encodeResponsesContent(blocks []ir.Block, role ir.Role, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		if b.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "cache_control", "per-part prompt_cache_breakpoint is not mapped")
		}
		if len(b.Citations) > 0 && warns != nil {
			warns.Add(ir.WarningDropped, "citations", "Responses input has no citations")
		}
		switch b.Type {
		case ir.BlockText:
			// This encoder serves the request input only. EasyInputMessage's
			// content union accepts input_text for every role; output_text is
			// an OutputMessage part and would make an assistant input item
			// match none of the valid input variants.
			out = append(out, map[string]any{"type": "input_text", "text": b.Text})
		case ir.BlockRefusal:
			// The request input union is input_text, input_image and
			// input_file; refusal is an OutputMessage part, and this encoder
			// emits a minimal EasyInputMessage with no id or status. Send the
			// refusal text as input text and report that the distinction is
			// lost instead of emitting an input part no schema accepts.
			if b.Refusal == "" {
				continue
			}
			out = append(out, map[string]any{"type": "input_text", "text": b.Refusal})
			if warns != nil {
				warns.AddOnce(ir.WarningDropped, "content.refusal",
					"Responses input has no refusal part; the refusal text is sent as input text")
			}
		case ir.BlockImage:
			part := map[string]any{"type": "input_image"}
			if b.Image.FileID != "" {
				part["file_id"] = b.Image.FileID
			} else if url := imageDataURL(b.Image); url != "" {
				part["image_url"] = url
			} else {
				continue
			}
			if b.Image.Detail != "" {
				part["detail"] = b.Image.Detail
			}
			out = append(out, part)
		case ir.BlockFile:
			part := map[string]any{"type": "input_file"}
			if b.File.FileID != "" {
				part["file_id"] = b.File.FileID
			}
			if data := fileDataURL(b.File); data != "" {
				part["file_data"] = data
			}
			if b.File.FileURL != "" {
				part["file_url"] = b.File.FileURL
			}
			if b.File.Filename != "" {
				part["filename"] = b.File.Filename
			}
			if b.File.Detail != "" {
				part["detail"] = b.File.Detail
			}
			out = append(out, part)
		case ir.BlockAudio:
			// The current Responses InputContent union is text, image and
			// file only; an input_audio part would be rejected upstream.
			if warns != nil {
				warns.Add(ir.WarningDropped, "content.input_audio", "Responses input has no audio part")
			}
		default:
			// The content-part union has no representation for a tool call,
			// reasoning or another structured block. Report the loss instead
			// of emitting a silently shortened message; the assistant branch
			// already carries its own representable blocks explicitly.
			if warns != nil {
				field := string(b.Type)
				if b.Type == ir.BlockRedactedReasoning {
					field = "reasoning"
				}
				warns.Add(ir.WarningDropped, "content."+field, responsesContentRoleDetail(role))
			}
		}
	}
	return out
}

// responsesContentRoleDetail describes what a Responses content part can carry
// for one role, so a dropped block's warning names the role-specific reason.
func responsesContentRoleDetail(role ir.Role) string {
	switch role {
	case ir.RoleAssistant:
		return "assistant content accepts text only"
	case ir.RoleSystem, ir.RoleDeveloper:
		return "system and developer content accepts text only"
	default:
		return "user content accepts text, images, files and audio only"
	}
}

func encodeResponsesReasoning(r *ir.Reasoning, warns *ir.Warnings) map[string]any {
	if r == nil {
		return map[string]any{"type": "reasoning", "summary": []any{}}
	}
	if r.Signature != "" && warns != nil {
		warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning signature cannot be replayed on Responses")
	}
	summary := make([]map[string]any, 0, len(r.Summary)+1)
	for _, s := range r.Summary {
		summary = append(summary, map[string]any{"type": "summary_text", "text": s})
	}
	// Responses has no visible raw-CoT item field; a foreign family's visible
	// reasoning is the closest thing to a summary, and preserving it beats
	// dropping it.
	if r.Text != "" && len(summary) == 0 {
		summary = append(summary, map[string]any{"type": "summary_text", "text": r.Text})
	}
	id := r.ID
	if id == "" {
		id = newResponsesItemID(ir.BlockReasoning)
	}
	item := map[string]any{"type": "reasoning", "summary": summary, "id": id}
	if r.Encrypted != "" {
		if r.Provider == "" || r.Provider == ir.Responses {
			item["encrypted_content"] = r.Encrypted
		} else if warns != nil {
			// A foreign encrypted artifact is provider-bound: sending it in a
			// Responses item would present bytes the target cannot verify.
			warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning artifact cannot be replayed on Responses")
		}
	}
	return item
}

// encodeResponsesFunctionCall renders one function-call item. A response
// output item carries its lifecycle status; a request-history item omits it.
func encodeResponsesFunctionCall(c *ir.ToolCall, status string) map[string]any {
	args := c.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	item := map[string]any{
		"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": string(args),
	}
	if status != "" {
		item["status"] = status
	}
	return item
}

func encodeResponsesTools(in []ir.Tool, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, t := range in {
		if t.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "tools.cache_control", "Responses function tools have no cache breakpoint")
		}
		tool := map[string]any{"type": "function", "name": t.Name}
		if t.Description != "" {
			tool["description"] = t.Description
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		tool["parameters"] = params
		if t.Strict != nil {
			tool["strict"] = *t.Strict
		}
		out = append(out, tool)
	}
	return out
}

func encodeResponsesToolChoice(tc *ir.ToolChoice) any {
	switch tc.Mode {
	case ir.ToolChoiceNone:
		return "none"
	case ir.ToolChoiceRequired:
		return "required"
	case ir.ToolChoiceTool:
		return map[string]any{"type": "function", "name": tc.Name}
	default:
		return "auto"
	}
}

func encodeResponsesFormat(rf *ir.ResponseFormat) map[string]any {
	switch rf.Type {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		out := map[string]any{"type": "json_schema", "schema": rf.Schema, "name": responseFormatName(rf)}
		if rf.Description != "" {
			out["description"] = rf.Description
		}
		if rf.Strict != nil {
			out["strict"] = *rf.Strict
		}
		return out
	default:
		return map[string]any{"type": "text"}
	}
}
