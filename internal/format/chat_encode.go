package format

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// EncodeChatRequest renders the canonical request as a Chat Completions body.
func EncodeChatRequest(req *ir.Request) ([]byte, error) {
	if err := ir.ValidateRequest(req); err != nil {
		return nil, err
	}
	if err := validateChatToolVocabulary(req); err != nil {
		return nil, err
	}
	if err := validateResponseFormatName("chat", req.ResponseFormat); err != nil {
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
	encodedMessages := encodeChatMessages(messages, &req.Warnings)
	if len(encodedMessages) == 0 {
		// Chat requires at least one message. An empty Responses input, or a
		// turn whose every part was dropped, must fail closed instead of
		// emitting messages:[] (the Anthropic encoder's twin guard).
		return nil, errors.New("chat target requires at least one non-empty message")
	}
	out := map[string]any{
		"model":    req.Model,
		"messages": encodedMessages,
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
	if req.Moderation != nil {
		out["moderation"] = req.Moderation
	}
	if req.PromptCacheOptions != nil {
		options := *req.PromptCacheOptions
		if options.Prewarm != nil {
			req.Warnings.Add(ir.WarningDropped, "prompt_cache_options.prewarm", "Chat has no prompt cache prewarm")
			options.Prewarm = nil
		}
		if options.ComparisonResponseID != "" {
			req.Warnings.Add(ir.WarningDropped, "prompt_cache_options.comparison_response_id", "Chat has no prompt cache diagnostics")
			options.ComparisonResponseID = ""
		}
		if options.TTL != "" || options.Mode != "" {
			out["prompt_cache_options"] = options
		}
	}
	if req.PromptCacheRetention != "" {
		out["prompt_cache_retention"] = req.PromptCacheRetention
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
			warns.Add(ir.WarningDropped, "cache_control", "per-part prompt_cache_breakpoint is not mapped")
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
			// A response can split one refusal across several parts. Chat has
			// one refusal field, so concatenate in block order; an empty last
			// part must not erase the earlier text.
			refusal += b.Refusal
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
			warns.Add(ir.WarningDropped, "cache_control", "per-part prompt_cache_breakpoint is not mapped")
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
// URL has no representation and is reported. The span is relative to the
// message content, while each IR citation is relative to its own text block,
// so the running rune length of preceding text blocks is added to every span.
// A message encoded as a parts array (an image is present) has no precise
// message-level span definition; the concatenated-text offset is the best
// available mapping and matches what text-only clients see.
func chatMessageAnnotations(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	offset := 0
	for _, b := range blocks {
		if len(b.Citations) == 0 {
			if b.Type == ir.BlockText {
				offset += utf8.RuneCountInString(b.Text)
			}
			continue
		}
		if b.Type != ir.BlockText {
			if warns != nil {
				warns.Add(ir.WarningDropped, "citations", "citation is not attached to a text block")
			}
			continue
		}
		// The source location type may carry no character span; Chat requires
		// both span fields, so the 0..0 range is fabricated. Report it once for
		// this text block.
		for _, c := range b.Citations {
			if c.URL != "" && c.Type != "url_citation" && c.StartIndex == 0 && c.EndIndex == 0 {
				if warns != nil {
					warns.Add(ir.WarningApproximated, "citations", "source citation has no character span; target span is 0..0")
				}
				break
			}
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
					"start_index": offset + c.StartIndex, "end_index": offset + c.EndIndex,
				},
			})
		}
		offset += utf8.RuneCountInString(b.Text)
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
