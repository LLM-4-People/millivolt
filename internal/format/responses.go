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
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// OpenAI Responses wire codec. Responses is item-oriented: input and output
// are ordered heterogeneous item lists, not a message array. The IR keeps the
// item identity and opaque reasoning payloads so a Responses-to-Responses
// round trip is faithful; cross-protocol encoders flatten items into messages
// and warn when an item has no equivalent.

type responsesWireRequest struct {
	Model             string              `json:"model"`
	Input             json.RawMessage     `json:"input"`
	Instructions      *string             `json:"instructions"`
	MaxOutputTokens   *int                `json:"max_output_tokens"`
	Temperature       *float64            `json:"temperature"`
	TopP              *float64            `json:"top_p"`
	TopLogprobs       *int                `json:"top_logprobs"`
	Tools             []responsesWireTool `json:"tools"`
	ToolChoice        json.RawMessage     `json:"tool_choice"`
	ParallelToolCalls *bool               `json:"parallel_tool_calls"`
	Reasoning         *struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	} `json:"reasoning"`
	Text *struct {
		Format    json.RawMessage `json:"format"`
		Verbosity string          `json:"verbosity"`
	} `json:"text"`
	Stream             bool              `json:"stream"`
	Store              *bool             `json:"store"`
	PreviousResponseID string            `json:"previous_response_id"`
	Metadata           map[string]string `json:"metadata"`
	ServiceTier        string            `json:"service_tier"`
	User               string            `json:"user"`
	Include            []string          `json:"include"`
	Truncation         string            `json:"truncation"`
	Background         bool              `json:"background"`
	MaxToolCalls       *int              `json:"max_tool_calls"`
	Conversation       json.RawMessage   `json:"conversation"`
	Prompt             json.RawMessage   `json:"prompt"`
}

type responsesWireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type responsesWireItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`

	ID               string `json:"id"`
	EncryptedContent string `json:"encrypted_content"`
	Summary          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

// DecodeResponsesRequest converts one Responses request into the canonical
// request.
func DecodeResponsesRequest(body []byte) (*ir.Request, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("responses request body is not valid UTF-8")
	}
	var in responsesWireRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, errors.New("responses request: model is required")
	}
	if in.Background {
		return nil, errors.New("responses request: background execution is not supported")
	}
	if len(in.Conversation) > 0 && !bytes.Equal(bytes.TrimSpace(in.Conversation), []byte("null")) {
		return nil, errors.New("responses request: conversation is not supported")
	}
	if len(in.Prompt) > 0 && !bytes.Equal(bytes.TrimSpace(in.Prompt), []byte("null")) {
		return nil, errors.New("responses request: prompt templates are not supported")
	}
	req := &ir.Request{
		Model:              in.Model,
		Instructions:       in.Instructions,
		MaxOutputTokens:    in.MaxOutputTokens,
		Temperature:        in.Temperature,
		TopP:               in.TopP,
		TopLogprobs:        in.TopLogprobs,
		ParallelToolCalls:  in.ParallelToolCalls,
		Stream:             in.Stream,
		Store:              in.Store,
		PreviousResponseID: in.PreviousResponseID,
		Metadata:           in.Metadata,
		ServiceTier:        in.ServiceTier,
		User:               in.User,
	}
	if len(in.Include) > 0 {
		otherInclude := false
		for _, inc := range in.Include {
			if inc == "message.output_text.logprobs" {
				v := true
				req.Logprobs = &v
				continue
			}
			otherInclude = true
		}
		if otherInclude {
			req.Warnings.Add(ir.WarningDropped, "include", "include options other than message.output_text.logprobs have no cross-protocol mapping")
		}
	}
	if in.Reasoning != nil {
		req.Reasoning = &ir.ReasoningConfig{Effort: in.Reasoning.Effort, Summary: in.Reasoning.Summary}
	}
	if in.Text != nil {
		rf, warn, err := decodeResponsesFormat(in.Text.Format)
		if err != nil {
			return nil, err
		}
		req.ResponseFormat = rf
		if warn != "" {
			req.Warnings.Add(ir.WarningDropped, "text.format", warn)
		}
		if in.Text.Verbosity != "" {
			req.Warnings.Add(ir.WarningDropped, "text.verbosity", "no cross-protocol equivalent")
		}
	}
	if in.Truncation != "" && in.Truncation != "disabled" {
		req.Warnings.Add(ir.WarningDropped, "truncation", "server-side truncation is not translated")
	}
	if in.MaxToolCalls != nil {
		req.Warnings.Add(ir.WarningDropped, "max_tool_calls", "no cross-protocol equivalent")
	}
	if len(in.Tools) > 0 {
		tools, warns, err := decodeResponsesTools(in.Tools)
		if err != nil {
			return nil, err
		}
		req.Tools = tools
		req.Warnings = append(req.Warnings, warns...)
	}
	if tc, err := decodeResponsesToolChoice(in.ToolChoice); err != nil {
		return nil, err
	} else if tc != nil {
		req.ToolChoice = tc
	}
	msgs, warns, err := decodeResponsesInput(in.Input)
	if err != nil {
		return nil, err
	}
	req.Messages = msgs
	req.Warnings = append(req.Warnings, warns...)
	if err := ir.ValidateRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

func decodeResponsesInput(raw json.RawMessage) ([]ir.Message, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, errors.New("responses request: input is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil, nil
		}
		return []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock(text)}}}, nil, nil
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return nil, nil, fmt.Errorf("responses input exceeds %d items", metrics.ContentPartMax)
	}
	var items []responsesWireItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("responses input: %w", err)
	}
	var (
		out   []ir.Message
		warns ir.Warnings
	)
	appendAssistant := func(blocks ...ir.Block) {
		if n := len(out); n > 0 && out[n-1].Role == ir.RoleAssistant {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, ir.Message{Role: ir.RoleAssistant, Content: blocks})
	}
	appendToolResult := func(block ir.Block) {
		if n := len(out); n > 0 && out[n-1].Role == ir.RoleUser {
			out[n-1].Content = append(out[n-1].Content, block)
			return
		}
		out = append(out, ir.Message{Role: ir.RoleUser, Content: []ir.Block{block}})
	}
	for i, item := range items {
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			role := ir.Role(item.Role)
			switch role {
			case ir.RoleUser, ir.RoleAssistant, ir.RoleSystem, ir.RoleDeveloper:
			default:
				return nil, nil, fmt.Errorf("responses input item %d: unsupported role %q", i, item.Role)
			}
			blocks, w, err := decodeResponsesContent(item.Content, role)
			if err != nil {
				return nil, nil, fmt.Errorf("responses input item %d: %w", i, err)
			}
			warns = append(warns, w...)
			if role == ir.RoleAssistant {
				appendAssistant(blocks...)
			} else {
				out = append(out, ir.Message{Role: role, Content: blocks})
			}
		case "function_call":
			call, w, err := decodeResponsesFunctionCall(item)
			if err != nil {
				return nil, nil, fmt.Errorf("responses input item %d: %w", i, err)
			}
			warns = append(warns, w...)
			appendAssistant(ir.Block{Type: ir.BlockToolCall, ToolCall: call})
		case "function_call_output":
			content, w, err := decodeResponsesContent(item.Output, ir.RoleUser)
			if err != nil {
				return nil, nil, fmt.Errorf("responses input item %d: %w", i, err)
			}
			warns = append(warns, w...)
			if item.CallID == "" {
				return nil, nil, fmt.Errorf("responses input item %d: function_call_output has no call_id", i)
			}
			appendToolResult(ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				CallID: item.CallID, Content: content,
			}})
		case "reasoning":
			var summary []string
			for _, s := range item.Summary {
				if s.Text != "" {
					summary = append(summary, s.Text)
				}
			}
			reasoning := &ir.Reasoning{
				ID: item.ID, Summary: summary, Encrypted: item.EncryptedContent, Provider: ir.Responses,
			}
			if len(item.Content) > 0 {
				blocks, _, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
				if err == nil {
					var b strings.Builder
					for _, block := range blocks {
						if block.Type == ir.BlockText {
							b.WriteString(block.Text)
						}
					}
					reasoning.Text = b.String()
				}
			}
			appendAssistant(ir.Block{Type: ir.BlockReasoning, Reasoning: reasoning})
		case "item_reference":
			return nil, nil, fmt.Errorf("responses input item %d: item_reference requires server-side storage and cannot be translated", i)
		case "custom_tool_call", "custom_tool_call_output", "computer_call_output",
			"local_shell_call", "local_shell_call_output", "shell_call", "shell_call_output",
			"apply_patch_call", "apply_patch_call_output", "mcp_call", "mcp_list_tools",
			"mcp_approval_request", "mcp_approval_response", "tool_search_call",
			"tool_search_output", "additional_tools", "configuration_update",
			"compaction", "compaction_trigger", "image_generation_call",
			"code_interpreter_call", "program", "program_output":
			warns.Add(ir.WarningDropped, "input."+typ, "provider-specific item has no cross-protocol equivalent")
		default:
			warns.Add(ir.WarningDropped, "input."+typ, "unsupported input item")
		}
	}
	return out, warns, nil
}

func decodeResponsesContent(raw json.RawMessage, role ir.Role) ([]ir.Block, ir.Warnings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil, nil
		}
		return []ir.Block{ir.TextBlock(text)}, nil, nil
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return nil, nil, fmt.Errorf("content part array exceeds %d parts", metrics.ContentPartMax)
	}
	var parts []struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Refusal    string `json:"refusal"`
		ImageURL   string `json:"image_url"`
		FileID     string `json:"file_id"`
		FileData   string `json:"file_data"`
		FileURL    string `json:"file_url"`
		Filename   string `json:"filename"`
		Detail     string `json:"detail"`
		InputAudio *struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, err
	}
	var (
		out   []ir.Block
		warns ir.Warnings
	)
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, ir.TextBlock(p.Text))
		case "refusal":
			out = append(out, ir.Block{Type: ir.BlockRefusal, Refusal: p.Refusal})
		case "input_image":
			img := &ir.Image{URL: p.ImageURL, FileID: p.FileID, Detail: p.Detail}
			normalizeImageURL(img)
			out = append(out, ir.Block{Type: ir.BlockImage, Image: img})
		case "input_file":
			out = append(out, ir.Block{Type: ir.BlockFile, File: &ir.File{
				FileID: p.FileID, FileData: p.FileData, FileURL: p.FileURL,
				Filename: p.Filename, Detail: p.Detail,
			}})
		case "input_audio":
			if p.InputAudio == nil {
				warns.Add(ir.WarningDropped, "content.input_audio", "missing payload")
				continue
			}
			out = append(out, ir.Block{Type: ir.BlockAudio, Audio: &ir.Audio{Data: p.InputAudio.Data, Format: p.InputAudio.Format}})
		default:
			warns.Add(ir.WarningDropped, "content."+p.Type, "unsupported content part")
		}
	}
	_ = role
	return out, warns, nil
}

func decodeResponsesFunctionCall(item responsesWireItem) (*ir.ToolCall, ir.Warnings, error) {
	var warns ir.Warnings
	if item.Name == "" {
		return nil, nil, errors.New("function_call has no name")
	}
	args, err := parseToolArguments(item.Arguments)
	if err != nil {
		return nil, nil, err
	}
	id := item.CallID
	if id == "" {
		id = item.ID
	}
	if id == "" {
		return nil, nil, errors.New("function_call has no call_id")
	}
	return &ir.ToolCall{ID: id, Name: item.Name, Arguments: args}, warns, nil
}

func decodeResponsesTools(in []responsesWireTool) ([]ir.Tool, ir.Warnings, error) {
	var (
		out   []ir.Tool
		warns ir.Warnings
	)
	for i, t := range in {
		if t.Type != "" && t.Type != "function" {
			return nil, nil, fmt.Errorf("responses tool %d: provider-defined type %q has no cross-protocol equivalent", i, t.Type)
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, nil, fmt.Errorf("responses tool %d: name is required", i)
		}
		params := t.Parameters
		if len(params) == 0 || bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
			params = json.RawMessage(`{}`)
		}
		out = append(out, ir.Tool{
			Type: "function", Name: t.Name, Description: t.Description,
			Parameters: params, Strict: t.Strict,
		})
	}
	return out, warns, nil
}

func decodeResponsesToolChoice(raw json.RawMessage) (*ir.ToolChoice, error) {
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
		}
		return nil, fmt.Errorf("unsupported responses tool_choice %q", shorthand)
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("responses tool_choice: %w", err)
	}
	if obj.Type == "function" && obj.Name != "" {
		return &ir.ToolChoice{Mode: ir.ToolChoiceTool, Name: obj.Name}, nil
	}
	return nil, fmt.Errorf("unsupported responses tool_choice type %q", obj.Type)
}

func decodeResponsesFormat(raw json.RawMessage) (*ir.ResponseFormat, string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, "", nil
	}
	var obj struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Strict      *bool           `json:"strict"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "", fmt.Errorf("responses text.format: %w", err)
	}
	switch obj.Type {
	case "", "text":
		return &ir.ResponseFormat{Type: "text"}, "", nil
	case "json_object":
		return &ir.ResponseFormat{Type: "json_object"}, "", nil
	case "json_schema":
		if len(obj.Schema) == 0 {
			return nil, "", errors.New("responses text.format.json_schema.schema is required")
		}
		return &ir.ResponseFormat{
			Type: "json_schema", Name: obj.Name, Description: obj.Description,
			Schema: obj.Schema, Strict: obj.Strict,
		}, "", nil
	default:
		return nil, "unsupported text.format type " + obj.Type, nil
	}
}

// EncodeResponsesRequest renders the canonical request as a Responses body.
func EncodeResponsesRequest(req *ir.Request) ([]byte, error) {
	if err := ir.ValidateRequest(req); err != nil {
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
	if req.Reasoning != nil {
		if req.Reasoning.BudgetTokens != nil && req.Reasoning.Effort == "" {
			req.Reasoning.Effort = effortFromBudget(*req.Reasoning.BudgetTokens)
			req.Warnings.Add(ir.WarningApproximated, "reasoning.budget_tokens", "mapped to reasoning.effort")
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
		if folded := foldLeadingSystem(req.Messages); folded != "" {
			instructions = &folded
		}
	}
	if instructions != nil && *instructions != "" {
		out["instructions"] = *instructions
	}
	if req.MaxOutputTokens != nil {
		out["max_output_tokens"] = *req.MaxOutputTokens
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
		if len(r) > 0 {
			out["reasoning"] = r
		}
	}
	if req.ResponseFormat != nil {
		out["text"] = map[string]any{"format": encodeResponsesFormat(req.ResponseFormat)}
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
	if req.ServiceTier != "" {
		out["service_tier"] = req.ServiceTier
	}
	if req.User != "" {
		out["user"] = req.User
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
// Responses instructions field.
func foldLeadingSystem(messages []ir.Message) string {
	var parts []string
	for _, m := range messages {
		if m.Role != ir.RoleSystem && m.Role != ir.RoleDeveloper {
			break
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockText && b.Text != "" {
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
		switch m.Role {
		case ir.RoleAssistant:
			var textBlocks []ir.Block
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockReasoning, ir.BlockRedactedReasoning:
					out = append(out, encodeResponsesReasoning(b.Reasoning, &req.Warnings))
				case ir.BlockToolCall:
					out = append(out, encodeResponsesFunctionCall(b.ToolCall))
				case ir.BlockText, ir.BlockRefusal:
					textBlocks = append(textBlocks, b)
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
			warns.Add(ir.WarningDropped, "cache_control", "Responses input has no cache breakpoints")
		}
		if len(b.Citations) > 0 && warns != nil {
			warns.Add(ir.WarningDropped, "citations", "Responses input has no citations")
		}
		switch b.Type {
		case ir.BlockText:
			textType := "input_text"
			if role == ir.RoleAssistant {
				textType = "output_text"
			}
			out = append(out, map[string]any{"type": textType, "text": b.Text})
		case ir.BlockRefusal:
			out = append(out, map[string]any{"type": "refusal", "refusal": b.Refusal})
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
			if b.File.FileData != "" {
				part["file_data"] = b.File.FileData
			}
			if b.File.FileURL != "" {
				part["file_url"] = b.File.FileURL
			}
			if b.File.Filename != "" {
				part["filename"] = b.File.Filename
			}
			out = append(out, part)
		case ir.BlockAudio:
			out = append(out, map[string]any{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": b.Audio.Data, "format": b.Audio.Format},
			})
		}
	}
	return out
}

func encodeResponsesReasoning(r *ir.Reasoning, warns *ir.Warnings) map[string]any {
	if r == nil {
		return map[string]any{"type": "reasoning", "summary": []any{}}
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
	item := map[string]any{"type": "reasoning", "summary": summary}
	if r.ID != "" {
		item["id"] = r.ID
	}
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

func encodeResponsesFunctionCall(c *ir.ToolCall) map[string]any {
	args := c.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return map[string]any{
		"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": string(args),
	}
}

func encodeResponsesTools(in []ir.Tool, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, t := range in {
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
		out := map[string]any{"type": "json_schema", "schema": rf.Schema}
		if rf.Name != "" {
			out["name"] = rf.Name
		}
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

type responsesWireResponse struct {
	ID                string              `json:"id"`
	Object            string              `json:"object"`
	CreatedAt         int64               `json:"created_at"`
	Status            string              `json:"status"`
	Error             *responsesWireError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Instructions       json.RawMessage     `json:"instructions"`
	MaxOutputTokens    *int                `json:"max_output_tokens"`
	Model              string              `json:"model"`
	Output             []responsesWireItem `json:"output"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	PreviousResponseID *string             `json:"previous_response_id"`
	Reasoning          json.RawMessage     `json:"reasoning"`
	Store              *bool               `json:"store"`
	Temperature        *float64            `json:"temperature"`
	Text               json.RawMessage     `json:"text"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	Tools              []responsesWireTool `json:"tools"`
	TopP               *float64            `json:"top_p"`
	Truncation         string              `json:"truncation"`
	Usage              *responsesWireUsage `json:"usage"`
}

type responsesWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
	Type    string `json:"type"`
}

type responsesWireUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// DecodeResponsesResponse converts one non-streaming Responses body into the
// canonical response.
func DecodeResponsesResponse(body []byte) (*ir.Response, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("responses response body is not valid UTF-8")
	}
	var in responsesWireResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	resp := &ir.Response{
		ID:      in.ID,
		Model:   in.Model,
		Created: in.CreatedAt,
		Status:  in.Status,
	}
	if in.Usage != nil {
		resp.Usage = responsesUsageToIR(in.Usage)
	}
	if in.Status == "failed" || in.Error != nil {
		e := &ir.Error{Message: "upstream response failed"}
		if in.Error != nil {
			e.Code = in.Error.Code
			e.Type = in.Error.Type
			if in.Error.Message != "" {
				e.Message = in.Error.Message
			}
		}
		resp.Error = e
		resp.Finish = ir.FinishError
		return resp, nil
	}
	for i, item := range in.Output {
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			blocks, warns, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
			if err != nil {
				return nil, fmt.Errorf("responses output item %d: %w", i, err)
			}
			resp.Blocks = append(resp.Blocks, blocks...)
			resp.Warnings = append(resp.Warnings, warns...)
		case "function_call":
			call, warns, err := decodeResponsesFunctionCall(item)
			if err != nil {
				return nil, fmt.Errorf("responses output item %d: %w", i, err)
			}
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
			resp.Warnings = append(resp.Warnings, warns...)
		case "reasoning":
			var summary []string
			for _, s := range item.Summary {
				if s.Text != "" {
					summary = append(summary, s.Text)
				}
			}
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
				ID: item.ID, Summary: summary, Encrypted: item.EncryptedContent, Provider: ir.Responses,
			}})
		default:
			resp.Warnings.Add(ir.WarningDropped, "output."+typ, "provider-specific item has no cross-protocol equivalent")
		}
	}
	if in.IncompleteDetails != nil {
		resp.IncompleteReason = in.IncompleteDetails.Reason
		switch in.IncompleteDetails.Reason {
		case "max_output_tokens":
			resp.Finish = ir.FinishLength
		case "content_filter":
			resp.Finish = ir.FinishContentFilter
		default:
			resp.Finish = ir.FinishOther
		}
	} else if hasToolCall(resp.Blocks) {
		resp.Finish = ir.FinishToolCalls
	} else {
		resp.Finish = ir.FinishStop
	}
	return resp, nil
}

func hasToolCall(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type == ir.BlockToolCall {
			return true
		}
	}
	return false
}

func responsesUsageToIR(in *responsesWireUsage) ir.Usage {
	u := ir.Usage{
		InputTokens:  in.InputTokens,
		OutputTokens: in.OutputTokens,
		TotalTokens:  in.TotalTokens,
	}
	if in.InputTokensDetails != nil {
		u.CacheReadTokens = in.InputTokensDetails.CachedTokens
		u.CacheWriteTokens = in.InputTokensDetails.CacheWriteTokens
	}
	if in.OutputTokensDetails != nil {
		u.ReasoningTokens = in.OutputTokensDetails.ReasoningTokens
	}
	return u
}

// EncodeResponsesResponse renders the canonical response as a Responses body.
func EncodeResponsesResponse(resp *ir.Response) ([]byte, error) {
	if err := ir.ValidateResponse(resp); err != nil {
		return nil, err
	}
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	status := "completed"
	if resp.Status != "" {
		status = resp.Status
	}
	if resp.Error != nil {
		status = "failed"
	}
	out := map[string]any{
		"id":                   resp.ID,
		"object":               "response",
		"created_at":           created,
		"status":               status,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                resp.Model,
		"output":               encodeResponsesOutput(resp.Blocks, &resp.Warnings),
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            nil,
		"store":                false,
		"temperature":          nil,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                nil,
		"truncation":           "disabled",
	}
	if resp.Error != nil {
		code := resp.Error.Code
		if code == "" {
			code = "server_error"
		}
		out["error"] = map[string]any{
			"code":    code,
			"message": resp.Error.Message,
		}
	}
	if resp.Finish == ir.FinishLength || resp.Finish == ir.FinishContentFilter {
		out["status"] = "incomplete"
		reason := "max_output_tokens"
		if resp.Finish == ir.FinishContentFilter {
			reason = "content_filter"
		}
		out["incomplete_details"] = map[string]any{"reason": reason}
	}
	if resp.Usage != (ir.Usage{}) {
		out["usage"] = encodeResponsesUsage(resp.Usage)
	}
	return sse.MarshalWireJSON(out)
}

func encodeResponsesOutput(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	var textBlocks []ir.Block
	flushText := func() {
		if len(textBlocks) == 0 {
			return
		}
		out = append(out, map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": encodeResponsesOutputContent(textBlocks, warns),
		})
		textBlocks = nil
	}
	for _, b := range blocks {
		if b.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "cache_control", "Responses output has no cache breakpoints")
		}
		if len(b.Citations) > 0 && warns != nil {
			warns.Add(ir.WarningDropped, "citations", "Responses output has no citation mapping")
		}
		switch b.Type {
		case ir.BlockText, ir.BlockRefusal:
			textBlocks = append(textBlocks, b)
		case ir.BlockToolCall:
			flushText()
			out = append(out, encodeResponsesFunctionCall(b.ToolCall))
		case ir.BlockReasoning:
			flushText()
			out = append(out, encodeResponsesReasoning(b.Reasoning, warns))
		case ir.BlockRedactedReasoning:
			flushText()
			out = append(out, encodeResponsesReasoning(b.Reasoning, warns))
		}
	}
	flushText()
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func encodeResponsesOutputContent(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			out = append(out, map[string]any{
				"type": "output_text", "text": b.Text, "annotations": []any{}, "logprobs": []any{},
			})
		case ir.BlockRefusal:
			out = append(out, map[string]any{"type": "refusal", "refusal": b.Refusal})
		}
	}
	return out
}

func encodeResponsesUsage(u ir.Usage) map[string]any {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	out := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  total,
		"input_tokens_details": map[string]any{
			"cached_tokens":      u.CacheReadTokens,
			"cache_write_tokens": u.CacheWriteTokens,
		},
		"output_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
	}
	return out
}

// DecodeResponsesError decodes the standard OpenAI error envelope used by the
// Responses HTTP surface.
func DecodeResponsesError(body []byte) (*ir.Error, error) {
	var in struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	if in.Error.Message == "" && in.Error.Type == "" {
		return nil, errors.New("not an OpenAI error envelope")
	}
	code := ""
	switch v := in.Error.Code.(type) {
	case string:
		code = v
	}
	return &ir.Error{Type: in.Error.Type, Code: code, Message: in.Error.Message}, nil
}
