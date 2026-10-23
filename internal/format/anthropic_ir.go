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
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	ServiceTier  string          `json:"service_tier"`
	OutputConfig json.RawMessage `json:"output_config"`
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
}

type anthropicWireBlock struct {
	Type string `json:"type"`

	Text         string           `json:"text"`
	Citations    []ir.Citation    `json:"citations"`
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
		blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: p.Text, CacheControl: p.CacheControl})
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
	var parts []anthropicWireBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, err
	}
	return decodeAnthropicBlocks(parts)
}

func decodeAnthropicBlocks(parts []anthropicWireBlock) ([]ir.Block, ir.Warnings, error) {
	var (
		blocks []ir.Block
		warns  ir.Warnings
	)
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, ir.Block{
				Type: ir.BlockText, Text: p.Text, CacheControl: p.CacheControl, Citations: p.Citations,
			})
		case "image":
			img, err := decodeAnthropicImage(p.Source)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, ir.Block{Type: ir.BlockImage, Image: img, CacheControl: p.CacheControl})
		case "document":
			file, text, err := decodeAnthropicDocument(p)
			if err != nil {
				return nil, nil, err
			}
			if text != "" {
				blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: text, CacheControl: p.CacheControl})
			} else {
				blocks = append(blocks, ir.Block{Type: ir.BlockFile, File: file, CacheControl: p.CacheControl})
			}
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
			blocks = append(blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
				ID: p.ID, Name: p.Name, Arguments: args,
			}})
		case "tool_result":
			content, w, err := decodeAnthropicContent(p.Content)
			if err != nil {
				return nil, nil, err
			}
			warns = append(warns, w...)
			blocks = append(blocks, ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				CallID: p.ToolUseID, Content: content, IsError: p.IsError,
			}})
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
		return &ir.Image{URL: src.URL}, nil
	case "file":
		return &ir.Image{FileID: src.FileID}, nil
	default:
		return nil, fmt.Errorf("unsupported anthropic image source %q", src.Type)
	}
}

func decodeAnthropicDocument(p anthropicWireBlock) (*ir.File, string, error) {
	if p.Source == nil {
		return nil, "", errors.New("anthropic document block has no source")
	}
	switch p.Source.Type {
	case "base64":
		return &ir.File{FileData: p.Source.Data, MediaType: p.Source.MediaType, Filename: p.Title}, "", nil
	case "text":
		// A plain-text document's data is the text itself, not base64.
		return nil, string(p.Source.Data), nil
	case "url":
		return &ir.File{FileURL: p.Source.URL, Filename: p.Title}, "", nil
	case "file":
		return &ir.File{FileID: p.Source.FileID, Filename: p.Title}, "", nil
	case "content":
		blocks, _, err := decodeAnthropicContent(p.Source.Content)
		if err != nil {
			return nil, "", err
		}
		var b strings.Builder
		for _, block := range blocks {
			if block.Type == ir.BlockText {
				b.WriteString(block.Text)
			}
		}
		return nil, b.String(), nil
	default:
		return nil, "", fmt.Errorf("unsupported anthropic document source %q", p.Source.Type)
	}
}

func decodeAnthropicTools(in []anthropicWireTool) ([]ir.Tool, ir.Warnings, error) {
	var (
		out   []ir.Tool
		warns ir.Warnings
	)
	for i, t := range in {
		if t.Type != "" && t.Type != "custom" {
			return nil, nil, fmt.Errorf("anthropic tool %d: provider-defined type %q has no cross-protocol equivalent", i, t.Type)
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
		return &ir.ReasoningConfig{Enabled: &enabled, Display: obj.Display}, nil, nil
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
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		req.Warnings.Add(ir.WarningApproximated, "response_format.json_object", "Anthropic only supports json_schema")
	}
	out := map[string]any{
		"model":    req.Model,
		"messages": encodeAnthropicMessages(req.Messages, &req.Warnings),
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
	if system := encodeAnthropicSystem(req.Messages, req.Instructions); system != nil {
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
	if oc := encodeAnthropicOutputConfig(req.ResponseFormat, req.Reasoning); oc != nil {
		out["output_config"] = oc
	}
	if req.User != "" {
		out["metadata"] = map[string]any{"user_id": req.User}
	}
	if req.ServiceTier != "" {
		out["service_tier"] = req.ServiceTier
	}
	if promptCacheKey != "" {
		out["prompt_cache_key"] = promptCacheKey
	}
	return sse.MarshalWireJSON(out)
}

// encodeAnthropicSystem lifts the Responses instructions and leading
// system/developer messages into the top-level system field. Mid-conversation
// system messages stay in messages.
func encodeAnthropicSystem(messages []ir.Message, instructions *string) any {
	var blocks []map[string]any
	if instructions != nil && *instructions != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": *instructions})
	}
	for _, m := range messages {
		if m.Role != ir.RoleSystem && m.Role != ir.RoleDeveloper {
			break
		}
		for _, b := range m.Content {
			if b.Type != ir.BlockText {
				continue
			}
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	// Preserve the historical string join when no block carries a cache
	// breakpoint; a breakpoint requires the block-array form.
	for _, block := range blocks {
		if _, ok := block["cache_control"]; ok {
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
	// Drop the leading system/developer run: it is rendered at the top level.
	start := 0
	for start < len(in) && (in[start].Role == ir.RoleSystem || in[start].Role == ir.RoleDeveloper) {
		start++
	}
	var out []anthropicOutMessage
	for _, m := range in[start:] {
		role := string(m.Role)
		if m.Role == ir.RoleDeveloper {
			role = "system"
		}
		content := encodeAnthropicContent(m.Content, warns)
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
func encodeAnthropicContent(blocks []ir.Block, warns *ir.Warnings) any {
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
		return text.String()
	}
	return encodeAnthropicBlocks(blocks, ir.RoleUser, warns)
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
			block := map[string]any{"type": "text", "text": b.Text}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			if len(b.Citations) > 0 {
				block["citations"] = b.Citations
			}
			out = append(out, block)
		case ir.BlockImage:
			block := map[string]any{"type": "image", "source": encodeAnthropicImage(b.Image)}
			if b.CacheControl != nil {
				block["cache_control"] = b.CacheControl
			}
			out = append(out, block)
		case ir.BlockFile:
			block := map[string]any{"type": "document", "source": encodeAnthropicFile(b.File)}
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
			content := encodeAnthropicToolResultContent(b.ToolResult.Content)
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
			if b.Reasoning.Signature != "" || b.Reasoning.Provider == ir.Anthropic {
				out = append(out, map[string]any{
					"type": "thinking", "thinking": b.Reasoning.Text, "signature": b.Reasoning.Signature,
				})
			} else if b.Reasoning.Text != "" {
				// A reasoning block minted by another family has no Anthropic
				// signature; emitting it as text preserves the visible content.
				if warns != nil {
					warns.Add(ir.WarningApproximated, "reasoning", "cross-protocol reasoning was emitted as visible text")
				}
				out = append(out, map[string]any{"type": "text", "text": b.Reasoning.Text})
			} else if warns != nil && b.Reasoning.Encrypted != "" {
				warns.Add(ir.WarningOpaque, "reasoning", "provider-specific reasoning artifact cannot be replayed on Anthropic Messages")
			}
		case ir.BlockRedactedReasoning:
			if b.Reasoning.Provider == ir.Anthropic && b.Reasoning.Encrypted != "" {
				out = append(out, map[string]any{"type": "redacted_thinking", "data": b.Reasoning.Encrypted})
			} else if warns != nil {
				warns.Add(ir.WarningOpaque, "reasoning", "redacted reasoning cannot be replayed on Anthropic Messages")
			}
		case ir.BlockRefusal:
			out = append(out, map[string]any{"type": "text", "text": b.Refusal})
		}
	}
	_ = role
	return out
}

func encodeAnthropicImage(img *ir.Image) map[string]any {
	switch {
	case img.FileID != "":
		return map[string]any{"type": "file", "file_id": img.FileID}
	case img.Data != "":
		return map[string]any{"type": "base64", "media_type": img.MediaType, "data": img.Data}
	default:
		return map[string]any{"type": "url", "url": img.URL}
	}
}

func encodeAnthropicFile(f *ir.File) map[string]any {
	switch {
	case f.FileID != "":
		return map[string]any{"type": "file", "file_id": f.FileID}
	case f.FileURL != "":
		return map[string]any{"type": "url", "url": f.FileURL}
	case strings.HasPrefix(f.MediaType, "text/"):
		if data, err := base64.StdEncoding.DecodeString(f.FileData); err == nil {
			return map[string]any{"type": "text", "media_type": f.MediaType, "data": string(data)}
		}
	}
	return map[string]any{"type": "base64", "media_type": f.MediaType, "data": f.FileData}
}

func encodeAnthropicToolResultContent(blocks []ir.Block) any {
	if len(blocks) == 0 {
		return ""
	}
	allText := true
	var text strings.Builder
	for _, b := range blocks {
		if b.Type != ir.BlockText {
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
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case ir.BlockImage:
			out = append(out, map[string]any{"type": "image", "source": encodeAnthropicImage(b.Image)})
		case ir.BlockFile:
			out = append(out, map[string]any{"type": "document", "source": encodeAnthropicFile(b.File)})
		}
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
		if len(tc.NameRaw) > 0 {
			out["name"] = tc.NameRaw
		} else {
			out["name"] = tc.Name
		}
	default:
		out["type"] = "auto"
	}
	if parallel != nil {
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

func encodeAnthropicOutputConfig(rf *ir.ResponseFormat, reasoning *ir.ReasoningConfig) map[string]any {
	out := map[string]any{}
	if reasoning != nil && reasoning.Effort != "" {
		out["effort"] = reasoning.Effort
	}
	if rf != nil && rf.Type == "json_schema" && len(rf.Schema) > 0 {
		out["format"] = map[string]any{"type": "json_schema", "schema": rf.Schema}
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
	Usage        anthropicWireUsage   `json:"usage"`
}

type anthropicWireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokensDetails      *struct {
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
	resp.Usage = anthropicUsageToIR(in.Usage)
	if err := validateUsage(resp.Usage); err != nil {
		return nil, err
	}
	return resp, nil
}

func anthropicUsageToIR(u anthropicWireUsage) ir.Usage {
	out := ir.Usage{
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	out.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	out.TotalTokens = out.InputTokens + out.OutputTokens
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
	}
	return out
}

func anthropicFinishReason(reason string) ir.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence":
		return ir.FinishStop
	case "max_tokens", "model_context_window_exceeded":
		return ir.FinishLength
	case "tool_use":
		return ir.FinishToolCalls
	default:
		// Keep the source spelling for reasons Chat Completions can echo
		// verbatim (pause_turn, refusal) and for unknown provider values,
		// rather than collapsing them into a clean stop.
		return ir.FinishReason(reason)
	}
}

// EncodeAnthropicResponse renders the canonical response as a non-streaming
// Anthropic message.
func EncodeAnthropicResponse(resp *ir.Response) ([]byte, error) {
	if err := ir.ValidateResponse(resp); err != nil {
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
