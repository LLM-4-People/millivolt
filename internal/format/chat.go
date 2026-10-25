package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		PromptCacheBreakpoint json.RawMessage `json:"prompt_cache_breakpoint"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, err
	}
	var (
		out   []ir.Block
		warns ir.Warnings
	)
	for _, p := range parts {
		if rawPresent(p.PromptCacheBreakpoint) {
			warns.Add(ir.WarningDropped, "prompt_cache_breakpoint", "per-part prompt_cache_breakpoint is not mapped")
		}
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
// attachChatAnnotations decodes message-level url_citation annotations into
// citations on the text block that contains each message-relative span. The
// Chat response decoder and the assistant request decoder (a client replaying a
// prior response) share this owner, so the non-standard extension is preserved
// instead of silently dropped. A span outside the message, or one that crosses
// a text-block boundary, has no IR representation and is reported.
func attachChatAnnotations(blocks []ir.Block, annotations []chatWireAnnotation) ([]ir.Block, ir.Warnings, error) {
	citations, warns, err := decodeChatAnnotations(annotations)
	if err != nil {
		return nil, nil, err
	}
	if len(citations) == 0 {
		return blocks, warns, nil
	}
	hasText := false
	for _, b := range blocks {
		if b.Type == ir.BlockText {
			hasText = true
			break
		}
	}
	if !hasText {
		warns.Add(ir.WarningDropped, "annotations", "Chat annotations have no text block to attach to")
		return blocks, warns, nil
	}
	offsets := make([]int, len(blocks))
	textLength := 0
	for i, b := range blocks {
		offsets[i] = textLength
		if b.Type == ir.BlockText {
			textLength += utf8.RuneCountInString(b.Text)
		}
	}
	for _, c := range citations {
		if c.StartIndex < 0 || c.EndIndex < c.StartIndex || c.EndIndex > textLength {
			warns.Add(ir.WarningDropped, "annotations.span", "url_citation span falls outside the message text")
			continue
		}
		if !attachChatCitation(blocks, offsets, c) {
			warns.Add(ir.WarningDropped, "annotations.span", "url_citation span crosses a text block boundary")
		}
	}
	return blocks, warns, nil
}

// attachChatCitation assigns one message-relative citation to the text block
// whose rune range contains it, rebasing the span to that block.
func attachChatCitation(blocks []ir.Block, offsets []int, c ir.Citation) bool {
	for i := range blocks {
		if blocks[i].Type != ir.BlockText {
			continue
		}
		end := offsets[i] + utf8.RuneCountInString(blocks[i].Text)
		if c.StartIndex < offsets[i] || c.EndIndex > end {
			continue
		}
		c.StartIndex -= offsets[i]
		c.EndIndex -= offsets[i]
		blocks[i].Citations = append(blocks[i].Citations, c)
		return true
	}
	return false
}

func decodeChatAnnotations(in []chatWireAnnotation) ([]ir.Citation, ir.Warnings, error) {
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
		start, err := intFromJSONIndex("url_citation.start_index", a.URLCitation.StartIndex)
		if err != nil {
			return nil, nil, err
		}
		end, err := intFromJSONIndex("url_citation.end_index", a.URLCitation.EndIndex)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, ir.Citation{
			Type: "url_citation", URL: a.URLCitation.URL, Title: a.URLCitation.Title,
			StartIndex: start, EndIndex: end,
		})
	}
	return out, warns, nil
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
