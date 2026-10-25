package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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
