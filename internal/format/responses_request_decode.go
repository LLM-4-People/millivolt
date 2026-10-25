package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// DecodeResponsesRequest converts one Responses request into the canonical
// request.
func DecodeResponsesRequest(body []byte) (*ir.Request, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("responses request body is not valid UTF-8")
	}
	// Bound the collections the typed decode materializes. Input has its own
	// check in decodeResponsesInput, before that function's item decode.
	var bounds struct {
		Tools         json.RawMessage `json:"tools"`
		Include       json.RawMessage `json:"include"`
		StreamOptions json.RawMessage `json:"stream_options"`
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
	// The member map is decoded below and one warning is added per present
	// member. Bound it before that materializes: a request-sized object of
	// tiny members otherwise expands into a body-sized map plus a body-sized
	// warning join before the header sink cuts it.
	if err := checkWireObject(bounds.StreamOptions, "stream_options", WireCollectionMax); err != nil {
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
	maxOutputTokens, err := jsonnum.OptionalInt(in.MaxOutputTokens, "max_output_tokens")
	if err != nil {
		return nil, fmt.Errorf("responses request: %w", err)
	}
	topLogprobs, err := jsonnum.OptionalInt(in.TopLogprobs, "top_logprobs")
	if err != nil {
		return nil, fmt.Errorf("responses request: %w", err)
	}
	req := &ir.Request{
		Model:                in.Model,
		Instructions:         in.Instructions,
		MaxOutputTokens:      maxOutputTokens,
		Temperature:          in.Temperature,
		TopP:                 in.TopP,
		TopLogprobs:          topLogprobs,
		ParallelToolCalls:    in.ParallelToolCalls,
		Stream:               in.Stream,
		Store:                in.Store,
		PreviousResponseID:   in.PreviousResponseID,
		Metadata:             in.Metadata.values,
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
	maxToolCalls, err := jsonnum.OptionalInt(in.MaxToolCalls, "max_tool_calls")
	if err != nil {
		return nil, fmt.Errorf("responses request: %w", err)
	}
	if maxToolCalls != nil {
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
				// Obfuscation is on by default; false disables it and needs no
				// equivalent on the target. Only true is a loss.
				if include {
					req.Warnings.Add(ir.WarningDropped, "stream_options.include_obfuscation",
						"provider-generated stream obfuscation cannot be translated")
				}
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
	ids := newRequestToolIDs()
	for i, item := range items {
		if item.Type != "function_call" {
			continue
		}
		id := responsesFunctionCallItemID(item)
		if id == "" {
			continue
		}
		if err := ids.preReserve(id); err != nil {
			return nil, nil, fmt.Errorf("responses input item %d: %w", i, err)
		}
	}
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
			call, w, err := decodeResponsesFunctionCall(item, false, ids)
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
		// textOffset is the running rune length of the text parts before the
		// current one. A Responses url_citation span is message-relative while
		// the IR citation is block-relative.
		textOffset int
	)
	for _, p := range parts {
		if rawPresent(p.PromptCacheBreakpoint) {
			warns.Add(ir.WarningDropped, "prompt_cache_breakpoint", "per-part prompt_cache_breakpoint is not mapped")
		}
		switch p.Type {
		case "input_text", "output_text", "text", "reasoning_text":
			block := ir.TextBlock(p.Text)
			partRunes := utf8.RuneCountInString(p.Text)
			citations, w, err := decodeResponsesAnnotations(p.Annotations, textOffset, partRunes)
			if err != nil {
				return nil, nil, err
			}
			block.Citations = citations
			warns = append(warns, w...)
			out = append(out, block)
			textOffset += partRunes
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
			if file.FileData != "" && file.MediaType == "" {
				file.MediaType = inferFileMediaType(file)
			}
			if file.FileData != "" && file.MediaType == "" {
				// The bytes cannot be typed for a target that needs a media
				// type. Keep a file id or URL on the same part; without one
				// the block has no payload left and is dropped.
				warns.Add(ir.WarningDropped, "content.file_data", "inline file data has no recognizable media type")
				file.FileData = ""
				if file.FileID == "" && file.FileURL == "" {
					continue
				}
			}
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
// identity cannot be carried is reported rather than silently discarded. The
// source span is message-relative, so offset (the rune length of preceding text
// parts) is subtracted and a span outside this part is dropped instead of
// emitting a negative or out-of-range block span.
func decodeResponsesAnnotations(in []responsesWireAnnotation, offset, partRunes int) ([]ir.Citation, ir.Warnings, error) {
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
			start, err := intFromJSONIndex("url_citation.start_index", a.StartIndex)
			if err != nil {
				return nil, nil, err
			}
			end, err := intFromJSONIndex("url_citation.end_index", a.EndIndex)
			if err != nil {
				return nil, nil, err
			}
			start -= offset
			end -= offset
			if start < 0 || end < start || end > partRunes {
				warns.Add(ir.WarningDropped, "annotations.span", "url_citation span falls outside its text part")
				continue
			}
			out = append(out, ir.Citation{
				Type: "url_citation", URL: a.URL, Title: a.Title,
				StartIndex: start, EndIndex: end,
			})
		case "":
			warns.Add(ir.WarningDropped, "annotations", "annotation has no type")
		default:
			warns.Add(ir.WarningDropped, "annotations."+a.Type, "annotation has no cross-protocol equivalent")
		}
	}
	return out, warns, nil
}

// responsesFunctionCallItemID returns the id a function-call item references:
// call_id when present, otherwise the item id.
func responsesFunctionCallItemID(item responsesWireItem) string {
	if item.CallID != "" {
		return item.CallID
	}
	return item.ID
}

// decodeResponsesFunctionCall decodes one function-call item. requireEvidence
// rejects a missing, null or whitespace argument value; the response decoder
// sets it for a non-clean terminal, while request history keeps the empty
// object.
func decodeResponsesFunctionCall(item responsesWireItem, requireEvidence bool, ids *requestToolIDs) (*ir.ToolCall, ir.Warnings, error) {
	var warns ir.Warnings
	if item.Name == "" {
		return nil, nil, errors.New("function_call has no name")
	}
	if err := checkNativeIdentity("tool name", item.Name); err != nil {
		return nil, nil, err
	}
	id := responsesFunctionCallItemID(item)
	if id == "" {
		return nil, nil, errors.New("function_call has no call_id")
	}
	if err := checkNativeIdentity("tool id", id); err != nil {
		return nil, nil, err
	}
	if err := ids.reserve(id); err != nil {
		return nil, nil, err
	}
	// Structural identity failures stay fatal even when the source also
	// omitted the argument value; only a well-formed call can be dropped for
	// unusable arguments.
	if requireEvidence && toolArgumentsMissing(item.Arguments) {
		return nil, nil, errToolArgumentsMissing
	}
	args, err := parseToolArguments(item.Arguments)
	if err != nil {
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
