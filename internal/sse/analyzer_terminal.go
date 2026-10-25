package sse

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// captureTerminalEvent records one Responses terminal document at any
// boundary: line-oriented, buffered or reassembled. A reassembled document is
// the whole event, so its usage object may span several data lines that no
// per-line scan could parse. This owner keeps the terminal flag, wire family,
// raw tail, cost, usage and finish in one place.
func (a *Analyzer) captureTerminalEvent(eventType string, payload []byte, now time.Time) {
	a.terminatorSeen = true
	a.responsesStream = true
	a.usageChunk = append(a.usageChunk[:0], payload...)
	a.captureCost(payload)
	var keys frameKeys
	keys.scan(payload)
	a.captureUsage(&keys, payload)
	a.captureResponsesTerminalFinish(eventType, payload, now)
}

func responsesTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled":
		return true
	}
	return false
}

// captureResponsesTerminalFinish maps the Responses terminal event onto the
// canonical finish vocabulary the record and the degenerate classifier read.
// Without it, a legitimate response.incomplete on a short max_output_tokens was
// classified as an empty completion and a completed answer kept no reason.
// The terminal document may also be the only place the turn's output appears,
// so text, refusal and reasoning presence are recorded here before classifying.
func (a *Analyzer) captureResponsesTerminalFinish(eventType string, payload []byte, now time.Time) {
	// A Responses failure states its provider error under response.error, not
	// the top-level error key the per-line gate looks for. Decode it here so
	// every terminal path - line-oriented, buffered event, and reassembled
	// document at its blank-line boundary - shares one owner. The first real
	// error wins like every other in-band path.
	if eventType == "response.failed" {
		a.captureResponseFailedError(payload)
		// The failed terminal is itself the failure statement. A missing,
		// null, or empty error object therefore still classifies as an
		// upstream lifecycle failure; a provider type or a code-only object
		// already set its own classification above. The client body stays
		// untouched - this is the record view only.
		if a.errType == "" {
			a.errType = TypeUpstreamError
			a.errMsg = "upstream response failed"
		}
	}
	var ev struct {
		Response struct {
			ID                string `json:"id"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Output []struct {
				Type             string          `json:"type"`
				Role             string          `json:"role"`
				CallID           string          `json:"call_id"`
				ID               string          `json:"id"`
				Name             string          `json:"name"`
				EncryptedContent string          `json:"encrypted_content"`
				Content          json.RawMessage `json:"content"`
				Summary          []struct {
					Text string `json:"text"`
				} `json:"summary"`
			} `json:"output"`
		} `json:"response"`
	}
	parsed := json.Unmarshal(payload, &ev) == nil
	terminalUnits := 0
	terminalReasoningCount := 0
	if parsed {
		if ev.Response.ID != "" {
			a.responseID = ev.Response.ID
		}
		for _, item := range ev.Response.Output {
			typ := item.Type
			if typ == "" && item.Role != "" {
				typ = "message"
			}
			switch {
			case ResponsesToolCallItem(typ):
				// A function call may appear only in the terminal document;
				// count each call once per identity so a legitimate tool turn
				// is never classified as empty and replaced by a failure. A
				// call that was not announced by a streamed item contributes
				// the chunk that announcement would have counted.
				if a.countResponsesToolCall(item.CallID, item.ID, item.Name) {
					a.recordTerminalToolUnit(now)
				}
			case typ == "message":
				text, refusal, audioData := terminalMessageText(item.Content)
				if audioData {
					// Audio bytes in a terminal message are official output
					// even when no transcript accompanies them.
					a.responsesOutputSeen = true
				}
				if text != "" {
					a.answerishSeen = true
					// The terminal document repeats the whole answer. Count
					// every non-empty text unit here; only the units no
					// streamed text chunk already represents are added below.
					terminalUnits += terminalTextUnits(item.Content)
					if a.CapturePreview && a.preview == nil {
						a.preview = []byte(metrics.TruncatePreview(text))
					}
				} else if refusal != "" {
					// A refusal is answer-shaped output but not answer
					// content, exactly like a refusal delta: presence for
					// classification/rescue gates, never HadAnswerContent.
					a.answerishSeen = true
					if a.CapturePreview && a.preview == nil {
						a.preview = []byte(metrics.TruncatePreview(refusal))
					}
				}
			case typ == "reasoning":
				units := terminalReasoningUnits(item.Content, item.Summary)
				if units > 0 {
					a.terminalReasoningSeen = true
					terminalReasoningCount += units
				}
				if item.EncryptedContent != "" {
					a.terminalReasoningSeen = true
				}
			default:
				if typ != "" {
					a.responsesOutputSeen = true
				}
			}
		}
	}
	if a.textChunks < int64(terminalUnits) {
		a.recordTerminalAnswerUnits(now, terminalUnits-int(a.textChunks))
	}
	if a.reasoningChunks < int64(terminalReasoningCount) {
		a.recordTerminalReasoningUnits(now, terminalReasoningCount-int(a.reasoningChunks))
	}
	if a.finishReason != "" {
		return
	}
	switch eventType {
	case "response.failed":
		a.finishReason = string(ir.FinishError)
	case "response.cancelled":
		a.finishReason = string(ir.FinishCancelled)
	case "response.incomplete":
		reason := ""
		if parsed && ev.Response.IncompleteDetails != nil {
			reason = ev.Response.IncompleteDetails.Reason
		}
		switch reason {
		case "content_filter":
			a.finishReason = string(ir.FinishContentFilter)
		case "max_output_tokens":
			a.finishReason = string(ir.FinishLength)
		default:
			a.finishReason = string(ir.FinishOther)
		}
	default: // response.completed
		if a.toolCalls > 0 {
			a.finishReason = string(ir.FinishToolCalls)
		} else {
			a.finishReason = string(ir.FinishStop)
		}
	}
}

// recordTerminalAnswerUnits records the content-bearing units of a Responses
// terminal document that were not already streamed. The terminal event is the
// chunk that carried them, so it feeds the same TTFT/chunk-count accounting as
// the equivalent output_text delta. units is the number of non-empty message
// text parts left after subtracting streamed answer chunks, so a no-usage
// record keeps one chunk per unit without counting the same text twice.
func (a *Analyzer) recordTerminalAnswerUnits(now time.Time, units int) {
	if units <= 0 {
		return
	}
	if a.firstTokenAt.IsZero() {
		a.firstTokenAt = now
	}
	a.lastTokenAt = now
	a.chunkCount += int64(units)
	if a.firstAnswerAt.IsZero() {
		a.firstAnswerAt = now
	}
}

// recordTerminalReasoningUnits records reasoning units delivered only inside a
// Responses terminal document. It mirrors the streamed reasoning accounting:
// TTFT and last-token timing, one chunk per unit, and the first-reasoning
// time. Reasoning units are generation, never tool chunks.
func (a *Analyzer) recordTerminalReasoningUnits(now time.Time, units int) {
	if units <= 0 {
		return
	}
	if a.firstTokenAt.IsZero() {
		a.firstTokenAt = now
	}
	a.lastTokenAt = now
	a.chunkCount += int64(units)
	if a.firstReasoningAt.IsZero() {
		a.firstReasoningAt = now
	}
}

// recordTerminalToolUnit records a function call that appeared only in the
// terminal document. It contributes the same content-bearing tool chunk an
// announced function-call item would have: TTFT plus a chunk that is not
// generation, so the no-usage fallback matches the streamed equivalent.
func (a *Analyzer) recordTerminalToolUnit(now time.Time) {
	if a.firstTokenAt.IsZero() {
		a.firstTokenAt = now
	}
	a.lastTokenAt = now
	a.chunkCount++
	a.toolChunks++
}

// terminalTextUnits counts the non-empty answer text units in a message item's
// terminal content: one per output_text/input_text/text part, one per
// output_audio transcript, or one for a plain string. Refusal parts are
// answer-shaped but not content and never count.
func terminalTextUnits(raw json.RawMessage) int {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		if plain != "" {
			return 1
		}
		return 0
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return 0
	}
	var parts []struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Transcript string `json:"transcript"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return 0
	}
	units := 0
	for _, part := range parts {
		switch part.Type {
		case "output_text", "input_text", "text":
			if part.Text != "" {
				units++
			}
		case "output_audio":
			if part.Transcript != "" {
				units++
			}
		}
	}
	return units
}

// terminalMessageText returns the first non-empty answer text, refusal text
// and audio data presence in a message item's terminal content. The value may
// be a plain string or the documented part array. Text, refusal and audio data
// stay separate: text (including an output_audio transcript) is answer
// content, while a refusal and audio data are answer-shaped bytes.
func terminalMessageText(raw json.RawMessage) (text, refusal string, audioData bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", "", false
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain, "", false
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return "", "", false
	}
	var parts []struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Refusal    string `json:"refusal"`
		Transcript string `json:"transcript"`
		Data       string `json:"data"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", "", false
	}
	for _, part := range parts {
		switch part.Type {
		case "output_text", "input_text", "text":
			if part.Text != "" && text == "" {
				text = part.Text
			}
		case "output_audio":
			if part.Transcript != "" && text == "" {
				text = part.Transcript
			}
			if part.Data != "" {
				audioData = true
			}
		case "refusal":
			if part.Refusal != "" && refusal == "" {
				refusal = part.Refusal
			}
		}
	}
	return text, refusal, audioData
}

// terminalReasoningUnits counts the non-empty summary and reasoning_text parts
// of one terminal reasoning item. Each part is one content-bearing unit, the
// same way one streamed reasoning delta is one chunk.
func terminalReasoningUnits(content json.RawMessage, summary []struct {
	Text string `json:"text"`
}) int {
	units := 0
	for _, part := range summary {
		if part.Text != "" {
			units++
		}
	}
	if len(content) == 0 {
		return units
	}
	if !metrics.ContentPartsWithinLimit(content) {
		return units
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return units
	}
	for _, part := range parts {
		if part.Type == "reasoning_text" && part.Text != "" {
			units++
		}
	}
	return units
}

// responsesReasoningEvent matches OpenAI/xAI Responses-API reasoning stream
// events ("type":"response.reasoning_text.delta" / "...reasoning_summary_text.delta"),
// whose reasoning text rides in a generic "delta" field rather than a named
// reasoning key.
func responsesReasoningEvent(eventType string, keys *frameKeys) bool {
	switch eventType {
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return keys.deltaNonEmpty
	}
	return false
}

// responsesReasoningSummaryPart reports whether a reasoning summary part
// announcement carries visible text. The part is nested under "part", which
// the flat key probe does not classify as a reasoning channel, so a
// summary-part-only stream must still record reasoning presence (and never
// classify as an empty completion). The payload decode is scoped to this one
// documented event type.
func responsesReasoningSummaryPart(eventType string, payload []byte) bool {
	if eventType != "response.reasoning_summary_part.added" {
		return false
	}
	var ev struct {
		Part struct {
			Text string `json:"text"`
		} `json:"part"`
	}
	return json.Unmarshal(payload, &ev) == nil && ev.Part.Text != ""
}

// responsesContentEvents are the OpenAI/xAI Responses-API stream events that
// carry user-visible payload in a generic "delta" field rather than a
// chat-completions "content" key. They exist only when something IS streaming,
// so their presence marks the stream as content-bearing - this keeps the
// degenerate-outcome classifier from ever flagging a healthy Responses stream
// as a void (its terminal events carry no finish_reason at all).
func responsesContentEvent(eventType string, keys *frameKeys) bool {
	switch eventType {
	case "response.output_text.delta", "response.output_item.delta", "response.web_search_call.delta",
		"response.audio.transcript.delta":
		return keys.deltaNonEmpty
	}
	return false
}
