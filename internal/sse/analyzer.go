// Package sse provides minimal, allocation-conscious parsers for extracting
// metrics from OpenAI-compatible SSE chat completion streams. Ordinary text
// chunks are analyzed without allocations; structured metadata is decoded only
// when needed.
//
// The format we handle:
//
//	data: {"choices":[{"delta":{"content":"text"}}],"usage":{...}}
//	data: [DONE]
//
// In particular we care about:
//   - Content-bearing delta chunks (for TTFT / token timing)
//   - The [DONE] sentinel
//   - The final usage payload (injected by stream_options.include_usage)
//   - Tool calls (count of tool_calls in delta)
//   - finish_reason
//
// We locate fields without materializing every frame. Full decoding validates
// retained metadata; lexical token presence also works on data-line fragments.
// In-band error events are additionally reassembled across
// multi-line data: boundaries (bounded, steady-state allocation-free) so a
// pretty-printed error envelope is caught exactly as the client's SSE parser
// sees it - see Analyzer.eventData.
package sse

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Analyzer tracks streaming metrics across an SSE conversation. It is
// designed to be called once per newline-delimited line as bytes pass through
// the proxy, accumulating state cheaply. After the stream ends, Fill sets the
// final fields on a metrics.Record.
type Analyzer struct {
	firstTokenAt time.Time
	lastTokenAt  time.Time
	chunkCount   int64

	// Reasoning/answer separation: tracks when reasoning ("thinking") tokens
	// vs answer tokens arrive. Reasoning tokens are from reasoning_content
	// (DeepSeek R1, Qwen QwQ, etc.) or thinking blocks (Claude); answer tokens
	// are from content.
	firstReasoningAt time.Time
	firstAnswerAt    time.Time

	toolCalls    int
	toolNames    []string
	finishReason string

	// terminatorSeen records that the stream's documented end-of-stream marker
	// arrived: a [DONE] line, a non-empty finish_reason chunk, or a Responses
	// API terminal event (response.completed/failed/incomplete). A stream that
	// ends at clean EOF WITHOUT ever seeing one is a truncation.
	terminatorSeen bool
	// chatlike marks the stream as a chat-completions/Responses-API feed (a
	// payload carrying a "choices" key or a "type":"response.*" event). The
	// degenerate-outcome rules apply only to chatlike streams so a foreign SSE
	// protocol proxied through never gets a synthetic error injected.
	chatlike bool

	// toolChunks counts content-bearing chunks that carry tool-call deltas.
	// chunkCount (every content-bearing chunk: content, reasoning, AND tool
	// calls) feeds the output-token fallback when the provider sends no usage
	// blob - but tool-call argument chunks are not generation (reasoning or
	// answer) tokens, so the gen-TPS numerator must exclude them. toolChunks
	// lets Fill derive the content+reasoning-only chunk count.
	toolChunks int64

	// err captures an IN-BAND error payload (data: {"error":{...}}): some
	// providers signal failures inside a 200-OK stream instead of via the HTTP
	// status. The client sees these, so the record must too - otherwise the
	// dashboard shows a clean, empty "success" for a request that failed.
	errType, errCode, errMsg string

	// eventData accumulates the concatenated data: payloads of the CURRENT SSE
	// event. The SSE spec lets one event's data span several "data:" lines
	// (joined with "\n") and ends the event at a blank line; providers/gateways
	// that pretty-print an in-band error JSON over multiple data: lines only
	// become parseable once reassembled. A per-line scan can never catch those
	// (a live miss: coralbricks' overload error reached the client, which
	// reassembles multi-line events, while the proxy recorded a clean 200).
	// eventData is reset at each event boundary (blank line) and never grows
	// past eventDataMax - beyond that only per-line lexical scans continue.
	// General multi-line token/usage accounting is not yet supported.
	eventData []byte

	usage json.RawMessage

	// usageChunk holds the raw payload of the LAST chunk that carried a
	// "usage" object. Cost key paths are documented as relative to the
	// RESPONSE ROOT - on a stream that is the final chunk object, which may
	// carry the cost in a sibling pricing object beside usage (nano-gpt's
	// x_nanogpt_pricing) rather than inside usage itself. The usage object
	// stays the fallback root so paths written against it keep resolving.
	usageChunk []byte

	// CostKeys are optional per-provider JSON key paths for the per-request
	// cost, checked before the built-in candidates (metrics.ExtractCost). Set
	// by the proxy from config; empty = auto-detect only.
	CostKeys []string
	// UsageKeys are optional per-provider usage field-name overrides (canonical
	// field -> custom key path), checked before the built-in candidates. Set by
	// the proxy from config; empty = auto-detect only.
	UsageKeys map[string]string

	// CapturePreview gates response-content capture: when false (the default,
	// matching capture_body_preview), no message text is ever retained. Set by
	// the proxy from config.
	CapturePreview bool

	// preview captures the first N bytes of content for the dashboard's
	// response-preview view. Only the first content chunk is kept, and only
	// when CapturePreview is on.
	preview []byte

	// model and requestID from the streaming chunk, captured once.
	model string
	reqID string
}

// Feed ingests one raw SSE line (the bytes between \n delimiters, NOT
// including the trailing \n). It must be called in order for every line the
// proxy forwards. Feed returns immediately for lines that contain no
// actionable information.
func (a *Analyzer) Feed(line []byte, now time.Time) {
	// A blank line is the SSE event boundary: it terminates the event whose
	// data: lines (possibly several) we have been accumulating. Parse the
	// reassembled event for an in-band error before resetting, so a multi-line
	// pretty-printed error JSON - which a per-line scan can never decode - is
	// caught the same way the client's SSE parser sees it.
	if isBlankLine(line) {
		a.flushEvent()
		return
	}

	// Skip comments and any non-data line. Require the full "data:" prefix
	// (not just a leading 'd') so a short/garbled upstream line like "data"
	// or "d" can never slice out of range below. A non-blank, non-data line
	// (e.g. "event:"/"id:"/"retry:") is part of the current event: keep the
	// accumulated data for it, don't reset.
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}

	// Fast-path: check for the [DONE] sentinel. Tolerate "data:[DONE]" (no
	// space) and a trailing \r. Skip "data:" + one optional space, then the
	// payload must be exactly "[DONE]" (modulo \r); anything else (e.g.
	// "[DONE]extra") falls through to the normal JSON scan.
	payload := line[len("data:"):]
	if len(payload) > 0 && payload[0] == ' ' {
		payload = payload[1:]
	}
	if string(bytes.TrimRight(payload, "\r")) == "[DONE]" {
		a.terminatorSeen = true
		return
	}

	// Accumulate this data: line into the current event buffer so a multi-line
	// event (data split across lines per the SSE spec) can be parsed whole at
	// the boundary. Joined with "\n" per spec; bounded by eventDataMax - past
	// the cap only per-line lexical scans remain available for accounting.
	if len(a.eventData)+len(payload)+1 <= eventDataMax {
		if len(a.eventData) > 0 {
			a.eventData = append(a.eventData, '\n')
		}
		a.eventData = append(a.eventData, payload...)
	}
	// We only inspect content-bearing data: lines. We look for key patterns
	// within the JSON payload without fully decoding.

	// Track first/last token of any kind (content, reasoning, or tool call).
	// The first content-bearing chunk marks TTFT.
	eventType := responseEventType(payload)
	hasContent := a.tokenStart(payload) || responsesContentEvent(eventType, payload)
	hasReasoning := a.reasoningStart(payload) || responsesReasoningEvent(eventType, payload)
	toolBlock := extractContainer(jsonKey(payload, "tool_calls"), '[', ']')
	hasToolCall := len(toolBlock) > 0 && skipWS(toolBlock, 1) < len(toolBlock)-1

	// Set TTFT on the first chunk that carries real data (content, reasoning,
	// or a tool call) - not on the empty role chunk.
	if hasContent || hasReasoning || hasToolCall {
		if a.firstTokenAt.IsZero() {
			a.firstTokenAt = now
		}
		a.lastTokenAt = now
		a.chunkCount++
		// A chunk that carries ONLY a tool call (no content, no reasoning) is
		// not a generation token - track it separately so the output-token
		// fallback can exclude tool-call argument chunks from gen TPS.
		if hasToolCall && !hasContent && !hasReasoning {
			a.toolChunks++
		}
	}

	// Reasoning/answer separation for thinking models.
	if hasReasoning && a.firstReasoningAt.IsZero() {
		a.firstReasoningAt = now
	}
	if hasContent && a.firstAnswerAt.IsZero() {
		a.firstAnswerAt = now
		// Capture a short preview of the first content chunk (only when the
		// operator enabled capture_body_preview).
		if a.CapturePreview && a.preview == nil {
			a.preview = extractPreview(payload)
		}
	}

	// Count tool calls and capture their names. Each distinct tool call carries
	// an "id" in its first delta chunk; subsequent argument chunks repeat the
	// tool_calls key but do not include "id". The outer chunk `"id":"1"` is
	// excluded by searching only within the tool_calls array.
	if hasToolCall {
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(toolBlock, &calls) == nil {
			for _, call := range calls {
				if call.ID == "" {
					continue
				}
				a.toolCalls++
				if name := call.Function.Name; name != "" && !containsStr(a.toolNames, name) {
					a.toolNames = append(a.toolNames, name)
				}
			}
		}
	}

	// Capture finish_reason: "finish_reason":"stop" etc. A non-null value is a
	// documented end-of-stream marker in itself (every provider sends it only
	// on the final chunk), so it also counts as the terminator being seen.
	if fr := a.extractFinishReason(payload); fr != "" {
		a.finishReason = fr
		a.terminatorSeen = true
		// The terminal chunk is the stream's response document tail: cost /
		// usage may live here in a sibling of usage (nano-gpt's
		// x_nanogpt_pricing rides exactly this chunk). Copied once - payload
		// aliases the relay's reused line buffer.
		a.usageChunk = append(a.usageChunk[:0], payload...)
	}

	// Chat/Responses-API shape detection + Responses terminal events
	// (response.completed / response.failed / response.incomplete): the
	// Responses API has no [DONE], so those events terminate the stream.
	if !a.chatlike {
		if jsonKey(payload, "choices") != nil || strings.HasPrefix(eventType, "response.") {
			a.chatlike = true
		}
	}
	if responsesTerminalEvent(eventType) {
		a.terminatorSeen = true
		a.usageChunk = append(a.usageChunk[:0], payload...)
	}

	// Capture the model + request id from the first chunk (they're in every
	// chunk, so we only need the first).
	if a.model == "" {
		a.model = jsonStringValue(payload, "model")
	}
	if a.reqID == "" {
		a.reqID = jsonStringValue(payload, "id")
	}

	// Capture the usage blob from the final chunk. Usage appears as
	// "usage":{...} near the end of the payload. Only the last one wins.
	// The enclosing chunk is kept too (copied - payload aliases the reused
	// line buffer): cost_keys paths are relative to the response root, and
	// the cost / token counts may live in a sibling of usage.
	if raw := extractObject(jsonKey(payload, "usage")); raw != nil {
		a.usage = append(a.usage[:0], raw...)
		a.usageChunk = append(a.usageChunk[:0], payload...)
	}

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
	if metrics.HasErrorKey(payload) {
		a.captureStreamError(payload)
	}
}

// extractCost runs the cost-key scan over a raw JSON document (the chunk
// root or the bare usage object). Returns (0, false) when the bytes are
// absent or unparseable - "no cost reported", never a fabricated number.
func (a *Analyzer) extractCost(raw []byte) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return 0, false
	}
	return metrics.ExtractCostFromValue(v, a.CostKeys)
}

// flushEvent parses the accumulated multi-line event buffer for an in-band
// error, then resets it for the next event. Called at each SSE event boundary
// (a blank line) and once at stream end via Fill. A single-line event is also
// covered here (its one data: line is the whole buffer); the per-line gate in
// Feed already caught the common case, so this is the safety net for events
// whose error JSON spans multiple data: lines.
func (a *Analyzer) flushEvent() {
	if len(a.eventData) > 0 && a.errType == "" && metrics.HasErrorKey(a.eventData) {
		a.captureStreamError(a.eventData)
	}
	a.eventData = a.eventData[:0]
}

// isBlankLine reports whether the line is empty or only whitespace/CR - the
// SSE event terminator.
func isBlankLine(line []byte) bool {
	for _, b := range line {
		if b != '\r' && b != ' ' && b != '\t' {
			return false
		}
	}
	return true
}

// eventDataMax bounds the per-event accumulation buffer. A legit SSE event is
// a few KB; the proxy's own line cap (sseScannerBufMax) is 1 MiB, so this is
// comfortably above any real event while preventing an unbounded buffer from
// a hostile upstream that never sends a blank line.
const eventDataMax = 1 << 20

// captureStreamError decodes one in-band error payload into errType/errCode/
// errMsg via the canonical envelope decoder (metrics.ParseErrorEnvelope). The
// first real error wins (later frames of a failing stream are usually shutdown
// noise). Non-errors - a null/empty error value or a decoy "error" key nested
// in a normal chunk - leave no mark.
func (a *Analyzer) captureStreamError(payload []byte) {
	if a.errType != "" {
		return
	}
	typ, code, msg := metrics.ParseErrorEnvelope(payload)
	if typ == "" {
		return
	}
	a.errType, a.errCode, a.errMsg = typ, code, msg
}

func responsesTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	}
	return false
}

// Responses event types are top-level, unlike the nested type fields in
// content/tool objects. Decode only candidate Responses frames, once in Feed;
// ordinary Chat Completions content takes the cheap negative gate.
func responseEventType(payload []byte) string {
	if !bytes.Contains(payload, []byte("response.")) && !bytes.Contains(payload, []byte(`\u`)) {
		return ""
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return ""
	}
	return event.Type
}

// TerminalLine is the shared terminal-marker gate for analysis and relay
// hold release. A quoted marker in content is never an event type.
func TerminalLine(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	payload := line[len("data:"):]
	if len(payload) > 0 && payload[0] == ' ' {
		payload = payload[1:]
	}
	return string(bytes.TrimRight(payload, "\r")) == "[DONE]" ||
		responsesTerminalEvent(responseEventType(payload))
}

// Fill populates rec with the analyzer's accumulated state. Call after the
// stream has ended.
func (a *Analyzer) Fill(rec *metrics.Record) {
	// Flush a trailing event that ended at EOF without its blank-line
	// terminator (a stream can close right after the last data: line). This
	// surfaces an in-band error the provider sent as its final event.
	a.flushEvent()

	rec.FirstTokenAt = a.firstTokenAt
	rec.LastTokenAt = a.lastTokenAt
	rec.FirstAnswerAt = a.firstAnswerAt
	// The stream carried answer text content when a content chunk arrived
	// (firstAnswerAt set). This flags content PRESENCE for the tool-call-only
	// detection in FinalizeRecord - which must distinguish it from a mixed
	// content+tool_calls response on every path, streaming or not.
	rec.HadAnswerContent = !a.firstAnswerAt.IsZero()
	rec.FinishReason = a.finishReason
	rec.ToolCalls = a.toolCalls
	// Generation tokens (reasoning + answer content), excluding tool-call
	// argument chunks. When the provider sends no usage blob, chunkCount is the
	// token fallback - but it counts tool-arg chunks, which are not generation,
	// so subtract them. When usage IS present, decodeUsage fills
	// OutputTokens/ReasoningTokens below and GenTokens is finalized in
	// FinalizeRecord (reasoning + answer); leave it 0 here so we don't clash.
	if a.usage == nil && a.chunkCount > 0 {
		rec.GenTokens = a.chunkCount - a.toolChunks
	}
	if a.chunkCount > 0 && rec.Usage.OutputTokens == 0 {
		rec.Usage.OutputTokens = a.chunkCount
	}
	// Usage: the usage object when the stream carries one; otherwise the
	// terminal chunk root (nano-gpt streams have NO usage object - the token
	// counts live in the sibling pricing object, reachable via usage_keys
	// paths relative to the response root). The chunk-root decode MERGES only
	// what it actually reported, so unresolved paths never clobber the
	// chunk-count fallbacks above with zeros; when real counts land the
	// GenTokens fallback yields to FinalizeRecord's usage-based finalization
	// (the same contract as a present usage object).
	if a.usage != nil {
		decodeUsage(a.usage, rec, a.UsageKeys)
	} else if len(a.usageChunk) > 0 {
		if mergeChunkUsage(a.usageChunk, rec, a.UsageKeys) {
			rec.GenTokens = 0
		}
	}
	// Cost is read straight from the provider's response (dynamic), so it
	// reflects what the provider actually charged for this request. The cost
	// root is the usage-bearing/terminal CHUNK (the stream's response
	// document - cost_keys paths are response-root relative, and the cost may
	// sit in a sibling object like nano-gpt's x_nanogpt_pricing); the bare
	// usage object is the fallback, so paths written against usage keep
	// working (a chunk root that lacks the path falls through).
	if cost, ok := a.extractCost(a.usageChunk); ok {
		rec.Cost = cost
	} else if cost, ok := a.extractCost(a.usage); ok {
		rec.Cost = cost
	}
	if a.preview != nil && rec.ResponsePreview == "" {
		rec.ResponsePreview = string(a.preview)
	}
	if a.model != "" && rec.ProviderModel == "" {
		rec.ProviderModel = a.model
	}
	if a.reqID != "" && rec.ProviderRequestID == "" {
		rec.ProviderRequestID = a.reqID
	}
	if len(a.toolNames) > 0 {
		rec.ToolNames = a.toolNames
	}
	// An in-band error payload is the provider's own statement of failure on
	// this stream - the client received it in-band, so it takes precedence
	// over a subsequent transport-level read error (stream_read_error),
	// which may just be the connection closing after the failure was sent.
	if a.errType != "" {
		rec.ErrorType = a.errType
		rec.ErrorCode = a.errCode
		rec.ErrorMsg = a.errMsg
	}
}

// Terminated reports whether a documented end-of-stream marker was seen.
func (a *Analyzer) Terminated() bool { return a.terminatorSeen }

// HasInBandError reports whether an in-band error payload was observed (a
// provider failure streamed inside a 200 - no synthetic stream-outcome
// signaling should stack on top of the provider's own).
func (a *Analyzer) HasInBandError() bool { return a.errType != "" }

// OutcomeCode classifies the finished stream via the canonical degenerate
// rules (metrics.ClassifyOutcome - the same predicate every relay path uses).
// "" = healthy; otherwise the error code to surface (or, on the passthrough
// path, the code whose in-band error chunk replaces the held terminal event).
func (a *Analyzer) OutcomeCode() string {
	return metrics.ClassifyOutcome(a.finishReason, a.terminatorSeen, !a.firstTokenAt.IsZero(), a.toolCalls, a.chatlike)
}

// extractPreview extracts a bounded prefix (metrics.PreviewMaxBytes) of the
// first content delta's text, decoding just the content field with a
// lightweight parse. Called only when CapturePreview is enabled.
func extractPreview(payload []byte) []byte {
	text := jsonStringValue(payload, "content")
	if text == "" {
		return nil
	}
	return []byte(metrics.TruncatePreview(text))
}

// precedingBackslashes counts consecutive '\\' bytes immediately before i.
func precedingBackslashes(b []byte, i int) int {
	n := 0
	for i-n-1 >= 0 && b[i-n-1] == '\\' {
		n++
	}
	return n
}

// decodeUsage parses the "usage":{...} JSON fragment captured from the final
// chunk into rec.Usage, using the canonical dynamic parser (auto-detects
// OpenAI/Anthropic key names, honors per-provider overrides). This happens
// once per stream, not per event.
func decodeUsage(raw json.RawMessage, rec *metrics.Record, usageKeys map[string]string) {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	rec.Usage = metrics.ParseUsage(obj, usageKeys)
}

// mergeChunkUsage decodes a usage-free terminal chunk (the stream's response
// document) and folds any token counts the provider reported in a sibling
// object (via usage_keys paths) into the record - WITHOUT replacing the
// chunk-count fallbacks: only positive values are merged. Returns whether
// anything merged.
func mergeChunkUsage(raw []byte, rec *metrics.Record, usageKeys map[string]string) bool {
	var obj any
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	parsed := metrics.ParseUsage(obj, usageKeys)
	merged := false
	for _, f := range []struct{ got, dst *int64 }{
		{&parsed.InputTokens, &rec.Usage.InputTokens},
		{&parsed.OutputTokens, &rec.Usage.OutputTokens},
		{&parsed.TotalTokens, &rec.Usage.TotalTokens},
		{&parsed.CacheReadTokens, &rec.Usage.CacheReadTokens},
		{&parsed.CacheWrite, &rec.Usage.CacheWrite},
		{&parsed.ReasoningTokens, &rec.Usage.ReasoningTokens},
	} {
		if *f.got > 0 {
			*f.dst = *f.got
			merged = true
		}
	}
	return merged
}

// tokenStart reports whether the payload (a JSON line from "data: {...}")
// contains a content-bearing delta. This is a fast byte-scan; we look for the
// "content" key followed by a non-empty string value (not ""), tolerating
// optional whitespace after the colon (re-serialized payloads).
func (a *Analyzer) tokenStart(payload []byte) bool {
	value := jsonKey(payload, "content")
	return len(value) > 1 && value[0] == '"' && value[1] != '"'
}

// reasoningStart reports whether the payload carries reasoning ("thinking")
// content. Providers surface reasoning under many field names, so we match a
// family of key prefixes rather than a single hardcoded one (provider-agnostic,
// per the project rule). These are NOT answer tokens.
//
// Matched (non-empty value required):
//   - "reasoning_content"  DeepSeek R1, Qwen QwQ/Qwen3, Z.AI/GLM, Kimi, xAI legacy, LiteLLM, older vLLM
//   - "reasoning"          OpenRouter normalized, current vLLM (renamed from reasoning_content)
//   - "reasoning_details"  OpenRouter structured array, MiniMax
//   - "reasoning_text"     OpenAI/xAI Responses API (response.reasoning_text.delta)
//   - "thinking" / "thinking_delta" / "thinking_blocks"  Anthropic + LiteLLM passthrough
//   - "thought":true       Gemini generateContent part marker
//
// Deliberately NOT matched: "reasoning_effort" (a request-config echo, not a
// token) and null/empty values (Qwen/DeepSeek send "reasoning_content":"" on
// the initial role chunk - that must not mark TTFT).
func (a *Analyzer) reasoningStart(payload []byte) bool {
	return tokenHasNonEmpty(payload, "reasoning_content") ||
		tokenHasNonEmpty(payload, "reasoning_details") ||
		tokenHasNonEmpty(payload, "reasoning_text") ||
		tokenHasNonEmpty(payload, "reasoning") || // bare "reasoning" (exact key; reasoning_content etc. matched above)
		tokenHasNonEmpty(payload, "thinking") ||
		tokenHasNonEmpty(payload, "thinking_delta") ||
		tokenHasNonEmpty(payload, "thinking_blocks") ||
		geminiThoughtMarker(payload)
}

// responsesReasoningEvent matches OpenAI/xAI Responses-API reasoning stream
// events ("type":"response.reasoning_text.delta" / "...reasoning_summary_text.delta"),
// whose reasoning text rides in a generic "delta" field rather than a named
// reasoning key.
func responsesReasoningEvent(eventType string, payload []byte) bool {
	switch eventType {
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		return tokenHasNonEmpty(payload, "delta")
	}
	return false
}

// responsesContentEvents are the OpenAI/xAI Responses-API stream events that
// carry user-visible payload in a generic "delta" field rather than a
// chat-completions "content" key. They exist only when something IS streaming,
// so their presence marks the stream as content-bearing - this keeps the
// degenerate-outcome classifier from ever flagging a healthy Responses stream
// as a void (its terminal events carry no finish_reason at all).
func responsesContentEvent(eventType string, payload []byte) bool {
	switch eventType {
	case "response.output_text.delta", "response.output_item.delta", "response.web_search_call.delta", "response.function_call_arguments.delta":
		return tokenHasNonEmpty(payload, "delta")
	}
	return false
}

// skipWS advances past JSON whitespace. Some gateways re-serialize the
// upstream JSON (Python json.dumps emits "key": "value" with a space after the
// colon), so value checks must tolerate that whitespace.
func skipWS(payload []byte, i int) int {
	for i < len(payload) && (payload[i] == ' ' || payload[i] == '\t' || payload[i] == '\r' || payload[i] == '\n') {
		i++
	}
	return i
}

// tokenHasNonEmpty reports whether the payload carries `"key"` (exact key, not
// `"key_suffix"`) with a non-empty, non-null value. The value may be a string
// ("reasoning_content":"…"), an array ("reasoning_details":[…]), or an object -
// any present, non-empty value counts as a reasoning token. The key is matched
// with its closing quote, so "reasoning" does NOT also match "reasoning_content"
// / "reasoning_details" / "reasoning_effort".
func tokenHasNonEmpty(payload []byte, key string) bool {
	for value := jsonKey(payload, key); len(value) > 0; value = jsonKey(value, key) {
		switch value[0] {
		case '"':
			if len(value) > 1 && value[1] != '"' {
				return true
			}
		case '[', '{':
			i := skipWS(value, 1)
			if i < len(value) && value[i] != ']' && value[i] != '}' {
				return true
			}
		case 'n', 'f':
		default:
			return true
		}
	}
	return false
}

// geminiThoughtMarker matches Gemini's `"thought":true` part flag (tolerating
// optional whitespace after the colon).
func geminiThoughtMarker(payload []byte) bool {
	return bytes.HasPrefix(jsonKey(payload, "thought"), []byte("true"))
}

// extractFinishReason returns the finish_reason value if present.
func (a *Analyzer) extractFinishReason(payload []byte) string {
	return jsonStringValue(payload, "finish_reason")
}

// jsonStringValue extracts the string value of `"key"` from a JSON payload via
// byte-scan, tolerating optional whitespace after the colon. Returns "" when
// the key is absent or its value is not a string.
func jsonStringValue(payload []byte, key string) string {
	value := jsonKey(payload, key)
	if len(value) < 2 || value[0] != '"' {
		return ""
	}
	escaped := false
	for i := 1; i < len(value); i++ {
		switch value[i] {
		case '\\':
			escaped = true
			i++
		case '"':
			if !escaped {
				return string(value[1:i])
			}
			var text string
			if json.Unmarshal(value[:i+1], &text) == nil {
				return text
			}
			return ""
		}
	}
	return ""
}

// All analyzer field names are ASCII identifiers. The cheap negative gate
// avoids repeated lexical walks for absent fields on ordinary token chunks;
// an escaped key always reaches the shared, escape-aware lookup owner.
func jsonKey(payload []byte, key string) []byte {
	if !bytes.Contains(payload, []byte(key)) && !bytes.Contains(payload, []byte(`\u`)) {
		return nil
	}
	return metrics.JSONKey(payload, key)
}

// containsStr reports whether s is in list.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// extractObject returns the {...} portion of raw starting at the opening
// brace, reading until braces balance. It is JSON-string-aware: braces inside
// a string literal do not affect depth, and a quote is escaped iff preceded
// by an odd number of consecutive backslashes. Returns nil if raw does not
// start with '{' (e.g. "usage":null) so a non-object value is never treated
// as a usage blob.
func extractObject(raw []byte) json.RawMessage {
	return extractContainer(raw, '{', '}')
}

func extractContainer(raw []byte, open, close byte) json.RawMessage {
	if len(raw) == 0 || raw[0] != open {
		return nil
	}
	depth := 0
	inString := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			// A quote toggles string state iff not escaped (even number of
			// preceding backslashes).
			if precedingBackslashes(raw, i)%2 == 0 {
				inString = !inString
			}
		case open:
			if !inString {
				depth++
			}
		case close:
			if !inString {
				depth--
				if depth == 0 {
					return raw[:i+1]
				}
			}
		}
	}
	return raw
}
