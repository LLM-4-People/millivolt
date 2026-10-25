package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// DecodeAnthropicRequest converts one Anthropic Messages request into the
// canonical request.
func DecodeAnthropicRequest(body []byte) (*ir.Request, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("anthropic request body is not valid UTF-8")
	}
	// Bound the collections before the typed decode materializes them. The
	// proxy preflights messages, tools and system content for its own relay
	// path; the codec owns the same guarantee for every caller, including the
	// stop_sequences list no earlier layer bounds.
	var bounds struct {
		Messages      json.RawMessage `json:"messages"`
		Tools         json.RawMessage `json:"tools"`
		System        json.RawMessage `json:"system"`
		StopSequences json.RawMessage `json:"stop_sequences"`
	}
	if err := json.Unmarshal(body, &bounds); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	for _, b := range []struct {
		raw   json.RawMessage
		field string
	}{
		{bounds.Messages, "messages"},
		{bounds.Tools, "tools"},
		{bounds.System, "system"},
		{bounds.StopSequences, "stop_sequences"},
	} {
		if err := checkWireArray(b.raw, b.field, WireCollectionMax); err != nil {
			return nil, err
		}
	}
	if err := checkStringArray(bounds.StopSequences, "stop_sequences"); err != nil {
		return nil, err
	}
	// System citations and every nested tool_result/document content array
	// share one aggregate: bounded parent arrays alone multiply through
	// nesting. This runs before the typed request decode.
	if err := checkAnthropicRequestCollections(bounds.System, bounds.Messages); err != nil {
		return nil, err
	}
	var in anthropicWireRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, errors.New("anthropic request: model is required")
	}
	maxTokens, err := jsonnum.OptionalInt(in.MaxTokens, "max_tokens")
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if maxTokens == nil {
		return nil, errors.New("anthropic request: max_tokens is required")
	}
	topK, err := jsonnum.OptionalInt(in.TopK, "top_k")
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	req := &ir.Request{
		Model:           in.Model,
		MaxOutputTokens: maxTokens,
		Temperature:     in.Temperature,
		TopP:            in.TopP,
		TopK:            topK,
		Stop:            in.StopSequences,
		Stream:          in.Stream,
		ServiceTier:     in.ServiceTier,
		CacheControl:    in.CacheControl,
	}
	if in.Metadata != nil {
		req.User = in.Metadata.UserID
	}
	if len(in.System) > 0 {
		blocks, warns, err := decodeAnthropicSystem(in.System)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, ir.Message{Role: ir.RoleSystem, Content: blocks})
		req.Warnings = append(req.Warnings, warns...)
	}
	ids := newRequestToolIDs()
	for i, m := range in.Messages {
		if err := preReserveAnthropicToolIDs(ids, m.Content); err != nil {
			return nil, fmt.Errorf("anthropic message %d: %w", i, err)
		}
	}
	for i, m := range in.Messages {
		// One accumulator spans every message so AddOnce sees the whole
		// request instead of restarting at each message.
		msg, err := decodeAnthropicMessage(m, &req.Warnings, ids)
		if err != nil {
			return nil, fmt.Errorf("anthropic message %d: %w", i, err)
		}
		req.Messages = append(req.Messages, msg)
	}
	if len(in.Tools) > 0 {
		tools, warns, err := decodeAnthropicTools(in.Tools)
		if err != nil {
			return nil, err
		}
		req.Tools = tools
		req.Warnings = append(req.Warnings, warns...)
	}
	if tc, parallel, err := decodeAnthropicToolChoice(in.ToolChoice); err != nil {
		return nil, err
	} else if tc != nil {
		req.ToolChoice = tc
		req.ParallelToolCalls = parallel
	}
	if r, warns, err := decodeAnthropicThinking(in.Thinking, maxTokens); err != nil {
		return nil, err
	} else if r != nil {
		req.Reasoning = r
		req.Warnings = append(req.Warnings, warns...)
	}
	if rf, effort, warns, err := decodeAnthropicOutputConfig(in.OutputConfig); err != nil {
		return nil, err
	} else {
		req.ResponseFormat = rf
		if effort != "" {
			if req.Reasoning == nil {
				req.Reasoning = &ir.ReasoningConfig{}
			}
			req.Reasoning.Effort = effort
		}
		req.Warnings = append(req.Warnings, warns...)
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"container", in.Container},
		{"diagnostics", in.Diagnostics},
	} {
		if rawPresent(field.raw) {
			req.Warnings.Add(ir.WarningDropped, field.name, "the canonical request has no Anthropic "+field.name+" state")
		}
	}
	if in.InferenceGeo != "" {
		req.Warnings.Add(ir.WarningDropped, "inference_geo", "the canonical request has no data-residency control")
	}
	if in.Speed != "" {
		switch in.Speed {
		case "standard":
			// Standard processing is the default; no tier is needed.
		case "fast":
			if req.ServiceTier == "" || req.ServiceTier == "auto" {
				req.ServiceTier = "fast"
				req.Warnings.Add(ir.WarningApproximated, "speed", "mapped Anthropic fast speed to service_tier fast")
			} else {
				req.Warnings.Add(ir.WarningDropped, "speed", "service_tier already selects the target tier")
			}
		default:
			req.Warnings.Add(ir.WarningDropped, "speed", "unsupported speed value")
		}
	}
	if err := ir.ValidateRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

func decodeAnthropicSystem(raw json.RawMessage) ([]ir.Block, ir.Warnings, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil, nil
		}
		return []ir.Block{ir.TextBlock(s)}, nil, nil
	}
	var parts []anthropicWireBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, fmt.Errorf("anthropic system: %w", err)
	}
	var (
		blocks []ir.Block
		warns  ir.Warnings
	)
	for _, p := range parts {
		if p.Type != "" && p.Type != "text" {
			warns.Add(ir.WarningDropped, "system."+p.Type, "unsupported system block")
			continue
		}
		citations, w := decodeAnthropicCitations(p.Citations, "text")
		warns = append(warns, w...)
		blocks = append(blocks, ir.Block{
			Type: ir.BlockText, Text: p.Text, CacheControl: p.CacheControl, Citations: citations,
		})
	}
	return blocks, warns, nil
}

// preReserveAnthropicToolIDs claims every explicit tool_use id in one message
// before any block is decoded. A later id-less tool_use then synthesizes
// against the whole request instead of colliding with an explicit id that
// appears after it.
func preReserveAnthropicToolIDs(ids *requestToolIDs, content json.RawMessage) error {
	return preReserveAnthropicContent(ids, content, "")
}

// preReserveAnthropicContent walks nested tool_result and document source
// content so an explicit id anywhere in the request participates in the
// namespace. A parse failure is left to the typed decoder, which owns the
// error; the pre-scan only needs the arrays it can read.
func preReserveAnthropicContent(ids *requestToolIDs, raw json.RawMessage, prefix string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var parts []anthropicWireBlock
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil
	}
	for i, p := range parts {
		where := fmt.Sprintf("block %d", i)
		if prefix != "" {
			where = prefix + " " + where
		}
		switch p.Type {
		case "tool_use":
			if p.ID == "" {
				continue
			}
			if err := ids.preReserve(p.ID); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		case "tool_result":
			if err := preReserveAnthropicContent(ids, p.Content, where+" content"); err != nil {
				return err
			}
		case "document":
			// Only a content-source document contributes nested blocks; the
			// other source types carry no content array the decoder reads.
			if p.Source != nil && p.Source.Type == "content" {
				if err := preReserveAnthropicContent(ids, p.Source.Content, where+" source content"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeAnthropicMessage(m anthropicWireMsg, warns *ir.Warnings, ids *requestToolIDs) (ir.Message, error) {
	role := ir.Role(m.Role)
	switch role {
	case ir.RoleUser, ir.RoleAssistant, ir.RoleSystem:
	default:
		return ir.Message{}, fmt.Errorf("unsupported role %q", m.Role)
	}
	blocks, err := decodeAnthropicContent(m.Content, warns, ids)
	if err != nil {
		return ir.Message{}, err
	}
	return ir.Message{Role: role, Content: blocks}, nil
}

func decodeAnthropicContent(raw json.RawMessage, warns *ir.Warnings, ids *requestToolIDs) ([]ir.Block, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ir.Block{ir.TextBlock(s)}, nil
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return nil, fmt.Errorf("anthropic content block array exceeds %d blocks", metrics.ContentPartMax)
	}
	if err := checkAnthropicCitations(raw, "content"); err != nil {
		return nil, err
	}
	var parts []anthropicWireBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	return decodeAnthropicBlocks(parts, warns, ids)
}

// syntheticToolID is a bounded, schema-conforming id for an Anthropic tool
// call that omitted its required id. The block index keeps it unique within
// one response.
func syntheticToolID(index int) string {
	return fmt.Sprintf("toolu_proxy_%d", index)
}

// decodeAnthropicCitations decodes one block's citations member by block
// type. Text blocks (including system and tool-result text) carry a citation
// array. A document block carries the CitationsConfigParam object instead; the
// canonical model has no document-citation configuration, so it is reported
// and never turned into text citations.
func decodeAnthropicCitations(raw json.RawMessage, blockType string) ([]ir.Citation, ir.Warnings) {
	trimmed := bytes.TrimSpace(raw)
	var warns ir.Warnings
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, warns
	}
	switch trimmed[0] {
	case '[':
		var citations []ir.Citation
		if err := json.Unmarshal(trimmed, &citations); err != nil {
			warns.Add(ir.WarningDropped, "citations", "citation array could not be decoded")
			return nil, warns
		}
		return citations, warns
	case '{':
		if blockType == "document" {
			warns.Add(ir.WarningDropped, "document.citations", "Anthropic document citation configuration is not represented")
		} else {
			warns.Add(ir.WarningDropped, "citations", "citation configuration has no text-block equivalent")
		}
		return nil, warns
	default:
		warns.Add(ir.WarningDropped, "citations", "citations member has an unsupported shape")
		return nil, warns
	}
}

func decodeAnthropicBlocks(parts []anthropicWireBlock, warns *ir.Warnings, ids *requestToolIDs) ([]ir.Block, error) {
	var blocks []ir.Block
	for i, p := range parts {
		switch p.Type {
		case "text":
			citations, w := decodeAnthropicCitations(p.Citations, "text")
			*warns = append(*warns, w...)
			blocks = append(blocks, ir.Block{
				Type: ir.BlockText, Text: p.Text, CacheControl: p.CacheControl, Citations: citations,
			})
		case "image":
			if wireValuePresent(p.Transformations) {
				warns.AddOnce(ir.WarningDropped, "image.transformations", "Anthropic image transformations have no cross-protocol equivalent")
			}
			img, err := decodeAnthropicImage(p.Source)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, ir.Block{Type: ir.BlockImage, Image: img, CacheControl: p.CacheControl})
		case "document":
			if _, w := decodeAnthropicCitations(p.Citations, "document"); len(w) > 0 {
				*warns = append(*warns, w...)
			}
			docBlocks, err := decodeAnthropicDocument(p, warns, ids)
			if err != nil {
				return nil, err
			}
			// The document's cache breakpoint covers everything through its
			// final decoded block.
			if p.CacheControl != nil && len(docBlocks) > 0 {
				last := &docBlocks[len(docBlocks)-1]
				if last.CacheControl == nil {
					last.CacheControl = p.CacheControl
				}
			}
			blocks = append(blocks, docBlocks...)
		case "tool_use":
			if wireValuePresent(p.Caller) {
				warns.AddOnce(ir.WarningDropped, "tool_use.caller", "server-side tool caller has no cross-protocol equivalent")
			}
			if p.ToolsetName != "" {
				warns.AddOnce(ir.WarningDropped, "tool_use.toolset_name", "server-side toolset name has no cross-protocol equivalent")
			}
			if p.Input != nil && !json.Valid(p.Input) {
				return nil, errors.New("anthropic tool_use input is not valid JSON")
			}
			args := bytes.TrimSpace(p.Input)
			if len(args) == 0 || bytes.Equal(args, []byte("null")) {
				args = json.RawMessage(`{}`)
			}
			if args[0] != '{' {
				return nil, errors.New("anthropic tool_use input must be a JSON object")
			}
			id := p.ID
			if id == "" {
				// Anthropic requires a non-empty tool_use id. Synthesize a
				// conforming one and report the approximation rather than
				// emitting an empty id on the target wire. The request-scoped
				// namespace keeps it unique across every message.
				id = ids.synthesize(syntheticToolID(i))
				warns.Add(ir.WarningApproximated, "tool_use.id", "generated a missing tool id")
			} else if err := ids.reserve(id); err != nil {
				return nil, err
			}
			blocks = append(blocks, ir.Block{
				Type: ir.BlockToolCall, CacheControl: p.CacheControl, ToolCall: &ir.ToolCall{
					ID: id, Name: p.Name, Arguments: args,
				},
			})
		case "tool_result":
			if p.ToolsetName != "" {
				warns.AddOnce(ir.WarningDropped, "tool_result.toolset_name", "server-side toolset name has no cross-protocol equivalent")
			}
			content, err := decodeAnthropicContent(p.Content, warns, ids)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, ir.Block{
				Type: ir.BlockToolResult, CacheControl: p.CacheControl, ToolResult: &ir.ToolResult{
					CallID: p.ToolUseID, Content: content, IsError: p.IsError,
				},
			})
		case "thinking":
			blocks = append(blocks, ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
				Text: p.Thinking, Signature: p.Signature, Provider: ir.Anthropic,
			}})
		case "redacted_thinking":
			blocks = append(blocks, ir.Block{Type: ir.BlockRedactedReasoning, Reasoning: &ir.Reasoning{
				Encrypted: p.Data, Provider: ir.Anthropic,
			}})
		case "server_tool_use", "web_search_tool_result", "web_fetch_tool_result",
			"code_execution_tool_result", "bash_code_execution_tool_result",
			"text_editor_code_execution_tool_result", "tool_search_tool_result",
			"container_upload", "search_result":
			warns.Add(ir.WarningDropped, "content."+p.Type, "provider-executed block has no cross-protocol equivalent")
		default:
			warns.Add(ir.WarningDropped, "content."+p.Type, "unsupported content block")
		}
	}
	return blocks, nil
}

func decodeAnthropicImage(src *struct {
	Type      string          `json:"type"`
	MediaType string          `json:"media_type"`
	Data      string          `json:"data"`
	URL       string          `json:"url"`
	FileID    string          `json:"file_id"`
	Content   json.RawMessage `json:"content"`
}) (*ir.Image, error) {
	if src == nil {
		return nil, errors.New("anthropic image block has no source")
	}
	switch src.Type {
	case "base64":
		return &ir.Image{Data: src.Data, MediaType: src.MediaType}, nil
	case "url":
		img := &ir.Image{URL: src.URL}
		normalizeImageURL(img)
		return img, nil
	case "file":
		return &ir.Image{FileID: src.FileID}, nil
	default:
		return nil, fmt.Errorf("unsupported anthropic image source %q", src.Type)
	}
}

// decodeAnthropicDocument returns the canonical blocks a document contributes
// to its message. A content-source document carries nested text and images;
// keeping them as blocks preserves the images and avoids manufacturing an
// empty file block. The document boundary (title/context/citations) has no
// cross-protocol equivalent and is reported.
func decodeAnthropicDocument(p anthropicWireBlock, warns *ir.Warnings, ids *requestToolIDs) ([]ir.Block, error) {
	if p.Source == nil {
		return nil, errors.New("anthropic document block has no source")
	}
	if len(bytes.TrimSpace(p.Context)) > 0 && !bytes.Equal(bytes.TrimSpace(p.Context), []byte("null")) {
		warns.Add(ir.WarningDropped, "document.context", "document context has no cross-protocol equivalent")
	}
	switch p.Source.Type {
	case "base64":
		file := &ir.File{FileData: p.Source.Data, MediaType: p.Source.MediaType, Filename: p.Title}
		return []ir.Block{{Type: ir.BlockFile, File: file}}, nil
	case "text":
		// A plain-text document's data is the text itself, not base64. The
		// title has no text-block field on any target, so a non-empty one is a
		// recorded loss instead of a silent drop.
		if p.Title != "" {
			warns.Add(ir.WarningDropped, "document.title", "plain-text document title has no cross-protocol equivalent")
		}
		return []ir.Block{ir.TextBlock(string(p.Source.Data))}, nil
	case "url":
		file := &ir.File{FileURL: p.Source.URL, Filename: p.Title}
		return []ir.Block{{Type: ir.BlockFile, File: file}}, nil
	case "file":
		file := &ir.File{FileID: p.Source.FileID, Filename: p.Title}
		return []ir.Block{{Type: ir.BlockFile, File: file}}, nil
	case "content":
		blocks, err := decodeAnthropicContent(p.Source.Content, warns, ids)
		if err != nil {
			return nil, err
		}
		warns.Add(ir.WarningApproximated, "document.content",
			"nested document content was inlined and its title, context and citations are not preserved")
		return blocks, nil
	default:
		return nil, fmt.Errorf("unsupported anthropic document source %q", p.Source.Type)
	}
}

func decodeAnthropicTools(in []anthropicWireTool) ([]ir.Tool, ir.Warnings, error) {
	var (
		out   []ir.Tool
		warns ir.Warnings
	)
	dropped := map[string]bool{}
	for i, t := range in {
		if t.Type != "" && t.Type != "custom" {
			return nil, nil, fmt.Errorf("anthropic tool %d: provider-defined type %q has no cross-protocol equivalent", i, t.Type)
		}
		allowedCallers, err := toolAllowedCallers(t.AllowedCallers)
		if err != nil {
			return nil, nil, fmt.Errorf("anthropic tool %d: %w", i, err)
		}
		for _, field := range []struct {
			name    string
			present bool
		}{
			{"tool.allowed_callers", allowedCallers},
			{"tool.defer_loading", t.DeferLoading != nil},
			{"tool.input_examples", len(bytes.TrimSpace(t.InputExamples)) > 0 && !bytes.Equal(bytes.TrimSpace(t.InputExamples), []byte("null"))},
			{"tool.eager_input_streaming", t.EagerInputStreaming != nil},
		} {
			if field.present && !dropped[field.name] {
				dropped[field.name] = true
				warns.Add(ir.WarningDropped, field.name, "provider-only tool knob has no cross-protocol equivalent")
			}
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, nil, fmt.Errorf("anthropic tool %d: name is required", i)
		}
		params := t.InputSchema
		if len(params) == 0 || bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
			params = json.RawMessage(`{}`)
		}
		out = append(out, ir.Tool{
			Type: "function", Name: t.Name, Description: t.Description,
			Parameters: params, Strict: t.Strict, CacheControl: t.CacheControl,
		})
	}
	return out, warns, nil
}

func decodeAnthropicToolChoice(raw json.RawMessage) (*ir.ToolChoice, *bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var obj struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse *bool  `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("anthropic tool_choice: %w", err)
	}
	var parallel *bool
	if obj.DisableParallelToolUse != nil {
		v := !*obj.DisableParallelToolUse
		parallel = &v
	}
	switch obj.Type {
	case "auto":
		return &ir.ToolChoice{Mode: ir.ToolChoiceAuto}, parallel, nil
	case "none":
		return &ir.ToolChoice{Mode: ir.ToolChoiceNone}, parallel, nil
	case "any":
		return &ir.ToolChoice{Mode: ir.ToolChoiceRequired}, parallel, nil
	case "tool":
		return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Name}, parallel, nil
	default:
		return nil, nil, fmt.Errorf("unsupported anthropic tool_choice type %q", obj.Type)
	}
}

func decodeAnthropicThinking(raw json.RawMessage, maxTokens *int) (*ir.ReasoningConfig, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var obj struct {
		Type         string          `json:"type"`
		BudgetTokens json.RawMessage `json:"budget_tokens"`
		Display      string          `json:"display"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("anthropic thinking: %w", err)
	}
	var warns ir.Warnings
	switch obj.Type {
	case "enabled":
		enabled := true
		budget, err := jsonnum.OptionalInt(obj.BudgetTokens, "thinking.budget_tokens")
		if err != nil {
			return nil, nil, fmt.Errorf("anthropic thinking: %w", err)
		}
		// Anthropic requires a budget for enabled thinking and documents a
		// 1024 floor plus a strict upper bound below max_tokens. Zero, a
		// negative or an absent budget would otherwise be mapped to a
		// reasoning effort the target cannot honor.
		if budget == nil {
			return nil, nil, errors.New("anthropic thinking: budget_tokens is required when type is enabled")
		}
		if *budget < 1024 {
			return nil, nil, fmt.Errorf("anthropic thinking: budget_tokens must be at least 1024")
		}
		if maxTokens != nil && *budget >= *maxTokens {
			return nil, nil, fmt.Errorf("anthropic thinking: budget_tokens must be less than max_tokens")
		}
		return &ir.ReasoningConfig{Enabled: &enabled, BudgetTokens: budget, Display: obj.Display}, nil, nil
	case "adaptive":
		enabled := true
		warns.Add(ir.WarningApproximated, "thinking.type", "adaptive thinking has no cross-protocol equivalent")
		return &ir.ReasoningConfig{Enabled: &enabled, Display: obj.Display}, warns, nil
	case "disabled":
		enabled := false
		return &ir.ReasoningConfig{Enabled: &enabled}, nil, nil
	case "between_tools":
		enabled := true
		warns.Add(ir.WarningApproximated, "thinking.type", "between_tools has no cross-protocol equivalent")
		return &ir.ReasoningConfig{Enabled: &enabled}, warns, nil
	default:
		return nil, nil, fmt.Errorf("unsupported anthropic thinking type %q", obj.Type)
	}
}

func decodeAnthropicOutputConfig(raw json.RawMessage) (*ir.ResponseFormat, string, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, "", nil, nil
	}
	var obj struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "", nil, fmt.Errorf("anthropic output_config: %w", err)
	}
	var rf *ir.ResponseFormat
	if obj.Format != nil {
		switch obj.Format.Type {
		case "json_schema":
			rf = &ir.ResponseFormat{Type: "json_schema", Schema: obj.Format.Schema}
		case "text", "":
			rf = &ir.ResponseFormat{Type: "text"}
		default:
			return nil, "", nil, fmt.Errorf("unsupported anthropic output_config.format %q", obj.Format.Type)
		}
	}
	return rf, obj.Effort, nil, nil
}
