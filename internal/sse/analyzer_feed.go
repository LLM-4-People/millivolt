package sse

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// feedData classifies one data payload. forcedType supplies the Responses
// event type when the document itself omits it (a terminal event named only
// on the SSE event: line).
func (a *Analyzer) feedData(payload []byte, forcedType string, now time.Time) {
	// One escape-aware pass records every key this frame classifies; the
	// predicates below read that result instead of rescanning the payload per
	// key. Results are per-frame only - nothing is cached across frames.
	var keys frameKeys
	keys.scan(payload)

	// Accumulate this data: line into the current event buffer so a multi-line
	// event (data split across lines per the SSE spec) can be parsed whole at
	// the boundary. Joined with "\n" per spec; bounded by eventDataMax - past
	// the cap only per-line lexical scans remain available for accounting.
	//
	// A first data line that is already a complete JSON object needs no
	// reassembly: the per-line gate below scans exactly those bytes for an
	// in-band error, and a complete object cannot continue into a later data
	// line as valid JSON. Skipping the copy keeps the buffer empty for the
	// ordinary one-object-per-event stream, saving a frame-sized copy and a
	// second scan per frame plus the largest-frame retention. A type-less
	// complete object that carries a top-level "response" or arrives on an
	// established Responses stream is the exception: it may be a terminal
	// document whose SSE name follows its data, so its bytes are retained for
	// the boundary classifier even when it is the first stream event.
	firstDataFrame := !a.eventDataSeen
	if firstDataFrame {
		a.eventDataSeen = true
		a.eventSkipAccum = completeJSONObject(payload) &&
			!(keys.eventType == nil && (a.responsesStream || keys.hasResponse))
	}
	if !a.eventSkipAccum && len(a.eventData)+len(payload)+1 <= eventDataMax {
		if len(a.eventData) > 0 {
			a.eventData = append(a.eventData, '\n')
		}
		a.eventData = append(a.eventData, payload...)
	}
	// We only inspect content-bearing data: lines. We look for key patterns
	// within the JSON payload without fully decoding.

	// Track first/last token of any kind (content, reasoning, or tool call).
	// The first content-bearing chunk marks TTFT.
	eventType := keys.responseEventType(payload)
	if eventType == "" {
		eventType = forcedType
	}
	hasContent := keys.contentStart() || keys.textStart() || keys.audioTranscriptStart() ||
		responsesContentEvent(eventType, &keys)
	// The generic reasoning-key scan is the Chat/Anthropic-family detector.
	// A Responses feed carries the official Response object in created and
	// completed events, and that envelope's `reasoning` echo is not output
	// presence. Keep the typed Responses owners, and run the generic scan only
	// for a non-Responses-looking frame.
	hasReasoning := responsesReasoningEvent(eventType, &keys) ||
		responsesReasoningSummaryPart(eventType, payload)
	if !a.responsesWire && !strings.HasPrefix(eventType, "response.") {
		hasReasoning = hasReasoning || keys.reasoningStart()
	}
	toolBlock := extractContainer(keys.toolCalls, '[', ']')
	hasToolCall := len(toolBlock) > 0 && metrics.SkipSpace(toolBlock, 1) < len(toolBlock)-1
	// Responses tool activity: a function call is announced by
	// response.output_item.added and its arguments stream through
	// response.function_call_arguments.delta. Both are tool activity, never
	// answer content (the latter used to set HadAnswerContent and TTFT for the
	// wrong channel).
	hasToolDelta := eventType == "response.function_call_arguments.delta" && keys.deltaNonEmpty
	// A Responses audio data delta carries output bytes, never transcript
	// text: it is official output for the degenerate classifier but not
	// answer content for HadAnswerContent/TTFT/preview.
	if eventType == "response.audio.delta" && keys.deltaNonEmpty {
		a.responsesOutputSeen = true
	}
	if eventType == "response.output_item.added" {
		var ev struct {
			Item struct {
				Type             string `json:"type"`
				CallID           string `json:"call_id"`
				ID               string `json:"id"`
				Name             string `json:"name"`
				EncryptedContent string `json:"encrypted_content"`
			} `json:"item"`
		}
		if json.Unmarshal(payload, &ev) == nil {
			switch {
			case ResponsesToolCallItem(ev.Item.Type):
				a.countResponsesToolCall(ev.Item.CallID, ev.Item.ID, ev.Item.Name)
				hasToolDelta = true
			case ev.Item.Type != "" && ev.Item.Type != "message" && ev.Item.Type != "reasoning":
				a.responsesOutputSeen = true
			}
			if ev.Item.Type == "reasoning" && ev.Item.EncryptedContent != "" {
				a.terminalReasoningSeen = true
			}
		}
	}

	// Set TTFT on the first chunk that carries real data (content, reasoning,
	// or a tool call) - not on the empty role chunk.
	a.recordPresence(hasContent, hasReasoning, hasToolCall, hasToolDelta, now)

	// Reasoning/answer separation for thinking models.
	if hasContent && a.firstAnswerAt.IsZero() {
		a.firstAnswerAt = now
		// Capture a short preview of the first content chunk (only when the
		// operator enabled capture_body_preview).
		if a.CapturePreview && a.preview == nil {
			a.preview = extractPreview(keys.content)
			if a.preview == nil {
				a.preview = extractPreview(keys.text)
			}
			if a.preview == nil {
				a.preview = extractPreview(keys.transcript)
			}
			if a.preview == nil && responsesContentEvent(eventType, &keys) {
				// Responses streams carry answer text in a top-level "delta"
				// string, not choices[].content/text.
				a.preview = extractPreview(keys.delta)
			}
		}
	}

	// Count tool calls and capture their names. A Chat tool call is identified
	// by its required delta index; the id may be omitted on the first fragment
	// (the official stream decoders synthesize one) and continuations repeat the
	// index without the id. Count one call for a new index whose fragment
	// carries a name or id, and retain a name from any fragment. A fragment with
	// no index keeps the older id-presence fallback. The outer chunk `"id":"1"`
	// is excluded by searching only within the tool_calls array. The retained
	// name and index sets are bounded while streaming, so neither can grow with
	// a hostile stream.
	if hasToolCall {
		// Decode each element independently. A malformed index in one element
		// must not discard its healthy siblings or turn the whole frame into
		// an empty tool call. The index is an exact non-negative integer;
		// 0.0 and 1e3 are valid, while a string, fraction, negative or
		// out-of-range value falls back to id presence.
		var calls []json.RawMessage
		if json.Unmarshal(toolBlock, &calls) == nil {
			for _, raw := range calls {
				var call struct {
					Index    json.RawMessage `json:"index"`
					ID       string          `json:"id"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(raw, &call) != nil {
					continue
				}
				index := 0
				hasIndex := false
				if trimmed := bytes.TrimSpace(call.Index); len(trimmed) > 0 {
					if n, err := jsonnum.Parse(trimmed); err == nil && n >= 0 {
						index, hasIndex = n, true
					}
				}
				if !hasIndex {
					a.retainToolName(call.Function.Name)
					if call.ID != "" {
						a.toolCalls++
					}
					continue
				}
				if _, counted := a.toolCallIndexes[index]; counted {
					a.retainToolName(call.Function.Name)
					continue
				}
				if call.ID == "" && call.Function.Name == "" {
					continue
				}
				if a.toolCallIndexes == nil {
					a.toolCallIndexes = make(map[int]struct{}, 4)
				}
				if len(a.toolCallIndexes) < metrics.RetainedToolCallIDsMax {
					a.toolCallIndexes[index] = struct{}{}
				}
				a.toolCalls++
				a.retainToolName(call.Function.Name)
			}
		}
	}

	// Capture finish_reason: "finish_reason":"stop" etc. A non-null value is a
	// documented end-of-stream marker in itself (every provider sends it only
	// on the final chunk), so it also counts as the terminator being seen.
	if fr := jsonValueString(keys.finishReason); fr != "" {
		a.finishReason = fr
		a.terminatorSeen = true
		// The terminal chunk is the stream's response document tail: cost /
		// usage may live here in a sibling of usage (nano-gpt's
		// x_nanogpt_pricing rides exactly this chunk). Copied once - payload
		// aliases the relay's reused line buffer.
		a.usageChunk = append(a.usageChunk[:0], payload...)
		a.captureCost(payload)
	}

	// Chat/Responses-API shape detection + Responses terminal events
	// (response.completed / response.failed / response.incomplete): the
	// Responses API has no [DONE], so those events terminate the stream.
	if !a.chatlike {
		if keys.choices != nil || strings.HasPrefix(eventType, "response.") {
			a.chatlike = true
		}
	}
	if strings.HasPrefix(eventType, "response.") {
		a.responsesStream = true
	}
	if responsesTerminalEvent(eventType) {
		a.captureTerminalEvent(eventType, payload, now)
	}
	// Refusal and deprecated function_call deltas are answer-shaped bytes:
	// a rescue appending a fresh attempt after them would duplicate visible
	// output-shaped text, so they disqualify the thinking rescue.
	if keys.refusalNonEmpty || keys.functionCallNonEmpty || keys.audioDataNonEmpty {
		a.answerishSeen = true
	}

	// Capture the model + request id from the first chunk (they're in every
	// chunk, so we only need the first).
	if a.model == "" {
		a.model = jsonValueString(keys.model)
	}
	if a.reqID == "" {
		a.reqID = jsonValueString(keys.requestID)
	}

	// Capture the usage blob from the final chunk. Usage appears as
	// "usage":{...} near the end of the payload. Only the last one wins.
	a.captureUsage(&keys, payload)

	// Capture an in-band error payload. Providers and gateways signal failures
	// mid-stream under a top-level "error" key in several shapes, all sharing
	// the same inner object:
	//   {"error":{"message":"...","type":"...","code":"..."}}      OpenAI/LiteLLM/OpenRouter
	//   {"type":"error","error":{"type":"overloaded_error",...}}   Anthropic
	//   {"error":"message"}                                        plain-string gateways
	// The client receives these chunks verbatim, so the record must reflect
	// the failure - otherwise a failed request looks like a clean, empty 200.
	// The byte-scan gate keeps the hot path allocation-free; the JSON decode
	// only runs on the rare error frame. The gate tolerates whitespace before
	// the colon ("error" :) - a re-serializing gateway may emit it.
	if keys.hasError {
		// A single-line error frame registers here, on its event's first data
		// line, while nothing of the event is on the wire yet: mark it for
		// RetryableUpstreamError so the relay may drop the frame and re-send.
		// A decoy key (null/empty error value) leaves errType empty and no
		// mark; an error on a LATER data line of a multi-line event never
		// qualifies either - its earlier bytes may already be relayed.
		hadErr := a.errType != ""
		a.captureStreamError(payload)
		if !hadErr && a.errType != "" && firstDataFrame {
			a.errRescuableFrame = true
		}
	}
}

// recordPresence applies one parsed event's content/reasoning/tool presence to
// the turn timers and chunk counters. feedData calls it per data line;
// flushEvent calls it for a reassembled event whose payload only became
// parseable at the SSE boundary, so the two paths cannot drift.
func (a *Analyzer) recordPresence(hasContent, hasReasoning, hasToolCall, hasToolDelta bool, now time.Time) {
	if hasContent || hasReasoning || hasToolCall || hasToolDelta {
		if a.firstTokenAt.IsZero() {
			a.firstTokenAt = now
		}
		a.lastTokenAt = now
		a.chunkCount++
		// A chunk that carries ONLY a tool call (no content, no reasoning) is
		// not a generation token - track it separately so the output-token
		// fallback can exclude tool-call argument chunks from gen TPS.
		if (hasToolCall || hasToolDelta) && !hasContent && !hasReasoning {
			a.toolChunks++
		}
		if hasContent {
			a.textChunks++
		}
		if hasReasoning {
			a.reasoningChunks++
		}
	}
	if hasReasoning && a.firstReasoningAt.IsZero() {
		a.firstReasoningAt = now
	}
}

// countResponsesToolCall records one Responses function_call item exactly once
// per call id. The name is retained in first-seen order like the Chat path.
// It reports whether this call is new, so the caller can add the one chunk an
// announced or terminal-only call contributes without double counting.
func (a *Analyzer) countResponsesToolCall(callID, id, name string) bool {
	// function_call and custom_tool_call carry call_id; MCPToolCall (and some
	// provider items) carry only id. Either identity dedupes the streamed
	// announcement against the terminal document; an item with neither is
	// still counted but cannot be deduped. An over-long id is keyed by its
	// digest so the set stays byte-bounded without merging distinct calls.
	key := callID
	if key == "" {
		key = id
	}
	if key != "" {
		key = metrics.RetainToolCallIDKey(key)
		if a.toolCallIDs == nil {
			a.toolCallIDs = make(map[string]struct{}, 4)
		}
		if _, seen := a.toolCallIDs[key]; seen {
			return false
		}
		if len(a.toolCallIDs) < metrics.RetainedToolCallIDsMax {
			a.toolCallIDs[key] = struct{}{}
		}
	}
	a.toolCalls++
	a.retainToolName(name)
	return true
}
