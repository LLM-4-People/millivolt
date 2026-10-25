package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// EncodeAnthropicRequest renders the canonical request as an Anthropic
// Messages body. defaultMaxTokens is injected when the IR carries no cap
// (Anthropic requires max_tokens); promptCacheKey is the proxy's tracked
// sub-conversation value.
func EncodeAnthropicRequest(req *ir.Request, defaultMaxTokens int, promptCacheKey string) ([]byte, error) {
	if err := ir.ValidateRequest(req); err != nil {
		return nil, err
	}
	if err := validateAnthropicToolVocabulary(req); err != nil {
		return nil, err
	}
	if req.PreviousResponseID != "" {
		return nil, errors.New("anthropic target cannot resolve previous_response_id")
	}
	if req.Store != nil && *req.Store {
		return nil, errors.New("anthropic target cannot store the response (store=true)")
	}
	if len(req.Metadata) > 0 {
		req.Warnings.Add(ir.WarningDropped, "metadata", "Anthropic Messages has no equivalent metadata map")
	}
	if req.Seed != nil {
		req.Warnings.Add(ir.WarningDropped, "seed", "Anthropic Messages has no seed")
	}
	if req.PresencePenalty != nil {
		req.Warnings.Add(ir.WarningDropped, "presence_penalty", "Anthropic Messages has no presence penalty")
	}
	if req.FrequencyPenalty != nil {
		req.Warnings.Add(ir.WarningDropped, "frequency_penalty", "Anthropic Messages has no frequency penalty")
	}
	if len(req.LogitBias) > 0 {
		req.Warnings.Add(ir.WarningDropped, "logit_bias", "Anthropic Messages has no logit bias")
	}
	if req.Logprobs != nil && *req.Logprobs {
		req.Warnings.Add(ir.WarningDropped, "logprobs", "Anthropic Messages has no logprobs")
	}
	if req.TopLogprobs != nil {
		req.Warnings.Add(ir.WarningDropped, "top_logprobs", "Anthropic Messages has no top_logprobs")
	}
	if req.Verbosity != "" {
		req.Warnings.Add(ir.WarningDropped, "verbosity", "Anthropic Messages has no verbosity")
	}
	if req.SafetyIdentifier != "" {
		req.Warnings.Add(ir.WarningDropped, "safety_identifier", "Anthropic Messages has no safety identifier")
	}
	if req.PromptCacheKey != "" {
		req.Warnings.Add(ir.WarningDropped, "prompt_cache_key", "Anthropic Messages has no prompt_cache_key")
	}
	if req.Moderation != nil {
		req.Warnings.Add(ir.WarningDropped, "moderation", "Anthropic Messages has no moderation configuration")
	}
	if req.PromptCacheOptions != nil {
		req.Warnings.Add(ir.WarningDropped, "prompt_cache_options", "Anthropic Messages uses cache_control, not prompt_cache_options")
	}
	if req.PromptCacheRetention != "" {
		req.Warnings.Add(ir.WarningDropped, "prompt_cache_retention", "Anthropic Messages uses cache_control, not prompt_cache_retention")
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		req.Warnings.Add(ir.WarningApproximated, "response_format.json_object", "Anthropic only supports json_schema")
	}
	system, messages := encodeAnthropicTurns(req.Messages, req.Instructions, &req.Warnings)
	if len(messages) == 0 {
		// Anthropic requires a non-empty messages array. Every input part can
		// be empty after translation, so fail closed before any upstream call
		// instead of emitting JSON null.
		return nil, errors.New("anthropic target requires at least one non-empty message")
	}
	out := map[string]any{
		"model":    req.Model,
		"messages": messages,
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	} else {
		out["max_tokens"] = defaultMaxTokens
	}
	out["stream"] = req.Stream
	if req.Temperature != nil {
		// Chat and Responses accept 0..2; Anthropic Messages accepts 0..1.
		// Clamping preserves the client's intent as closely as the target can
		// represent it instead of sending a value Anthropic rejects.
		temperature := *req.Temperature
		if temperature > 1 {
			temperature = 1
			req.Warnings.Add(ir.WarningApproximated, "temperature", "Anthropic Messages accepts 0..1")
		}
		out["temperature"] = temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if system != nil {
		out["system"] = system
	}
	if len(req.Tools) > 0 {
		out["tools"] = encodeAnthropicTools(req.Tools)
	}
	if tc := encodeAnthropicToolChoice(req.ToolChoice, req.ParallelToolCalls); tc != nil {
		out["tool_choice"] = tc
	}
	if thinking := encodeAnthropicThinking(req.Reasoning); thinking != nil {
		out["thinking"] = thinking
	}
	if oc := encodeAnthropicOutputConfig(req.ResponseFormat, req.Reasoning, &req.Warnings); oc != nil {
		out["output_config"] = oc
	}
	if req.User != "" {
		// Anthropic metadata.user_id is maxLength 512 in characters. An
		// overlong identifier is rejected rather than truncated, because a
		// truncated identifier can collide with a different caller.
		if utf8.RuneCountInString(req.User) > anthropicUserIDMax {
			return nil, fmt.Errorf("anthropic metadata.user_id exceeds %d characters", anthropicUserIDMax)
		}
		out["metadata"] = map[string]any{"user_id": req.User}
	}
	switch req.ServiceTier {
	case "fast":
		// Anthropic expresses fast processing with speed, not the service
		// tier vocabulary.
		out["speed"] = "fast"
	case "priority":
		// OpenAI's priority spelling is the other request opt-in for Fast
		// mode; Anthropic has one fast speed value, so the intent survives
		// with an approximation warning.
		out["speed"] = "fast"
		req.Warnings.Add(ir.WarningApproximated, "service_tier", "priority is a fast-mode request; Anthropic has one speed value")
	default:
		tier, tierWarns := mapServiceTier(ir.Anthropic, req.ServiceTier)
		req.Warnings = append(req.Warnings, tierWarns...)
		if tier != "" {
			out["service_tier"] = tier
		}
	}
	if req.CacheControl != nil {
		out["cache_control"] = req.CacheControl
	}
	if promptCacheKey != "" {
		out["prompt_cache_key"] = promptCacheKey
	}
	return sse.MarshalWireJSON(out)
}

type anthropicOutMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicSystemTextBlocks renders the text-only part of a system or
// developer turn. Anthropic system content cannot carry non-text blocks, and
// TextBlockParam.text has minLength 1, so both are dropped with a warning.
func anthropicSystemTextBlocks(content []ir.Block, warns *ir.Warnings) []map[string]any {
	var blocks []map[string]any
	for _, b := range content {
		if b.Type != ir.BlockText {
			if warns != nil {
				warns.Add(ir.WarningDropped, "system."+string(b.Type), "Anthropic system prompts are text-only")
			}
			continue
		}
		if b.Text == "" {
			continue
		}
		block := map[string]any{"type": "text", "text": b.Text}
		if b.CacheControl != nil {
			block["cache_control"] = b.CacheControl
		}
		if len(b.Citations) > 0 {
			if citations := anthropicCitations(b.Citations, warns); len(citations) > 0 {
				block["citations"] = citations
			}
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// anthropicTextContent keeps the historical string join for plain text while a
// cache breakpoint or citation requires the block-array form. Both the
// top-level and in-conversation system encoders share it.
func anthropicTextContent(blocks []map[string]any) any {
	if len(blocks) == 0 {
		return nil
	}
	for _, block := range blocks {
		if _, ok := block["cache_control"]; ok {
			return blocks
		}
		if _, ok := block["citations"]; ok {
			return blocks
		}
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// encodeAnthropicTurns renders the message history and the top-level system
// field in one pass. The top-level field owns the Responses instructions and
// every system or developer turn that arrives before the first emitted
// conversation turn. "First emitted turn" is out's emptiness, not the presence
// of a non-system input: a leading assistant turn can be dropped and an empty
// leading user turn can encode to nothing, so neither may push a later system
// turn into messages where Anthropic rejects it. A mid-conversation system run
// stays in messages only after an emitted user turn and before an assistant
// turn or the end; any other position is lifted to the top-level prompt with
// one approximation warning. A later developer turn has no Anthropic role and
// is lifted with one approximation warning.
func encodeAnthropicTurns(messages []ir.Message, instructions *string, warns *ir.Warnings) (any, []anthropicOutMessage) {
	var systemBlocks []map[string]any
	if instructions != nil && *instructions != "" {
		systemBlocks = append(systemBlocks, map[string]any{"type": "text", "text": *instructions})
	}
	var out []anthropicOutMessage
	leadingAssistantDropped := false
	droppedToolCalls := map[string]struct{}{}
	orphanResultDropped := false
	developerLifted := false
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Name != "" && warns != nil {
			warns.Add(ir.WarningDropped, "messages.name", "Anthropic Messages has no participant name")
		}
		switch m.Role {
		case ir.RoleSystem:
			// A mid-conversation system run is valid only after a user turn and
			// before an assistant turn (or at the end). Anthropic rejects it
			// between user turns and between a tool_use and its tool_result.
			// Judge the whole contiguous run, then lift it to the top-level
			// prompt with one approximation warning when its position is not
			// valid. A leading run always belongs at the top level.
			end := i + 1
			for end < len(messages) && messages[end].Role == ir.RoleSystem {
				end++
			}
			blocks := anthropicSystemTextBlocks(m.Content, warns)
			for j := i + 1; j < end; j++ {
				if messages[j].Name != "" && warns != nil {
					warns.Add(ir.WarningDropped, "messages.name", "Anthropic Messages has no participant name")
				}
				blocks = append(blocks, anthropicSystemTextBlocks(messages[j].Content, warns)...)
			}
			next := ir.Role("")
			for j := end; j < len(messages); j++ {
				if messages[j].Role != ir.RoleSystem {
					next = messages[j].Role
					break
				}
			}
			previous := ir.Role("")
			if len(out) > 0 {
				previous = ir.Role(out[len(out)-1].Role)
			}
			if len(out) == 0 || (previous == ir.RoleUser && (next == "" || next == ir.RoleAssistant)) {
				if len(out) == 0 {
					systemBlocks = append(systemBlocks, blocks...)
				} else {
					out = appendAnthropicTurn(out, string(m.Role), anthropicTextContent(blocks), warns)
				}
			} else {
				systemBlocks = append(systemBlocks, blocks...)
				if len(blocks) > 0 && warns != nil {
					warns.AddOnce(ir.WarningApproximated, "system", "a system message was moved before the conversation because Anthropic only accepts one after a user turn and before an assistant turn")
				}
			}
			i = end - 1
		case ir.RoleDeveloper:
			if len(out) != 0 && !developerLifted {
				developerLifted = true
				if warns != nil {
					warns.Add(ir.WarningApproximated, "system", "Anthropic Messages has no developer role; a later developer message was moved before the conversation")
				}
			}
			systemBlocks = append(systemBlocks, anthropicSystemTextBlocks(m.Content, warns)...)
		case ir.RoleAssistant:
			// Anthropic requires the first emitted turn to use the user role.
			// A source history may begin with an assistant turn (or a leading
			// user turn may have encoded to nothing), so drop the leading
			// assistant turns rather than prepending a fabricated user turn.
			// One warning covers every dropped turn, and the tool-call ids are
			// retained so their results can be dropped as orphans.
			if len(out) == 0 {
				for _, b := range m.Content {
					if b.Type == ir.BlockToolCall && b.ToolCall != nil && b.ToolCall.ID != "" {
						droppedToolCalls[b.ToolCall.ID] = struct{}{}
					}
				}
				if !leadingAssistantDropped {
					leadingAssistantDropped = true
					if warns != nil {
						warns.Add(ir.WarningDropped, "messages.leading_assistant", "Anthropic Messages requires the first turn to use the user role")
					}
				}
				continue
			}
			out = appendAnthropicTurn(out, string(m.Role), encodeAnthropicContent(m.Content, m.Role, warns), warns)
		default:
			contentBlocks := m.Content
			if m.Role == ir.RoleUser && len(droppedToolCalls) > 0 {
				// A result whose tool_use was in a dropped leading assistant
				// turn would be an orphan: Anthropic requires every tool_result
				// to answer a preceding assistant tool_use. Keep the user
				// turn's other blocks and report the loss once.
				filtered := make([]ir.Block, 0, len(m.Content))
				for _, b := range m.Content {
					if b.Type == ir.BlockToolResult && b.ToolResult != nil {
						if _, orphan := droppedToolCalls[b.ToolResult.CallID]; orphan {
							if !orphanResultDropped {
								orphanResultDropped = true
								if warns != nil {
									warns.Add(ir.WarningDropped, "messages.orphan_tool_result", "tool results for dropped leading assistant tool calls were dropped")
								}
							}
							continue
						}
					}
					filtered = append(filtered, b)
				}
				contentBlocks = filtered
			}
			out = appendAnthropicTurn(out, string(m.Role), encodeAnthropicContent(contentBlocks, m.Role, warns), warns)
		}
	}
	out = liftMisplacedAnthropicSystemRuns(out, &systemBlocks, warns)
	return anthropicTextContent(systemBlocks), out
}

// liftMisplacedAnthropicSystemRuns applies the emitted-role placement rule as
// a post-pass. Placement is decided from the source roles during the loop, but
// an empty assistant turn or an unrepresentable user turn can encode to
// nothing, leaving the system run between two emitted user turns - a shape
// Anthropic rejects. A system run is valid only before an emitted assistant
// turn or at the end; every other kept run moves to the top-level prompt with
// one approximation warning. The pass may leave adjacent user turns; Anthropic
// merges them server-side, and the tool_result adjacency the encoder owns is
// not disturbed.
func liftMisplacedAnthropicSystemRuns(out []anthropicOutMessage, systemBlocks *[]map[string]any, warns *ir.Warnings) []anthropicOutMessage {
	if len(out) == 0 {
		return out
	}
	kept := out[:0]
	for i, turn := range out {
		if turn.Role == "system" && i+1 < len(out) && out[i+1].Role != "assistant" {
			blocks := anthropicOutSystemBlocks(turn.Content)
			*systemBlocks = append(*systemBlocks, blocks...)
			if len(blocks) > 0 && warns != nil {
				warns.AddOnce(ir.WarningApproximated, "system", "a system message was moved before the conversation because Anthropic only accepts one after a user turn and before an assistant turn")
			}
			continue
		}
		kept = append(kept, turn)
	}
	return kept
}

// anthropicOutSystemBlocks converts one already-rendered in-conversation
// system turn back to the block form the top-level system field uses.
func anthropicOutSystemBlocks(content any) []map[string]any {
	switch rendered := content.(type) {
	case string:
		if rendered == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": rendered}}
	case []map[string]any:
		return rendered
	}
	return nil
}

// appendAnthropicTurn appends one rendered turn and merges it into a
// consecutive same-role predecessor. Anthropic merges consecutive same-role
// turns, and doing it here keeps tool_result blocks immediately after their
// tool_use turn. A merge appends the later turn after the earlier one, so the
// assembled user turn can put text before a tool_result; re-apply the
// results-first rule the per-turn encoder already owns and report the move.
func appendAnthropicTurn(out []anthropicOutMessage, role string, content any, warns *ir.Warnings) []anthropicOutMessage {
	if content == nil {
		return out
	}
	if n := len(out); n > 0 && out[n-1].Role == role {
		merged := mergeAnthropicContent(out[n-1].Content, content)
		if blocks, ok := merged.([]map[string]any); ok {
			if ordered, moved := orderAnthropicRenderedResultsFirst(blocks); moved {
				merged = ordered
				if warns != nil {
					warns.AddOnce(ir.WarningApproximated, "messages.tool_result",
						"tool results were moved before other content in a merged user turn")
				}
			}
		}
		out[n-1].Content = merged
		return out
	}
	return append(out, anthropicOutMessage{Role: role, Content: content})
}

// orderAnthropicRenderedResultsFirst moves every rendered tool_result block to
// the front of one user turn, preserving the relative order within each group.
// It reports whether a move occurred. The canonical-block twin is the ordering
// step in encodeAnthropicBlocks; this owner covers a turn assembled from
// already-rendered pieces.
func orderAnthropicRenderedResultsFirst(blocks []map[string]any) ([]map[string]any, bool) {
	seenOther := false
	moved := false
	for _, block := range blocks {
		if block["type"] == "tool_result" {
			if seenOther {
				moved = true
				break
			}
			continue
		}
		seenOther = true
	}
	if !moved {
		return blocks, false
	}
	ordered := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] == "tool_result" {
			ordered = append(ordered, block)
		}
	}
	for _, block := range blocks {
		if block["type"] != "tool_result" {
			ordered = append(ordered, block)
		}
	}
	return ordered, true
}

// encodeAnthropicContent renders a simple all-text turn as a string (the
// shape the Chat translator historically emitted) and every structured turn as
// a block array.
func encodeAnthropicContent(blocks []ir.Block, role ir.Role, warns *ir.Warnings) any {
	if len(blocks) == 0 {
		return nil
	}
	allText := true
	var text strings.Builder
	for _, b := range blocks {
		if b.Type != ir.BlockText || b.CacheControl != nil || len(b.Citations) > 0 {
			allText = false
			break
		}
		text.WriteString(b.Text)
	}
	if allText {
		// Anthropic's TextBlockParam.text has minLength 1. An all-empty
		// text-only turn contributes no content at all, matching the
		// zero-block skip below.
		if text.Len() == 0 {
			return nil
		}
		return text.String()
	}
	return encodeAnthropicBlocks(blocks, role, warns)
}

func mergeAnthropicContent(prev, next any) any {
	prevBlocks, prevOK := prev.([]map[string]any)
	nextBlocks, nextOK := next.([]map[string]any)
	if prevOK && nextOK {
		return append(prevBlocks, nextBlocks...)
	}
	// At least one side is a plain string; promote both to text blocks.
	toBlocks := func(v any) []map[string]any {
		if blocks, ok := v.([]map[string]any); ok {
			return blocks
		}
		if s, ok := v.(string); ok {
			return []map[string]any{{"type": "text", "text": s}}
		}
		return nil
	}
	return append(toBlocks(prev), toBlocks(next)...)
}

// isAnthropicCitation reports whether a citation carries a shape Anthropic's
// text block accepts. Chat and Responses annotations are url_citation only.
func isAnthropicCitation(c ir.Citation) bool {
	switch c.Type {
	case "char_location", "page_location", "content_block_location",
		"web_search_result_location", "search_result_location":
		return true
	}
	return false
}

func anthropicCitations(in []ir.Citation, warns *ir.Warnings) []ir.Citation {
	var out []ir.Citation
	for _, c := range in {
		if !isAnthropicCitation(c) {
			if warns != nil {
				warns.Add(ir.WarningDropped, "citations", "citation type has no Anthropic equivalent")
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

func encodeAnthropicBlocks(blocks []ir.Block, role ir.Role, warns *ir.Warnings) []map[string]any {
	// Tool results must come first in a user turn.
	var results, rest []ir.Block
	for _, b := range blocks {
		if b.Type == ir.BlockToolResult {
			results = append(results, b)
		} else {
			rest = append(rest, b)
		}
	}
	ordered := append(append([]ir.Block{}, results...), rest...)
	out := make([]map[string]any, 0, len(ordered))
	for _, b := range ordered {
		switch b.Type {
		case ir.BlockText:
			// Anthropic rejects an empty text block (minLength 1). Drop it
			// with its cache control and citations rather than emitting an
			// invalid block; a structured turn keeps its other blocks.
			if b.Text == "" {
				continue
			}
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			if len(b.Citations) > 0 {
				if citations := anthropicCitations(b.Citations, warns); len(citations) > 0 {
					block["citations"] = citations
				}
			}
			out = append(out, block)
		case ir.BlockImage:
			source := encodeAnthropicImage(b.Image, warns)
			if source == nil {
				continue
			}
			if role == ir.RoleAssistant && warns != nil {
				// AssistantMessageParam.content uses the shared
				// MessageContentParam type, so the block is schema-valid, but
				// assistant media is unusual and provider acceptance is not
				// verified. Keep the representable block and report the
				// approximation.
				warns.Add(ir.WarningApproximated, "assistant.image", "assistant media is schema-valid on Anthropic but runtime acceptance is not verified")
			}
			block := map[string]any{"type": "image", "source": source}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			out = append(out, block)
		case ir.BlockFile:
			source := encodeAnthropicFile(b.File, warns)
			if source == nil {
				continue
			}
			if role == ir.RoleAssistant && warns != nil {
				warns.Add(ir.WarningApproximated, "assistant.document", "assistant media is schema-valid on Anthropic but runtime acceptance is not verified")
			}
			block := map[string]any{"type": "document", "source": source}
			if b.File != nil && b.File.Filename != "" {
				if utf8.RuneCountInString(b.File.Filename) <= anthropicDocumentTitleMax {
					block["title"] = b.File.Filename
				} else if warns != nil {
					warns.Add(ir.WarningDropped, "document.title",
						"Anthropic document titles are limited to 500 characters")
				}
			}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			out = append(out, block)
		case ir.BlockAudio:
			if warns != nil {
				warns.Add(ir.WarningDropped, "audio", "Anthropic Messages has no audio input")
			}
		case ir.BlockToolCall:
			input := b.ToolCall.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			block := map[string]any{
				"type": "tool_use", "id": b.ToolCall.ID, "name": b.ToolCall.Name, "input": input,
			}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			out = append(out, block)
		case ir.BlockToolResult:
			content := encodeAnthropicToolResultContent(b.ToolResult.Content, warns)
			block := map[string]any{
				"type": "tool_result", "tool_use_id": b.ToolResult.CallID, "content": content,
			}
			if b.ToolResult.IsError {
				block["is_error"] = true
			}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			out = append(out, block)
		case ir.BlockReasoning:
			if b.CacheControl != nil && warns != nil {
				warns.Add(ir.WarningDropped, "reasoning.cache_control", "Anthropic thinking blocks have no cache breakpoint")
			}
			native := b.Reasoning.Signature != "" || b.Reasoning.Provider == ir.Anthropic
			if native {
				out = append(out, map[string]any{
					"type": "thinking", "thinking": b.Reasoning.Text, "signature": b.Reasoning.Signature,
				})
				// Anthropic thinking has one text payload; a summary that is
				// not the same visible text has no separate slot.
				if len(b.Reasoning.Summary) > 0 && warns != nil {
					warns.Add(ir.WarningDropped, "reasoning.summary", "Anthropic thinking has no separate reasoning summary")
				}
			} else if b.Reasoning.Text != "" {
				// A reasoning block minted by another family has no Anthropic
				// signature; emitting it as text preserves the visible content.
				// An encrypted artifact is still unreplayable, so that loss is
				// reported alongside the approximation, matching the
				// summary-only branch.
				if warns != nil {
					warns.Add(ir.WarningApproximated, "reasoning", "cross-protocol reasoning was emitted as visible text")
					if b.Reasoning.Encrypted != "" {
						warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning artifact cannot be replayed on Anthropic Messages")
					}
					if len(b.Reasoning.Summary) > 0 {
						warns.Add(ir.WarningDropped, "reasoning.summary", "Anthropic text has no separate reasoning summary")
					}
				}
				out = append(out, map[string]any{"type": "text", "text": b.Reasoning.Text})
			} else if len(b.Reasoning.Summary) > 0 {
				// No raw text is available; the summaries are the only visible
				// reasoning, so fold them into text with the same approximation
				// the Chat encoder applies instead of dropping the block. An
				// encrypted artifact is still unreplayable, so that loss stays
				// reported alongside the approximation.
				if warns != nil {
					warns.Add(ir.WarningApproximated, "reasoning", "cross-protocol reasoning summary was emitted as visible text")
					if b.Reasoning.Encrypted != "" {
						warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning artifact cannot be replayed on Anthropic Messages")
					}
				}
				out = append(out, map[string]any{"type": "text", "text": strings.Join(b.Reasoning.Summary, "")})
			} else if b.Reasoning.Encrypted != "" {
				if warns != nil {
					warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning artifact cannot be replayed on Anthropic Messages")
				}
			} else if warns != nil {
				warns.Add(ir.WarningDropped, "reasoning", "reasoning block has no representable content")
			}
		case ir.BlockRedactedReasoning:
			if b.Reasoning.Provider == ir.Anthropic && b.Reasoning.Encrypted != "" {
				out = append(out, map[string]any{"type": "redacted_thinking", "data": b.Reasoning.Encrypted})
			} else if warns != nil {
				warns.Add(ir.WarningOpaque, "reasoning", "redacted reasoning cannot be replayed on Anthropic Messages")
			}
		case ir.BlockRefusal:
			// A refusal becomes a text block, which Anthropic also rejects
			// when empty (minLength 1). Drop it explicitly instead of
			// emitting an invalid block; a non-empty refusal keeps its cache
			// breakpoint like any other text block.
			if b.Refusal == "" {
				if warns != nil {
					warns.Add(ir.WarningDropped, "content.refusal", "empty refusal has no representable content")
				}
				continue
			}
			block := map[string]any{"type": "text", "text": b.Refusal}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			out = append(out, block)
		}
	}
	return out
}
