package format

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

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
		failure := map[string]any{"code": responsesErrorCode(env.Error.Code), "message": env.Error.Message}
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

// responsesErrorCode maps an internal or provider error code onto the closed
// ResponseErrorCode enum. The failed envelope may carry only one of those 21
// values; the free-form error event and the record keep the specific internal
// code. A documented synonym is translated exactly, and anything else falls
// back to server_error rather than emitting an invalid enum member.
func responsesErrorCode(code string) string {
	if code == "rate_limit_error" {
		return "rate_limit_exceeded"
	}
	switch code {
	case "server_error", "rate_limit_exceeded", "invalid_prompt", "data_residency_mismatch",
		"bio_policy", "misalignment_policy_violation", "vector_store_timeout", "invalid_image",
		"invalid_image_format", "invalid_base64_image", "invalid_image_url", "image_too_large",
		"image_too_small", "image_parse_error", "image_content_policy_violation", "invalid_image_mode",
		"image_file_too_large", "unsupported_image_media_type", "empty_image_file",
		"failed_to_download_image", "image_file_not_found":
		return code
	}
	return "server_error"
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
		Output:           responsesAnyItems(encodeResponsesOutput(resp.Blocks, status, &resp.Warnings)),
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

func encodeResponsesOutput(blocks []ir.Block, status string, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	var textBlocks []ir.Block
	messageStatus := "completed"
	if status != "completed" {
		messageStatus = "incomplete"
	}
	flushText := func() {
		if len(textBlocks) == 0 {
			return
		}
		out = append(out, map[string]any{
			"id": newResponsesItemID(ir.BlockText), "type": "message", "role": "assistant",
			"status": messageStatus, "content": encodeResponsesOutputContent(textBlocks, warns),
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
			out = append(out, encodeResponsesFunctionCall(b.ToolCall, "completed"))
		case ir.BlockReasoning:
			flushText()
			out = append(out, encodeResponsesReasoning(b.Reasoning, warns))
		case ir.BlockRedactedReasoning:
			flushText()
			out = append(out, encodeResponsesReasoning(b.Reasoning, warns))
		default:
			if warns != nil {
				warns.AddOnce(ir.WarningDropped, "output."+string(b.Type),
					"Responses output has no equivalent content part")
			}
		}
	}
	flushText()
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

// responsesAnnotations maps the citations an output_text part can carry. The
// Responses url_citation spelling is flat, unlike Chat's wrapper; its span is
// relative to the message, so offset (the rune length of preceding output text)
// is added to every block-relative IR span. A citation with no URL (for
// example an Anthropic character location) cannot be represented and is
// reported.
func responsesAnnotations(citations []ir.Citation, offset int, warns *ir.Warnings) []map[string]any {
	var out []map[string]any
	// Anthropic's web_search_result_location carries url/title but no
	// character span, while Responses requires both span fields. The part-start
	// offset is therefore fabricated; report it once for this text part.
	spanApproximated := false
	for _, c := range citations {
		if c.URL != "" && c.Type != "url_citation" && c.StartIndex == 0 && c.EndIndex == 0 {
			spanApproximated = true
			break
		}
	}
	if spanApproximated && warns != nil {
		warns.Add(ir.WarningApproximated, "citations",
			fmt.Sprintf("source citation has no character span; target span is %d..%d", offset, offset))
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
			"start_index": offset + c.StartIndex, "end_index": offset + c.EndIndex,
		})
	}
	return out
}

func encodeResponsesOutputContent(blocks []ir.Block, warns *ir.Warnings) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	// textOffset is the running rune length of preceding output text. A
	// Responses url_citation span is message-relative, while an IR citation is
	// block-relative.
	textOffset := 0
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			part := map[string]any{
				"type": "output_text", "text": b.Text, "annotations": []any{}, "logprobs": []any{},
			}
			if annotations := responsesAnnotations(b.Citations, textOffset, warns); len(annotations) > 0 {
				part["annotations"] = annotations
			}
			out = append(out, part)
			textOffset += utf8.RuneCountInString(b.Text)
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
