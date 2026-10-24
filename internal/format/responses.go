package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
		Effort          string `json:"effort"`
		Summary         string `json:"summary"`
		GenerateSummary string `json:"generate_summary"`
		Context         string `json:"context"`
		Mode            string `json:"mode"`
	} `json:"reasoning"`
	Text *struct {
		Format    json.RawMessage `json:"format"`
		Verbosity string          `json:"verbosity"`
	} `json:"text"`
	Stream               bool                   `json:"stream"`
	Store                *bool                  `json:"store"`
	PreviousResponseID   string                 `json:"previous_response_id"`
	Metadata             map[string]string      `json:"metadata"`
	ServiceTier          string                 `json:"service_tier"`
	User                 string                 `json:"user"`
	SafetyIdentifier     string                 `json:"safety_identifier"`
	PromptCacheKey       string                 `json:"prompt_cache_key"`
	Moderation           *ir.Moderation         `json:"moderation"`
	PromptCacheOptions   *ir.PromptCacheOptions `json:"prompt_cache_options"`
	PromptCacheRetention string                 `json:"prompt_cache_retention"`
	Include              []string               `json:"include"`
	Truncation           string                 `json:"truncation"`
	Background           bool                   `json:"background"`
	MaxToolCalls         *int                   `json:"max_tool_calls"`
	Conversation         json.RawMessage        `json:"conversation"`
	Prompt               json.RawMessage        `json:"prompt"`
	ContextManagement    json.RawMessage        `json:"context_management"`
	AccessPrograms       json.RawMessage        `json:"access_programs"`
	StreamOptions        json.RawMessage        `json:"stream_options"`
}

type responsesWireTool struct {
	Type           string          `json:"type"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Parameters     json.RawMessage `json:"parameters"`
	Strict         *bool           `json:"strict"`
	AllowedCallers json.RawMessage `json:"allowed_callers"`
	Async          *bool           `json:"async"`
	DeferLoading   *bool           `json:"defer_loading"`
	OutputSchema   json.RawMessage `json:"output_schema"`
}

// responsesWireAnnotation is one output_text annotation. The flat url shape
// is also the source field set for a Chat url_citation, so both decoders
// share the same IR mapping.
type responsesWireAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	FileID     string `json:"file_id"`
	Filename   string `json:"filename"`
	Index      int    `json:"index"`
}

type responsesWireItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Phase   string          `json:"phase"`
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
	// Bound the collections the typed decode materializes. Input has its own
	// check in decodeResponsesInput, before that function's item decode.
	var bounds struct {
		Tools   json.RawMessage `json:"tools"`
		Include json.RawMessage `json:"include"`
	}
	if err := json.Unmarshal(body, &bounds); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	if err := checkWireArray(bounds.Tools, "tools", WireCollectionMax); err != nil {
		return nil, err
	}
	if err := checkWireArray(bounds.Include, "include", WireCollectionMax); err != nil {
		return nil, err
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
	if len(in.ContextManagement) > 0 && !bytes.Equal(bytes.TrimSpace(in.ContextManagement), []byte("null")) {
		// Compaction is server-side conversation state. The proxy is
		// stateless, so honoring it is impossible and silently dropping it
		// would change the model's context. Reject before any upstream call.
		return nil, errors.New("responses request: context_management is not supported")
	}
	if len(in.Conversation) > 0 && !bytes.Equal(bytes.TrimSpace(in.Conversation), []byte("null")) {
		return nil, errors.New("responses request: conversation is not supported")
	}
	if len(in.Prompt) > 0 && !bytes.Equal(bytes.TrimSpace(in.Prompt), []byte("null")) {
		return nil, errors.New("responses request: prompt templates are not supported")
	}
	req := &ir.Request{
		Model:                in.Model,
		Instructions:         in.Instructions,
		MaxOutputTokens:      in.MaxOutputTokens,
		Temperature:          in.Temperature,
		TopP:                 in.TopP,
		TopLogprobs:          in.TopLogprobs,
		ParallelToolCalls:    in.ParallelToolCalls,
		Stream:               in.Stream,
		Store:                in.Store,
		PreviousResponseID:   in.PreviousResponseID,
		Metadata:             in.Metadata,
		ServiceTier:          in.ServiceTier,
		User:                 in.User,
		SafetyIdentifier:     in.SafetyIdentifier,
		PromptCacheKey:       in.PromptCacheKey,
		Moderation:           in.Moderation,
		PromptCacheOptions:   in.PromptCacheOptions,
		PromptCacheRetention: in.PromptCacheRetention,
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
		// generate_summary is the deprecated alias for summary; both spellings
		// describe the same request, so the explicit summary wins.
		summary := in.Reasoning.Summary
		if summary == "" {
			summary = in.Reasoning.GenerateSummary
		}
		req.Reasoning = &ir.ReasoningConfig{
			Effort: in.Reasoning.Effort, Summary: summary,
			Context: in.Reasoning.Context, Mode: in.Reasoning.Mode,
		}
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
		req.Verbosity = in.Text.Verbosity
	}
	if in.Truncation != "" && in.Truncation != "disabled" {
		req.Warnings.Add(ir.WarningDropped, "truncation", "server-side truncation is not translated")
	}
	if in.MaxToolCalls != nil {
		req.Warnings.Add(ir.WarningDropped, "max_tool_calls", "no cross-protocol equivalent")
	}
	if wireValuePresent(in.AccessPrograms) {
		req.Warnings.Add(ir.WarningDropped, "access_programs", "provider access programs have no cross-protocol equivalent")
	}
	if wireValuePresent(in.StreamOptions) {
		// stream_options is an object. A non-object or a wrong-typed
		// include_obfuscation is malformed input, not a translatable option:
		// fail closed at the trust boundary. Every other present member is a
		// loss the target cannot carry, so record it (reject mode turns the
		// warning into a 400). Names are visited in sorted order so warning
		// order is deterministic.
		var options map[string]json.RawMessage
		if err := json.Unmarshal(in.StreamOptions, &options); err != nil {
			return nil, errors.New("responses request: stream_options must be a JSON object")
		}
		names := make([]string, 0, len(options))
		for name := range options {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			raw := options[name]
			if !wireValuePresent(raw) {
				continue
			}
			if name == "include_obfuscation" {
				var include bool
				if err := json.Unmarshal(raw, &include); err != nil {
					return nil, errors.New("responses request: stream_options.include_obfuscation must be a boolean")
				}
				req.Warnings.Add(ir.WarningDropped, "stream_options.include_obfuscation", "stream obfuscation settings have no cross-protocol equivalent")
				continue
			}
			req.Warnings.Add(ir.WarningDropped, "stream_options."+name, "stream option has no cross-protocol equivalent")
		}
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
	if len(raw) == 0 {
		// The Responses reference marks input optional. An absent field means
		// no messages; the target encoders then decide from instructions (a
		// Chat system message) or fail closed on an empty conversation.
		return nil, nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// Explicit null is not a valid input value even though the field is
		// optional.
		return nil, nil, errors.New("responses request: input must not be null")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil, nil
		}
		return []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock(text)}}}, nil, nil
	}
	if err := checkWireArray(raw, "input", WireCollectionMax); err != nil {
		return nil, nil, err
	}
	// Reasoning items carry a summary array that the typed item decode would
	// materialize unbounded. The enclosing item array is bounded above; the
	// summaries also share one aggregate budget so thousands of items at the
	// per-item cap cannot multiply. Every item's content and function-call
	// output share one part and annotation budget for the same reason.
	var itemBounds []struct {
		Summary json.RawMessage `json:"summary"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &itemBounds); err != nil {
		return nil, nil, fmt.Errorf("responses input: %w", err)
	}
	summaryTotal := 0
	var nested responsesNestedBudget
	for i, item := range itemBounds {
		if err := checkWireArray(item.Summary, "input.summary", metrics.ContentPartMax); err != nil {
			return nil, nil, err
		}
		if err := addWireArray(&summaryTotal, item.Summary, "input.summary", WireNestedCollectionMax); err != nil {
			return nil, nil, err
		}
		if err := checkResponsesContent(&nested, item.Content, fmt.Sprintf("input[%d].content", i)); err != nil {
			return nil, nil, err
		}
		if err := checkResponsesContent(&nested, item.Output, fmt.Sprintf("input[%d].output", i)); err != nil {
			return nil, nil, err
		}
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
	phaseSeen := false
	for i, item := range items {
		if item.Phase != "" {
			phaseSeen = true
		}
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
			if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
				return nil, nil, err
			}
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
				blocks, w, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
				if err != nil {
					return nil, nil, fmt.Errorf("responses input item %d: %w", i, err)
				}
				warns = append(warns, w...)
				var b strings.Builder
				for _, block := range blocks {
					if block.Type == ir.BlockText {
						b.WriteString(block.Text)
					}
				}
				reasoning.Text = b.String()
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
	if phaseSeen {
		// One warning per request: the phase is a per-item hint, and
		// repeating it for every item would amplify the header.
		warns.Add(ir.WarningDropped, "input.phase", "input item phase has no cross-protocol equivalent")
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
		Annotations           []responsesWireAnnotation `json:"annotations"`
		PromptCacheBreakpoint json.RawMessage           `json:"prompt_cache_breakpoint"`
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
		case "input_text", "output_text", "text", "reasoning_text":
			block := ir.TextBlock(p.Text)
			citations, w := decodeResponsesAnnotations(p.Annotations)
			block.Citations = citations
			warns = append(warns, w...)
			out = append(out, block)
		case "refusal":
			out = append(out, ir.Block{Type: ir.BlockRefusal, Refusal: p.Refusal})
		case "input_image":
			img := &ir.Image{URL: p.ImageURL, FileID: p.FileID, Detail: p.Detail}
			normalizeImageURL(img)
			out = append(out, ir.Block{Type: ir.BlockImage, Image: img})
		case "input_file":
			file := &ir.File{
				FileID: p.FileID, FileData: p.FileData, FileURL: p.FileURL,
				Filename: p.Filename, Detail: p.Detail,
			}
			normalizeFileData(file)
			out = append(out, ir.Block{Type: ir.BlockFile, File: file})
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

// decodeResponsesAnnotations maps the annotation shapes the IR can represent.
// A url_citation becomes a citation on its text block; a type whose source
// identity cannot be carried is reported rather than silently discarded.
func decodeResponsesAnnotations(in []responsesWireAnnotation) ([]ir.Citation, ir.Warnings) {
	var (
		out   []ir.Citation
		warns ir.Warnings
	)
	for _, a := range in {
		switch a.Type {
		case "url_citation":
			if a.URL == "" {
				warns.Add(ir.WarningDropped, "annotations.url_citation", "url_citation has no url")
				continue
			}
			out = append(out, ir.Citation{
				Type: "url_citation", URL: a.URL, Title: a.Title,
				StartIndex: a.StartIndex, EndIndex: a.EndIndex,
			})
		case "":
			warns.Add(ir.WarningDropped, "annotations", "annotation has no type")
		default:
			warns.Add(ir.WarningDropped, "annotations."+a.Type, "annotation has no cross-protocol equivalent")
		}
	}
	return out, warns
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
	if err := checkNativeIdentity("tool id", id); err != nil {
		return nil, nil, err
	}
	if err := checkNativeIdentity("tool name", item.Name); err != nil {
		return nil, nil, err
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
		present, err := toolAllowedCallers(t.AllowedCallers)
		if err != nil {
			return nil, nil, fmt.Errorf("responses tool %d: %w", i, err)
		}
		if present {
			warns.Add(ir.WarningDropped, "tool.allowed_callers", "only direct invocation is representable")
		}
		if t.Async != nil && *t.Async {
			warns.Add(ir.WarningDropped, "tool.async", "asynchronous execution has no cross-protocol equivalent")
		}
		if t.DeferLoading != nil && *t.DeferLoading {
			warns.Add(ir.WarningDropped, "tool.defer_loading", "deferred tool loading has no cross-protocol equivalent")
		}
		if wireValuePresent(t.OutputSchema) {
			warns.Add(ir.WarningDropped, "tool.output_schema", "tool output schemas have no cross-protocol equivalent")
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
					out = append(out, encodeResponsesFunctionCall(b.ToolCall))
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
		}
	}
	return out
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

type responsesWireResponse struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	// Error is raw because the shared envelope parser deliberately accepts a
	// string value and a numeric code; a typed object would reject those
	// shapes before the proxy can record the provider's failure.
	Error             json.RawMessage `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Instructions       json.RawMessage     `json:"instructions"`
	MaxOutputTokens    *int                `json:"max_output_tokens"`
	Model              string              `json:"model"`
	Metadata           map[string]string   `json:"metadata"`
	ServiceTier        string              `json:"service_tier"`
	Output             []responsesWireItem `json:"output"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	PreviousResponseID *string             `json:"previous_response_id"`
	Reasoning          json.RawMessage     `json:"reasoning"`
	Store              *bool               `json:"store"`
	Temperature        *float64            `json:"temperature"`
	Text               json.RawMessage     `json:"text"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	TopP               *float64            `json:"top_p"`
	Truncation         string              `json:"truncation"`
	Usage              *responsesWireUsage `json:"usage"`
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

// checkResponsesResponseBounds preflights the array shapes a Responses
// response materializes before the typed decode. It is shared by the
// non-streaming decoder and the streaming created/terminal branches so one
// large document cannot amplify into a large typed allocation in either path.
// The probe only retains raw members; it stops at the first over-bound array.
func checkResponsesResponseBounds(body []byte) error {
	var probe struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("parse responses response: %w", err)
	}
	if !metrics.ContentPartsWithinLimit(probe.Output) {
		return fmt.Errorf("responses output exceeds %d items", metrics.ContentPartMax)
	}
	// A reasoning output item's summary array is materialized by the typed
	// item decode; the bounded output array keeps this probe slice small. An
	// absent or null output is a provider error envelope (or an empty
	// response) and has no items to inspect.
	if len(probe.Output) > 0 && !bytes.Equal(bytes.TrimSpace(probe.Output), []byte("null")) {
		var itemBounds []struct {
			Summary json.RawMessage `json:"summary"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(probe.Output, &itemBounds); err != nil {
			return fmt.Errorf("parse responses response: %w", err)
		}
		summaryTotal := 0
		var nested responsesNestedBudget
		for i, item := range itemBounds {
			if err := checkWireArray(item.Summary, "output.summary", metrics.ContentPartMax); err != nil {
				return err
			}
			if err := addWireArray(&summaryTotal, item.Summary, "output.summary", WireNestedCollectionMax); err != nil {
				return err
			}
			if err := checkResponsesContent(&nested, item.Content, fmt.Sprintf("output[%d].content", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// DecodeResponsesResponse converts one non-streaming Responses body into the
// canonical response.
func DecodeResponsesResponse(body []byte) (*ir.Response, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("responses response body is not valid UTF-8")
	}
	if err := checkResponsesResponseBounds(body); err != nil {
		return nil, err
	}
	var in responsesWireResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	if err := checkNativeIdentity("id", in.ID); err != nil {
		return nil, err
	}
	if err := checkNativeIdentity("model", in.Model); err != nil {
		return nil, err
	}
	switch in.Status {
	case "completed", "incomplete", "failed", "cancelled":
	case "":
		// A provider error envelope may legitimately omit status; anything
		// else without a terminal status is an unfinished response that must
		// not be rendered as a completed one.
		if typ, _, _ := metrics.ParseErrorEnvelope(body); typ == "" {
			return nil, errors.New("responses response has no terminal status")
		}
	default:
		return nil, fmt.Errorf("responses response is not terminal (status %q)", in.Status)
	}
	resp := &ir.Response{
		ID:          in.ID,
		Model:       in.Model,
		Created:     in.CreatedAt,
		Status:      in.Status,
		Metadata:    in.Metadata,
		ServiceTier: in.ServiceTier,
	}
	if in.Usage != nil {
		resp.Usage = responsesUsageToIR(in.Usage)
		if err := validateUsage(resp.Usage); err != nil {
			return nil, err
		}
	}
	if in.Status == "failed" {
		e := &ir.Error{Message: "upstream response failed"}
		if typ, code, msg := metrics.ParseErrorComponents(in.Error); typ != "" || code != "" || msg != "" {
			e.Type, e.Code = typ, code
			if msg != "" {
				e.Message = msg
			}
		}
		resp.Error = e
		resp.Finish = ir.FinishError
		return resp, nil
	}
	// An in-band provider error on a nominally completed response is decoded
	// through the shared decoy-aware parser: an empty error object is not a
	// failure, while a string or numeric-code envelope still classifies.
	if typ, code, msg := metrics.ParseErrorEnvelope(body); typ != "" {
		resp.Error = &ir.Error{Type: typ, Code: code, Message: msg}
		resp.Finish = ir.FinishError
		return resp, nil
	}
	for i, item := range in.Output {
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch {
		case typ == "message":
			blocks, warns, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
			if err != nil {
				return nil, fmt.Errorf("responses output item %d: %w", i, err)
			}
			resp.Blocks = append(resp.Blocks, blocks...)
			resp.Warnings = append(resp.Warnings, warns...)
		case typ == "function_call":
			call, warns, err := decodeResponsesFunctionCall(item)
			if err != nil {
				return nil, fmt.Errorf("responses output item %d: %w", i, err)
			}
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockToolCall, ToolCall: call})
			resp.Warnings = append(resp.Warnings, warns...)
		case typ == "reasoning":
			if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
				return nil, err
			}
			var summary []string
			for _, s := range item.Summary {
				if s.Text != "" {
					summary = append(summary, s.Text)
				}
			}
			var reasoningText strings.Builder
			if len(item.Content) > 0 {
				blocks, warns, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
				if err != nil {
					return nil, fmt.Errorf("responses output item %d: %w", i, err)
				}
				resp.Warnings = append(resp.Warnings, warns...)
				for _, block := range blocks {
					if block.Type == ir.BlockText {
						reasoningText.WriteString(block.Text)
					}
				}
			}
			resp.Blocks = append(resp.Blocks, ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
				ID: item.ID, Text: reasoningText.String(), Summary: summary,
				Encrypted: item.EncryptedContent, Provider: ir.Responses,
			}})
		case sse.ResponsesToolCallItem(typ):
			// A provider-executed tool item (custom, local shell, MCP,
			// computer, ...) has no cross-protocol block. It is still tool
			// activity: count it so the finish is tool_calls and the record
			// does not bill its argument output as answer generation. The
			// translated wire keeps the existing dropped-item warning.
			resp.ProviderToolCalls++
			resp.Warnings.Add(ir.WarningDropped, "output."+typ, "provider-specific item has no cross-protocol equivalent")
		default:
			resp.Warnings.Add(ir.WarningDropped, "output."+typ, "provider-specific item has no cross-protocol equivalent")
		}
	}
	if in.IncompleteDetails != nil && in.IncompleteDetails.Reason != "" {
		resp.IncompleteReason = in.IncompleteDetails.Reason
		switch in.IncompleteDetails.Reason {
		case "max_output_tokens":
			resp.Finish = ir.FinishLength
		case "content_filter":
			resp.Finish = ir.FinishContentFilter
		default:
			// max_tool_calls, steered and provider-specific reasons are not a
			// clean stop and have no exact cross-protocol equivalent.
			resp.Finish = ir.FinishOther
		}
	} else if in.Status == "incomplete" {
		resp.Finish = ir.FinishOther
	} else if in.Status == "cancelled" {
		resp.Finish = ir.FinishCancelled
	} else if hasToolCall(resp.Blocks) || resp.ProviderToolCalls > 0 {
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

// ResponsesEnvelope describes one full Responses response object. The stream
// encoder, the non-stream encoder and the proxy's synthetic error frames all
// build their response from this owner, so a required envelope field cannot
// drift between them.
type ResponsesEnvelope struct {
	ID               string
	Model            string
	Created          int64
	Status           string
	IncompleteReason string
	Output           []any
	Error            *ir.Error
	Usage            ir.Usage
	Metadata         map[string]string
	ServiceTier      string
}

// NewResponsesEnvelope builds the complete response object required by the
// Responses schema. access_programs, metadata, error, incomplete_details,
// instructions, temperature and top_p are required keys even when null.
func NewResponsesEnvelope(env ResponsesEnvelope) (map[string]any, error) {
	created := env.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	status := env.Status
	if status == "" {
		status = "completed"
	}
	output := env.Output
	if output == nil {
		output = []any{}
	}
	metadata := env.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	out := map[string]any{
		"id":                   env.ID,
		"object":               "response",
		"access_programs":      nil,
		"created_at":           created,
		"status":               status,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"metadata":             metadata,
		"model":                env.Model,
		"output":               output,
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
	if status == "incomplete" && env.IncompleteReason != "" {
		out["incomplete_details"] = map[string]any{"reason": env.IncompleteReason}
	}
	if env.Error != nil {
		code := env.Error.Code
		if code == "" {
			code = "server_error"
		}
		failure := map[string]any{"code": code, "message": env.Error.Message}
		// ResponseError.type is the source error spelling when the provider
		// recorded one; an absent type stays absent rather than inventing a
		// vocabulary value.
		if env.Error.Type != "" {
			failure["type"] = env.Error.Type
		}
		out["error"] = failure
	}
	if env.ServiceTier != "" {
		// Every envelope caller goes through the Responses vocabulary; a
		// source tier from another wire cannot leak a foreign spelling.
		if tier, _ := mapServiceTier(ir.Responses, env.ServiceTier); tier != "" {
			out["service_tier"] = tier
		}
	}
	if env.Usage != (ir.Usage{}) {
		usage, err := encodeResponsesUsage(env.Usage)
		if err != nil {
			return nil, err
		}
		out["usage"] = usage
	}
	return out, nil
}

// responsesAnyItems converts typed output items for the shared envelope.
func responsesAnyItems(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// ResponsesSyntheticEvent builds one complete synthetic Responses SSE event
// for the proxy's committed-socket error sinks: the error event carries the
// required sequence_number and the failed event carries both sequence_number
// and the full required response envelope. payload and frame are returned so
// an analyzer can observe the same document the client receives.
func ResponsesSyntheticEvent(eventType string, sequence int64, id, model, code, message string) (payload, frame []byte, err error) {
	event := map[string]any{"type": eventType, "sequence_number": sequence}
	switch eventType {
	case "error":
		event["code"] = code
		event["message"] = message
		event["param"] = nil
	case "response.failed":
		env, envErr := NewResponsesEnvelope(ResponsesEnvelope{
			ID: responsesWireID(id), Model: model, Status: "failed",
			Error: &ir.Error{Code: code, Message: message},
		})
		if envErr != nil {
			return nil, nil, envErr
		}
		event["response"] = env
	default:
		return nil, nil, fmt.Errorf("unsupported synthetic Responses event %q", eventType)
	}
	payload, err = sse.MarshalWireJSON(event)
	if err != nil {
		return nil, nil, err
	}
	frame = append([]byte("event: "+eventType+"\ndata: "), payload...)
	frame = append(frame, '\n', '\n')
	return payload, frame, nil
}

// responsesWireID shapes a synthetic Responses identity. Encoder-created
// responses use the resp_ prefix; a proxy request id must not leak into the
// Responses envelope as a non-conforming id.
func responsesWireID(id string) string {
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	if id == "" {
		return "resp_proxy"
	}
	return "resp_" + id
}

// EncodeResponsesResponse renders the canonical response as a Responses body.
func EncodeResponsesResponse(resp *ir.Response) ([]byte, error) {
	if err := ir.ValidateResponse(resp); err != nil {
		return nil, err
	}
	if err := validateUsage(resp.Usage); err != nil {
		return nil, err
	}
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	status, incompleteReason, approximated := responsesTerminalStatus(resp.Finish, resp.IncompleteReason)
	if resp.Status != "" {
		// A Responses source's own lifecycle status is authoritative.
		status = resp.Status
	}
	if resp.Error != nil {
		status = "failed"
	}
	if status != "incomplete" {
		incompleteReason = ""
	}
	if approximated && resp.Status == "" {
		resp.Warnings.Add(ir.WarningApproximated, "finish_reason",
			fmt.Sprintf("finish reason %q is represented as Responses status %q", string(resp.Finish), status))
	}
	tier, tierWarns := mapServiceTier(ir.Responses, resp.ServiceTier)
	resp.Warnings = append(resp.Warnings, tierWarns...)
	out, err := NewResponsesEnvelope(ResponsesEnvelope{
		ID: resp.ID, Model: resp.Model, Created: created, Status: status,
		IncompleteReason: incompleteReason,
		Output:           responsesAnyItems(encodeResponsesOutput(resp.Blocks, &resp.Warnings)),
		Error:            resp.Error, Usage: resp.Usage,
		Metadata: resp.Metadata, ServiceTier: tier,
	})
	if err != nil {
		return nil, err
	}
	if resp.StopSequence != "" {
		resp.Warnings.Add(ir.WarningDropped, "stop_sequence", "Responses has no matched stop sequence")
	}
	return sse.MarshalWireJSON(out)
}

// responsesTerminalStatus maps one canonical finish reason onto the Responses
// lifecycle status and incomplete reason. A raw provider reason with no exact
// Responses equivalent (pause_turn, refusal, an unknown value) is an
// incomplete response and reports an approximation. refusal maps to
// content_filter because Responses has no refusal terminal reason. A source
// Responses reason is already authoritative and is preserved.
func responsesTerminalStatus(finish ir.FinishReason, incompleteReason string) (status, reason string, approximated bool) {
	switch finish {
	case ir.FinishStop, ir.FinishToolCalls:
		return "completed", "", false
	case ir.FinishLength:
		return "incomplete", "max_output_tokens", false
	case ir.FinishContentFilter:
		return "incomplete", "content_filter", false
	case ir.FinishCancelled:
		return "cancelled", "", false
	case ir.FinishError:
		return "failed", "", false
	case ir.FinishOther:
		if incompleteReason != "" {
			return "incomplete", incompleteReason, false
		}
		return "incomplete", "", false
	case "refusal":
		return "incomplete", "content_filter", true
	default:
		if incompleteReason != "" {
			return "incomplete", incompleteReason, false
		}
		return "incomplete", "", true
	}
}

func encodeResponsesOutput(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	var textBlocks []ir.Block
	flushText := func() {
		if len(textBlocks) == 0 {
			return
		}
		out = append(out, map[string]any{
			"id": newResponsesItemID(ir.BlockText), "type": "message", "role": "assistant",
			"status": "completed", "content": encodeResponsesOutputContent(textBlocks, warns),
		})
		textBlocks = nil
	}
	for _, b := range blocks {
		if b.CacheControl != nil && warns != nil {
			warns.Add(ir.WarningDropped, "cache_control", "Responses output has no cache breakpoints")
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

// responsesAnnotations maps the citations an output_text part can carry. The
// Responses url_citation spelling is flat, unlike Chat's wrapper. A citation
// with no URL (for example an Anthropic character location) cannot be
// represented and is reported.
func responsesAnnotations(citations []ir.Citation, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	// Anthropic's web_search_result_location carries url/title but no
	// character span, while Responses requires both span fields. The zero span
	// is therefore fabricated; report it once for this text part.
	spanApproximated := false
	for _, c := range citations {
		if c.URL != "" && c.Type != "url_citation" && c.StartIndex == 0 && c.EndIndex == 0 {
			spanApproximated = true
			break
		}
	}
	if spanApproximated && warns != nil {
		warns.Add(ir.WarningApproximated, "citations", "source citation has no character span; target span is 0..0")
	}
	for _, c := range citations {
		if c.URL == "" {
			if warns != nil {
				warns.Add(ir.WarningDropped, "citations", "citation type has no Responses annotation equivalent")
			}
			continue
		}
		out = append(out, map[string]any{
			"type": "url_citation", "url": c.URL, "title": c.Title,
			"start_index": c.StartIndex, "end_index": c.EndIndex,
		})
	}
	return out
}

func encodeResponsesOutputContent(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			part := map[string]any{
				"type": "output_text", "text": b.Text, "annotations": []any{}, "logprobs": []any{},
			}
			if annotations := responsesAnnotations(b.Citations, warns); len(annotations) > 0 {
				part["annotations"] = annotations
			}
			out = append(out, part)
		case ir.BlockRefusal:
			if len(b.Citations) > 0 && warns != nil {
				warns.Add(ir.WarningDropped, "citations", "refusal parts have no citation mapping")
			}
			out = append(out, map[string]any{"type": "refusal", "refusal": b.Refusal})
		}
	}
	return out
}

func encodeResponsesUsage(u ir.Usage) (map[string]any, error) {
	total := u.TotalTokens
	if total == 0 {
		sum, err := metrics.SumCounts(u.InputTokens, u.OutputTokens)
		if err != nil {
			return nil, fmt.Errorf("usage: %w", err)
		}
		total = sum
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
	return out, nil
}

// DecodeResponsesError decodes the standard OpenAI error envelope used by the
// Responses HTTP surface. The shared decoy-aware parser owns string error
// values and numeric codes, so a provider envelope this proxy can record is
// never rejected by a narrower local shape.
func DecodeResponsesError(body []byte) (*ir.Error, error) {
	if typ, code, msg := metrics.ParseErrorEnvelope(body); typ != "" {
		return &ir.Error{Type: typ, Code: code, Message: msg}, nil
	}
	return nil, errors.New("not an OpenAI error envelope")
}
