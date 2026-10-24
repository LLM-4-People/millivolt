package format

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Anthropic Messages wire codec. The decoder accepts the request and response
// shapes documented for /v1/messages; the encoder emits the same shapes. The
// IR keeps Anthropic thinking signatures and redacted-thinking payloads opaque
// and tagged with their provider, so an Anthropic-to-Anthropic round trip is
// byte-faithful and a cross-family encode warns instead of silently dropping
// them.

type anthropicWireRequest struct {
	Model         string              `json:"model"`
	Messages      []anthropicWireMsg  `json:"messages"`
	System        json.RawMessage     `json:"system"`
	MaxTokens     *int                `json:"max_tokens"`
	Stream        bool                `json:"stream"`
	Temperature   *float64            `json:"temperature"`
	TopP          *float64            `json:"top_p"`
	TopK          *int                `json:"top_k"`
	StopSequences []string            `json:"stop_sequences"`
	Tools         []anthropicWireTool `json:"tools"`
	ToolChoice    json.RawMessage     `json:"tool_choice"`
	Thinking      json.RawMessage     `json:"thinking"`
	CacheControl  *ir.CacheControl    `json:"cache_control"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	ServiceTier  string          `json:"service_tier"`
	Speed        string          `json:"speed"`
	OutputConfig json.RawMessage `json:"output_config"`
	// Stateful and data-residency controls the canonical request cannot carry.
	// Present values are reported rather than silently dropped; diagnostics
	// also covers its nested previous_message_id.
	Container    json.RawMessage `json:"container"`
	Diagnostics  json.RawMessage `json:"diagnostics"`
	InferenceGeo string          `json:"inference_geo"`
}

type anthropicWireMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicWireTool struct {
	Type         string           `json:"type"`
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	InputSchema  json.RawMessage  `json:"input_schema"`
	Strict       *bool            `json:"strict"`
	CacheControl *ir.CacheControl `json:"cache_control"`

	// Provider-only tool knobs. They have no cross-protocol equivalent and
	// are reported rather than silently dropped.
	AllowedCallers      json.RawMessage `json:"allowed_callers"`
	DeferLoading        *bool           `json:"defer_loading"`
	InputExamples       json.RawMessage `json:"input_examples"`
	EagerInputStreaming *bool           `json:"eager_input_streaming"`
}

type anthropicWireBlock struct {
	Type string `json:"type"`

	Text string `json:"text"`
	// Citations is raw because the member is block-type-aware: text, system
	// and tool-result text blocks carry a citation array, while a document
	// block carries the CitationsConfigParam object ({"enabled":true}).
	Citations    json.RawMessage  `json:"citations"`
	CacheControl *ir.CacheControl `json:"cache_control"`

	Source *struct {
		Type      string          `json:"type"`
		MediaType string          `json:"media_type"`
		Data      string          `json:"data"`
		URL       string          `json:"url"`
		FileID    string          `json:"file_id"`
		Content   json.RawMessage `json:"content"`
	} `json:"source"`
	Title string `json:"title"`
	// Context is a document-only hint with no cross-protocol equivalent.
	Context json.RawMessage `json:"context"`

	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`

	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

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
	if in.MaxTokens == nil {
		return nil, errors.New("anthropic request: max_tokens is required")
	}
	req := &ir.Request{
		Model:           in.Model,
		MaxOutputTokens: in.MaxTokens,
		Temperature:     in.Temperature,
		TopP:            in.TopP,
		TopK:            in.TopK,
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
	for i, m := range in.Messages {
		msg, warns, err := decodeAnthropicMessage(m)
		if err != nil {
			return nil, fmt.Errorf("anthropic message %d: %w", i, err)
		}
		req.Messages = append(req.Messages, msg)
		req.Warnings = append(req.Warnings, warns...)
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
	if r, warns, err := decodeAnthropicThinking(in.Thinking); err != nil {
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

func decodeAnthropicMessage(m anthropicWireMsg) (ir.Message, ir.Warnings, error) {
	role := ir.Role(m.Role)
	switch role {
	case ir.RoleUser, ir.RoleAssistant, ir.RoleSystem:
	default:
		return ir.Message{}, nil, fmt.Errorf("unsupported role %q", m.Role)
	}
	blocks, warns, err := decodeAnthropicContent(m.Content)
	if err != nil {
		return ir.Message{}, nil, err
	}
	return ir.Message{Role: role, Content: blocks}, warns, nil
}

func decodeAnthropicContent(raw json.RawMessage) ([]ir.Block, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil, nil
		}
		return []ir.Block{ir.TextBlock(s)}, nil, nil
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return nil, nil, fmt.Errorf("anthropic content block array exceeds %d blocks", metrics.ContentPartMax)
	}
	if err := checkAnthropicCitations(raw, "content"); err != nil {
		return nil, nil, err
	}
	var parts []anthropicWireBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, err
	}
	return decodeAnthropicBlocks(parts)
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

func decodeAnthropicBlocks(parts []anthropicWireBlock) ([]ir.Block, ir.Warnings, error) {
	var (
		blocks []ir.Block
		warns  ir.Warnings
	)
	for i, p := range parts {
		switch p.Type {
		case "text":
			citations, w := decodeAnthropicCitations(p.Citations, "text")
			warns = append(warns, w...)
			blocks = append(blocks, ir.Block{
				Type: ir.BlockText, Text: p.Text, CacheControl: p.CacheControl, Citations: citations,
			})
		case "image":
			img, err := decodeAnthropicImage(p.Source)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, ir.Block{Type: ir.BlockImage, Image: img, CacheControl: p.CacheControl})
		case "document":
			if _, w := decodeAnthropicCitations(p.Citations, "document"); len(w) > 0 {
				warns = append(warns, w...)
			}
			docBlocks, w, err := decodeAnthropicDocument(p)
			if err != nil {
				return nil, nil, err
			}
			warns = append(warns, w...)
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
			if p.Input != nil && !json.Valid(p.Input) {
				return nil, nil, errors.New("anthropic tool_use input is not valid JSON")
			}
			args := bytes.TrimSpace(p.Input)
			if len(args) == 0 || bytes.Equal(args, []byte("null")) {
				args = json.RawMessage(`{}`)
			}
			if args[0] != '{' {
				return nil, nil, errors.New("anthropic tool_use input must be a JSON object")
			}
			id := p.ID
			if id == "" {
				// Anthropic requires a non-empty tool_use id. Synthesize a
				// conforming one and report the approximation rather than
				// emitting an empty id on the target wire.
				id = syntheticToolID(i)
				warns.Add(ir.WarningApproximated, "tool_use.id", "generated a missing tool id")
			}
			blocks = append(blocks, ir.Block{
				Type: ir.BlockToolCall, CacheControl: p.CacheControl, ToolCall: &ir.ToolCall{
					ID: id, Name: p.Name, Arguments: args,
				},
			})
		case "tool_result":
			content, w, err := decodeAnthropicContent(p.Content)
			if err != nil {
				return nil, nil, err
			}
			warns = append(warns, w...)
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
	return blocks, warns, nil
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
func decodeAnthropicDocument(p anthropicWireBlock) ([]ir.Block, ir.Warnings, error) {
	if p.Source == nil {
		return nil, nil, errors.New("anthropic document block has no source")
	}
	var contextWarns ir.Warnings
	if len(bytes.TrimSpace(p.Context)) > 0 && !bytes.Equal(bytes.TrimSpace(p.Context), []byte("null")) {
		contextWarns.Add(ir.WarningDropped, "document.context", "document context has no cross-protocol equivalent")
	}
	switch p.Source.Type {
	case "base64":
		file := &ir.File{FileData: p.Source.Data, MediaType: p.Source.MediaType, Filename: p.Title}
		return []ir.Block{{Type: ir.BlockFile, File: file}}, contextWarns, nil
	case "text":
		// A plain-text document's data is the text itself, not base64. The
		// title has no text-block field on any target, so a non-empty one is a
		// recorded loss instead of a silent drop.
		warns := contextWarns
		if p.Title != "" {
			warns.Add(ir.WarningDropped, "document.title", "plain-text document title has no cross-protocol equivalent")
		}
		return []ir.Block{ir.TextBlock(string(p.Source.Data))}, warns, nil
	case "url":
		file := &ir.File{FileURL: p.Source.URL, Filename: p.Title}
		return []ir.Block{{Type: ir.BlockFile, File: file}}, contextWarns, nil
	case "file":
		file := &ir.File{FileID: p.Source.FileID, Filename: p.Title}
		return []ir.Block{{Type: ir.BlockFile, File: file}}, contextWarns, nil
	case "content":
		blocks, warns, err := decodeAnthropicContent(p.Source.Content)
		if err != nil {
			return nil, nil, err
		}
		warns = append(contextWarns, warns...)
		warns.Add(ir.WarningApproximated, "document.content",
			"nested document content was inlined and its title, context and citations are not preserved")
		return blocks, warns, nil
	default:
		return nil, nil, fmt.Errorf("unsupported anthropic document source %q", p.Source.Type)
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

func decodeAnthropicThinking(raw json.RawMessage) (*ir.ReasoningConfig, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var obj struct {
		Type         string `json:"type"`
		BudgetTokens *int   `json:"budget_tokens"`
		Display      string `json:"display"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("anthropic thinking: %w", err)
	}
	var warns ir.Warnings
	switch obj.Type {
	case "enabled":
		enabled := true
		return &ir.ReasoningConfig{Enabled: &enabled, BudgetTokens: obj.BudgetTokens, Display: obj.Display}, nil, nil
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
	messages := encodeAnthropicMessages(req.Messages, &req.Warnings)
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
	if system := encodeAnthropicSystem(req.Messages, req.Instructions, &req.Warnings); system != nil {
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

// encodeAnthropicSystem lifts the Responses instructions and leading
// system/developer messages into the top-level system field. A system message
// after the conversation has started is representable as SystemMessageParam
// and stays in messages at its original position. A later developer message
// has no Anthropic role and is lifted with an approximation warning.
func encodeAnthropicSystem(messages []ir.Message, instructions *string, warns *ir.Warnings) any {
	var blocks []map[string]any
	if instructions != nil && *instructions != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": *instructions})
	}
	seenNonSystem := false
	moved := false
	for _, m := range messages {
		if m.Role != ir.RoleSystem && m.Role != ir.RoleDeveloper {
			seenNonSystem = true
			continue
		}
		if m.Role == ir.RoleSystem && seenNonSystem {
			continue
		}
		if m.Role == ir.RoleDeveloper && seenNonSystem && !moved {
			moved = true
			if warns != nil {
				warns.Add(ir.WarningApproximated, "system", "Anthropic Messages has no developer role; a later developer message was moved before the conversation")
			}
		}
		blocks = append(blocks, anthropicSystemTextBlocks(m.Content, warns)...)
	}
	return anthropicTextContent(blocks)
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

type anthropicOutMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func encodeAnthropicMessages(in []ir.Message, warns *ir.Warnings) []anthropicOutMessage {
	var out []anthropicOutMessage
	seenNonSystem := false
	for _, m := range in {
		if m.Name != "" && warns != nil {
			warns.Add(ir.WarningDropped, "messages.name", "Anthropic Messages has no participant name")
		}
		// Developer messages are lifted at the top level. A leading system
		// message is lifted too; a later system turn stays here as the
		// SystemMessageParam the Messages schema accepts.
		if m.Role == ir.RoleDeveloper {
			continue
		}
		if m.Role == ir.RoleSystem && !seenNonSystem {
			continue
		}
		role := string(m.Role)
		var content any
		if m.Role == ir.RoleSystem {
			content = anthropicTextContent(anthropicSystemTextBlocks(m.Content, warns))
		} else {
			seenNonSystem = true
			content = encodeAnthropicContent(m.Content, m.Role, warns)
		}
		if content == nil {
			continue
		}
		// Anthropic merges consecutive same-role turns; doing it here keeps
		// tool_result blocks immediately after their tool_use turn.
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = mergeAnthropicContent(out[n-1].Content, content)
			continue
		}
		out = append(out, anthropicOutMessage{Role: role, Content: content})
	}
	return out
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
				block["title"] = b.File.Filename
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

// anthropicUserIDMax is the official metadata.user_id maxLength. It counts
// Unicode characters, matching the JSON Schema maxLength semantics.
const anthropicUserIDMax = 512

// anthropicImageMediaTypes is the Base64ImageSource media-type allowlist from
// the Anthropic Messages schema.
var anthropicImageMediaTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
}

func encodeAnthropicImage(img *ir.Image, warns *ir.Warnings) map[string]any {
	if img == nil {
		return nil
	}
	var source map[string]any
	switch {
	case img.FileID != "":
		source = map[string]any{"type": "file", "file_id": img.FileID}
	case img.Data != "":
		if !anthropicImageMediaTypes[img.MediaType] {
			if warns != nil {
				warns.Add(ir.WarningDropped, "image.media_type",
					"Anthropic base64 images must be jpeg, png, gif or webp")
			}
			return nil
		}
		source = map[string]any{"type": "base64", "media_type": img.MediaType, "data": img.Data}
	case img.URL != "":
		source = map[string]any{"type": "url", "url": img.URL}
	default:
		return nil
	}
	if img.Detail != "" && warns != nil {
		warns.Add(ir.WarningDropped, "image.detail", "Anthropic images have no detail level")
	}
	return source
}

// encodeAnthropicFile maps a document to an Anthropic source that the target
// accepts: application/pdf bytes or URL, text/plain bytes, or a provider file
// id. Other text media types are approximated as plain text; a document the
// target cannot read is dropped with a warning.
func encodeAnthropicFile(f *ir.File, warns *ir.Warnings) map[string]any {
	if f == nil {
		return nil
	}
	mediaType, data := f.MediaType, f.FileData
	if media, raw, ok := splitBase64DataURL(f.FileData); ok {
		data = raw
		if mediaType == "" {
			mediaType = media
		}
	}
	var source map[string]any
	switch {
	case f.FileID != "":
		source = map[string]any{"type": "file", "file_id": f.FileID}
	case f.FileURL != "":
		if mediaType != "" && mediaType != "application/pdf" {
			if warns != nil {
				warns.Add(ir.WarningDropped, "file.media_type", "Anthropic document URLs must be PDFs")
			}
			return nil
		}
		source = map[string]any{"type": "url", "url": f.FileURL}
	case data == "":
		return nil
	case mediaType == "application/pdf":
		source = map[string]any{"type": "base64", "media_type": mediaType, "data": data}
	case strings.HasPrefix(mediaType, "text/"):
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			if warns != nil {
				warns.Add(ir.WarningDropped, "file.media_type", "inline plain-text document data is not valid base64")
			}
			return nil
		}
		if mediaType != "text/plain" && warns != nil {
			warns.Add(ir.WarningApproximated, "file.media_type", "Anthropic plain-text documents accept text/plain only")
		}
		source = map[string]any{"type": "text", "media_type": "text/plain", "data": string(decoded)}
	default:
		if warns != nil {
			warns.Add(ir.WarningDropped, "file.media_type", "Anthropic documents accept PDF or plain-text data")
		}
		return nil
	}
	if f.Detail != "" && warns != nil {
		warns.Add(ir.WarningDropped, "file.detail", "Anthropic documents have no detail level")
	}
	return source
}

func encodeAnthropicToolResultContent(blocks []ir.Block, warns *ir.Warnings) any {
	if len(blocks) == 0 {
		return ""
	}
	// The plain string form cannot carry a cache breakpoint or citations.
	// Use the array form whenever a text block has either so it survives.
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
		return text.String()
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			// TextBlockParam requires non-empty text. A mixed tool result
			// keeps its image/file parts and drops the empty text block with
			// its cache breakpoint or citations.
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
			if source := encodeAnthropicImage(b.Image, warns); source != nil {
				out = append(out, map[string]any{"type": "image", "source": source})
			}
		case ir.BlockFile:
			if source := encodeAnthropicFile(b.File, warns); source != nil {
				out = append(out, map[string]any{"type": "document", "source": source})
			}
		case ir.BlockAudio:
			if warns != nil {
				warns.Add(ir.WarningDropped, "tool_result.audio", "Anthropic tool results have no audio part")
			}
		default:
			if warns != nil {
				warns.Add(ir.WarningDropped, "tool_result."+string(b.Type), "block has no Anthropic tool-result equivalent")
			}
		}
	}
	if len(out) == 0 {
		// Every block was dropped, so no text block remains to carry a
		// breakpoint. The empty string is the valid empty tool_result.
		return ""
	}
	return out
}

func encodeAnthropicTools(in []ir.Tool) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, t := range in {
		tool := map[string]any{"name": t.Name}
		if t.Description != "" {
			tool["description"] = t.Description
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		tool["input_schema"] = params
		if t.Strict != nil {
			tool["strict"] = *t.Strict
		}
		if t.CacheControl != nil {
			tool["cache_control"] = t.CacheControl
		}
		out = append(out, tool)
	}
	return out
}

func encodeAnthropicToolChoice(tc *ir.ToolChoice, parallel *bool) map[string]any {
	if tc == nil && parallel == nil {
		return nil
	}
	mode := ir.ToolChoiceAuto
	if tc != nil {
		mode = tc.Mode
	}
	out := map[string]any{}
	switch mode {
	case ir.ToolChoiceNone:
		out["type"] = "none"
	case ir.ToolChoiceRequired:
		out["type"] = "any"
	case ir.ToolChoiceTool:
		out["type"] = "tool"
		out["name"] = tc.Name
	default:
		out["type"] = "auto"
	}
	// ToolChoiceNone carries only its type in the Anthropic schema; the
	// parallel-use modifier is declared on auto/any/tool only.
	if parallel != nil && mode != ir.ToolChoiceNone {
		out["disable_parallel_tool_use"] = !*parallel
	}
	return out
}

func encodeAnthropicThinking(r *ir.ReasoningConfig) map[string]any {
	if r == nil {
		return nil
	}
	if r.Enabled != nil && !*r.Enabled {
		return map[string]any{"type": "disabled"}
	}
	if r.BudgetTokens != nil {
		out := map[string]any{"type": "enabled", "budget_tokens": *r.BudgetTokens}
		if r.Display != "" {
			out["display"] = r.Display
		}
		return out
	}
	if r.Enabled != nil && *r.Enabled {
		out := map[string]any{"type": "adaptive"}
		if r.Display != "" {
			out["display"] = r.Display
		}
		return out
	}
	return nil
}

// anthropicEfforts is Anthropic's documented output_config.effort vocabulary.
var anthropicEfforts = map[string]bool{
	"low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

func encodeAnthropicOutputConfig(rf *ir.ResponseFormat, reasoning *ir.ReasoningConfig, warns *ir.Warnings) map[string]any {
	out := map[string]any{}
	if reasoning != nil {
		effort := reasoning.Effort
		if reasoning.Enabled != nil && !*reasoning.Enabled {
			effort = ""
			if warns != nil {
				warns.Add(ir.WarningDropped, "reasoning.effort", "disabled thinking has no Anthropic output_config spelling")
			}
		} else if effort == "minimal" {
			// Anthropic's documented effort levels start at low; minimal has
			// no exact spelling there, so the nearest level preserves the
			// client's low-reasoning intent instead of failing the request.
			effort = "low"
			if warns != nil {
				warns.Add(ir.WarningApproximated, "reasoning.effort", "Anthropic Messages has no minimal effort; mapped to low")
			}
		}
		if effort != "" {
			switch {
			case anthropicEfforts[effort]:
				out["effort"] = effort
			case effort == "none":
				// Anthropic represents no extended thinking by omitting the
				// effort field, not by a "none" value.
				if warns != nil {
					warns.Add(ir.WarningApproximated, "reasoning.effort", "no effort field is emitted for none")
				}
			default:
				if warns != nil {
					warns.Add(ir.WarningDropped, "reasoning.effort", "Anthropic Messages has no equivalent effort")
				}
			}
		}
		if reasoning.Summary != "" && warns != nil {
			warns.Add(ir.WarningDropped, "reasoning.summary", "Anthropic Messages has no reasoning summary")
		}
		if reasoning.Context != "" && warns != nil {
			warns.Add(ir.WarningDropped, "reasoning.context", "Anthropic Messages has no reasoning context")
		}
		if reasoning.Mode != "" && warns != nil {
			warns.Add(ir.WarningDropped, "reasoning.mode", "Anthropic Messages has no reasoning mode")
		}
	}
	if rf != nil && rf.Type == "json_schema" && len(rf.Schema) > 0 {
		out["format"] = map[string]any{"type": "json_schema", "schema": rf.Schema}
		if rf.Name != "" && warns != nil {
			warns.Add(ir.WarningDropped, "response_format.name", "Anthropic output_config.format has no name")
		}
		if rf.Description != "" && warns != nil {
			warns.Add(ir.WarningDropped, "response_format.description", "Anthropic output_config.format has no description")
		}
		// Anthropic's JSONOutputFormat has no strict mode. Strict false is
		// the target's default and therefore not a loss; strict true is.
		if rf.Strict != nil && *rf.Strict && warns != nil {
			warns.Add(ir.WarningDropped, "response_format.strict", "Anthropic output_config.format has no strict mode")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicWireResponse is the non-streaming Anthropic response shape.
type anthropicWireResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []anthropicWireBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence string               `json:"stop_sequence"`
	StopDetails  json.RawMessage      `json:"stop_details"`
	Usage        anthropicWireUsage   `json:"usage"`
}

type anthropicWireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	// ServiceTier and Speed describe the serving tier of this usage. They
	// exist only on the response side; the request spells the tier through
	// service_tier/speed as well.
	ServiceTier         string `json:"service_tier"`
	Speed               string `json:"speed"`
	OutputTokensDetails *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

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
	blocks, warns, err := decodeAnthropicBlocks(in.Content)
	if err != nil {
		return nil, err
	}
	resp := &ir.Response{
		ID:           in.ID,
		Model:        in.Model,
		Created:      time.Now().Unix(),
		Blocks:       blocks,
		Finish:       anthropicFinishReason(in.StopReason),
		StopSequence: in.StopSequence,
		Warnings:     warns,
	}
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
	return resp, nil
}

func anthropicUsageToIR(u anthropicWireUsage) (ir.Usage, error) {
	out := ir.Usage{
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	input, err := metrics.SumCounts(u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
	if err != nil {
		return ir.Usage{}, fmt.Errorf("usage: %w", err)
	}
	total, err := metrics.SumCounts(input, u.OutputTokens)
	if err != nil {
		return ir.Usage{}, fmt.Errorf("usage: %w", err)
	}
	out.InputTokens = input
	out.TotalTokens = total
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
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
	blocks := encodeAnthropicBlocks(resp.Blocks, ir.RoleAssistant, &resp.Warnings)
	usage := map[string]any{
		"input_tokens":                nonNegative(resp.Usage.InputTokens - resp.Usage.CacheReadTokens - resp.Usage.CacheWriteTokens),
		"output_tokens":               resp.Usage.OutputTokens,
		"cache_creation_input_tokens": resp.Usage.CacheWriteTokens,
		"cache_read_input_tokens":     resp.Usage.CacheReadTokens,
	}
	if resp.Usage.ReasoningTokens != 0 {
		usage["output_tokens_details"] = map[string]any{"thinking_tokens": resp.Usage.ReasoningTokens}
	}
	serviceTier, speed, tierWarns := anthropicResponseUsageTier(resp.ServiceTier)
	resp.Warnings = append(resp.Warnings, tierWarns...)
	if serviceTier != "" {
		usage["service_tier"] = serviceTier
	}
	if speed != "" {
		usage["speed"] = speed
	}
	out := map[string]any{
		"id":            resp.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       blocks,
		"stop_reason":   anthropicStopReason(resp.Finish),
		"stop_sequence": nil,
		"usage":         usage,
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

// DecodeAnthropicError decodes the Anthropic error envelope carried by a
// non-2xx body or an SSE error event.
func DecodeAnthropicError(body []byte) (*ir.Error, error) {
	var in struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	if in.Error.Message == "" && in.Error.Type == "" {
		return nil, errors.New("not an anthropic error envelope")
	}
	return &ir.Error{Type: in.Error.Type, Message: in.Error.Message}, nil
}

// EncodeAnthropicError renders the canonical error as an Anthropic error
// envelope.
func EncodeAnthropicError(e *ir.Error) ([]byte, error) {
	typ := anthropicErrorType(e.Type)
	return sse.MarshalWireJSON(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": e.Message,
		},
	})
}

// anthropicErrorType maps the shared OpenAI error vocabulary onto Anthropic's
// documented type set. Shared names pass through; an unknown or provider-only
// class becomes api_error, Anthropic's catch-all, rather than leaking a type
// its clients do not recognize.
func anthropicErrorType(typ string) string {
	switch typ {
	case "":
		return "api_error"
	case "server_error", "upstream_error", "content_policy_violation":
		return "api_error"
	case "insufficient_quota", "insufficient_credits":
		return "billing_error"
	case "context_length_exceeded":
		return "invalid_request_error"
	}
	switch typ {
	case "invalid_request_error", "authentication_error", "permission_error",
		"not_found_error", "request_too_large", "rate_limit_error",
		"api_error", "overloaded_error", "timeout_error", "billing_error":
		return typ
	}
	return "api_error"
}
