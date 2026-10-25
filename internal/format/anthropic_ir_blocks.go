package format

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// anthropicUserIDMax is the official metadata.user_id maxLength. It counts
// Unicode characters, matching the JSON Schema maxLength semantics.
const anthropicUserIDMax = 512

// anthropicDocumentTitleMax is the official DocumentBlockParam title maxLength,
// counted in Unicode characters.
const anthropicDocumentTitleMax = 500

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
