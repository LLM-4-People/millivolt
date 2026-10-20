// Package sse provides minimal, allocation-conscious parsers for extracting
// metrics from OpenAI-compatible SSE chat completion streams, plus the
// client-facing frame primitives and completion envelopes (frames.go,
// envelope.go) the proxy and its format bridges render through.
// Ordinary text chunks are analyzed without allocations; structured metadata
// is decoded only when needed.
//
// The format we handle:
//
//	data: {"choices":[{"delta":{"content":"text"}}],"usage":{...}}
//	data: {"choices":[{"text":"text"}],"usage":{...}}   (legacy /v1/completions)
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
	"math/bits"
	"slices"
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
	// are from chat delta.content, Responses content deltas, and the legacy
	// /v1/completions choices[].text field.
	firstReasoningAt time.Time
	firstAnswerAt    time.Time

	// The distinct tool calls of this stream have exactly two owners, and their
	// sum is the count (see toolCalls). The IDENTITY of a streamed tool call is
	// its array index: a client keys it by index, not by id, so
	//
	//   - two entries at different indices are two calls even when a gateway
	//     hands them the same id, and
	//   - an argument-only continuation at an index already seen is the same
	//     call, whether or not it repeats the id.
	//
	// An entry with an empty or absent id is therefore a real call whenever it
	// carries an index. An entry the provider gave NO index to (a
	// non-conforming gateway) falls back to its id, and an entry with neither
	// contributes nothing: that is the continuation shape, indistinguishable
	// from a repeat of a call already counted.
	//
	// Two producers contribute and cannot double count, because the event
	// flush owns every byte the event buffer kept (Feed's per-line decode runs
	// only for a line the buffer had to drop):
	//
	//   - countEventToolCalls, which owns the REASSEMBLED event and therefore
	//     also sees a call whose array is legal SSE spread across several data
	//     lines, plus the deprecated function_call shape that no per-line scan
	//     can complete; and
	//   - the per-line scan in Feed, which reads the RAW line and still sees a
	//     call in a data line the bounded event buffer dropped.
	//
	// Both are bounded by maxTrackedToolCalls (an index bitset plus a fallback
	// id set), so a stream's accounting is O(1) per entry and cannot grow with
	// what an upstream chooses to send - max_concurrent defaults to unlimited,
	// so per-stream work is not bounded by any operator setting.
	toolIdx     toolCallIndexSet
	toolCallIDs []string
	// toolNames is the retained first-seen name list and toolNameSet is its
	// O(1) membership index. Both are bounded by metrics.RetainedToolNamesMax
	// while the stream is running, so the record cap cannot be exceeded before
	// Fill.
	toolNames    []string
	toolNameSet  map[string]struct{}
	finishReason string

	// toolCallBytesSeen marks that at least one tool-call-bearing delta has
	// been parsed (a tool_calls entry, or a deprecated function_call delta).
	// Unlike the tool-call count, which counts DISTINCT calls, this is sticky
	// from the FIRST tool-call byte: it is what lets the relay open its
	// withhold region at the start of a tool call rather than at its commit,
	// so an uncommitted tool call is never on the wire. See
	// ToolCallRegionOpened.
	toolCallBytesSeen bool

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

	// responsesStream marks a Responses-API-shaped feed (typed "response.*"
	// events). Those events carry a per-response monotonic sequence_number
	// used to order the stream, so appending a re-send's restarted event
	// sequence would break documented ordering: the thinking rescue must
	// never append after visible Responses events. chat-completions chunks
	// have no ordering fields and accumulate independently.
	responsesStream bool

	// answerishSeen marks a delta that carried answer-shaped bytes the
	// streaming rescue must never append after, even though they are not
	// metrics content: a refusal message (the official delta field for
	// refusals) or a deprecated function_call declaration. It also feeds
	// OutcomeCode's answer presence (a clean stop after either is a delivered
	// answer, not a degenerate void); it never changes the record's content
	// accounting (HadAnswerContent stays content-only).
	answerishSeen bool

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

	// errRescuableFrame records that the in-band error registered on a data
	// line that was the FIRST data line of its SSE event (the single-line
	// frame shape every OpenAI-compatible provider uses), so at that instant
	// nothing of the error event's DATA was on the wire (field lines like
	// `event: error` may already have been relayed; the relay closes the
	// client's pending event before re-sending) and the relay may still drop
	// the frame and transparently re-send. Consumed one-shot by
	// RetryableUpstreamError; the boundary-flush (multi-line) discovery path
	// never sets it - earlier lines of such an event may already be relayed.
	errRescuableFrame bool

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

// SetChatLike seeds the chat-completions shape from the request body before
// any response frame arrives. A zero-frame, comment-only or [DONE]-only chat
// stream must still classify as a degenerate completion; without this seed the
// analyzer's chatlike flag can only be learned from a received payload.
func (a *Analyzer) SetChatLike(v bool) { a.chatlike = v }

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

	// Skip comments and any non-data line: a non-blank, non-data line
	// (e.g. "event:"/"id:"/"retry:") is part of the current event: keep the
	// accumulated data for it, don't reset.
	payload, ok := DataPayload(line)
	if !ok {
		return
	}

	// Fast-path: check for the [DONE] sentinel. Tolerate "data:[DONE]" (no
	// space) and a trailing \r. Anything else (e.g. "[DONE]extra") falls
	// through to the normal JSON scan.
	if string(bytes.TrimRight(payload, "\r")) == "[DONE]" {
		a.terminatorSeen = true
		return
	}

	// Accumulate this data: line into the current event buffer so a multi-line
	// event (data split across lines per the SSE spec) can be parsed whole at
	// the boundary. Joined with "\n" per spec; bounded by eventDataMax - past
	// the cap only per-line lexical scans remain available for accounting.
	firstDataFrame := len(a.eventData) == 0
	accumulated := len(a.eventData)+len(payload)+1 <= eventDataMax
	if !accumulated && len(a.eventData) > 0 {
		// The event buffer is full before this line. Flush what it holds now,
		// rather than letting the per-line producer for later lines fill the
		// shared tool-call bound first and make the earlier event's calls
		// uncountable. The cap is already an event boundary for reassembly.
		a.flushEvent()
		firstDataFrame = true
		accumulated = len(payload)+1 <= eventDataMax
	}
	if accumulated {
		if len(a.eventData) > 0 {
			a.eventData = append(a.eventData, '\n')
		}
		a.eventData = append(a.eventData, payload...)
	}
	// We only inspect content-bearing data: lines. We look for key patterns
	// within the JSON payload without fully decoding.

	// One escape-aware pass records every key this frame classifies; the
	// predicates below read that result instead of rescanning the payload per
	// key. Results are per-frame only - nothing is cached across frames.
	var keys frameKeys
	keys.scan(payload)

	// Track first/last token of any kind (content, reasoning, or tool call).
	// The first content-bearing chunk marks TTFT.
	eventType := keys.responseEventType(payload)
	hasContent := keys.contentStart() || keys.textStart() || responsesContentEvent(eventType, &keys)
	hasReasoning := keys.reasoningStart() || responsesReasoningEvent(eventType, &keys)
	toolValue := keys.toolCalls
	toolBlock, toolBlockComplete := extractContainer(toolValue, '[', ']')
	hasToolCall := len(toolBlock) > 0 && metrics.SkipSpace(toolBlock, 1) < len(toolBlock)-1
	if !hasToolCall && valueIsObject(toolValue) {
		// A tool_calls value that is an OBJECT is one whole entry: it carries
		// the index the call is keyed by, so it is a real call, not a malformed
		// frame. extractContainer cannot see it (it only opens on '['), so the
		// shared non-empty value test is the gate.
		hasToolCall = true
	}
	// A tool_calls array that this line does not close continues on the next
	// data line of the same event, and so does a tool-call key whose value
	// starts on the next line at all. Either way the withhold region must open
	// on THIS line: waiting would relay tool-call bytes before the region
	// existed, which is exactly the uncommitted-tool-call hazard atomic
	// withholding exists to prevent. Neither is counted as generation content
	// (hasToolCall stays false), because no complete call is known yet.
	opensToolCall := !toolBlockComplete || toolCallValueOnNextLine(payload)
	// Count the calls this line carries, but only when the event buffer had to
	// DROP it. An accumulated line belongs to the event boundary, which decodes
	// it there: only that producer can also see an array split across several
	// data lines, so decoding the same bytes here too would unmarshal one array
	// twice for no new information. This producer exists for exactly the case
	// the bounded event buffer cannot cover.
	if hasToolCall && !accumulated {
		a.registerToolCalls(toolValue)
	}

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
			a.preview = extractPreview(keys.content)
			if a.preview == nil {
				a.preview = extractPreview(keys.text)
			}
		}
	}

	// Tool-call accounting is shared by the per-line producer above and the
	// reassembled event producer in flushEvent; neither may double count an
	// event the other owns.
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
		a.terminatorSeen = true
		a.usageChunk = append(a.usageChunk[:0], payload...)
	}
	// A Responses failure states its provider error under response.error, not
	// the top-level error key the frame gate looks for. Decode it only on the
	// terminal failure event (the hot path for every other frame stays a byte
	// scan), and let the first real error win like every other in-band path.
	if eventType == "response.failed" {
		a.captureResponseFailedError(payload)
	}
	// Refusal and deprecated function_call deltas are answer-shaped bytes: a
	// rescue appending a fresh attempt after them would duplicate visible
	// output-shaped text, so they disqualify the thinking rescue. A deprecated
	// function_call is also a tool call in the older shape, so it opens the
	// withhold region exactly like a tool_calls delta does.
	deprecatedFunctionCall := keys.functionCallNonEmpty
	if keys.refusalNonEmpty || deprecatedFunctionCall {
		a.answerishSeen = true
	}
	if deprecatedFunctionCall {
		a.registerFunctionCall(jsonKey(payload, functionCallKey))
	}
	if hasToolCall || opensToolCall || deprecatedFunctionCall {
		a.toolCallBytesSeen = true
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
	// The enclosing chunk is kept too (copied - payload aliases the reused
	// line buffer): cost_keys paths are relative to the response root, and
	// the cost / token counts may live in a sibling of usage.
	if raw := extractObject(keys.usage); raw != nil {
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
// error, then counts its tool calls and resets it for the next event. Called at
// each SSE event boundary (a blank line) and once at stream end via Fill. A
// single-line event is also covered here (its one data: line is the whole
// buffer); the per-line gate in Feed already caught the common case, so this is
// the safety net for events whose error JSON spans multiple data: lines.
func (a *Analyzer) flushEvent() {
	if len(a.eventData) > 0 {
		if a.errType == "" {
			var keys frameKeys
			keys.scan(a.eventData)
			if keys.hasError {
				a.captureStreamError(a.eventData)
			}
		}
		a.countEventToolCalls()
	}
	a.eventData = a.eventData[:0]
}

// The two delta keys that carry a tool call. This list is the ONE definition of
// what millivolt counts as a tool call, shared by the per-line withhold opener
// (toolCallValueOnNextLine) and the event-level counter, so the shape that
// withholds tool-call bytes and the shape that is counted as a tool call can
// never disagree.
const (
	toolCallsKey    = "tool_calls"
	functionCallKey = "function_call"
)

// toolCallKeys is that same vocabulary as a list, for the per-line scans that
// must consider every tool-call key.
var toolCallKeys = [...]string{toolCallsKey, functionCallKey}

// deprecatedFunctionCallID is the reserved id the legacy function_call shape
// registers under. That shape streams a single OBJECT (never an array) and
// carries neither an index nor an id of its own, so every one of its argument
// deltas resolves to this one entry: the set then counts it exactly once per
// stream, which is the number of calls the legacy shape can express. The
// leading NUL cannot collide with a provider's own tool-call id.
const deprecatedFunctionCallID = "\x00function_call"

// maxTrackedToolCalls bounds how many distinct tool calls one stream's
// accounting tracks, and is the single bound on that state: past it the count
// stops growing, no further name is recorded, and a tool-call value that
// ARRIVES after the cap is not decoded at all. It exists because the proxy-stream path must stay bounded and
// cheap while max_concurrent defaults to UNLIMITED, so nothing else caps what
// one stream's accounting may allocate. It is far above any real completion
// (a client has to execute every one of these calls before its turn ends), so
// hitting it means a pathological upstream, not a shape to report exactly -
// which is why the count saturates instead of pretending to be exact. Past the
// bound the classification still works: every degenerate class it feeds needs
// only "zero calls or not".
const maxTrackedToolCalls = 1024

// toolCallIndexSet is the bounded index-keyed half of the tool-call identity
// set: one bit per index below maxTrackedToolCalls, so registering an entry is
// O(1) and the whole set is a fixed 128 bytes - no map, no growth, no linear
// rescan per entry. A fixed array (not a slice) keeps the zero-alloc path.
type toolCallIndexSet [maxTrackedToolCalls / 64]uint64

// add records index. The count is a popcount of the set rather than a running
// total, so registering an entry is a single OR with no second field to keep
// consistent.
func (s *toolCallIndexSet) add(index int) {
	s[index/64] |= 1 << uint(index%64)
}

// count returns how many indices are set.
func (s *toolCallIndexSet) count() int {
	n := 0
	for _, word := range s {
		n += bits.OnesCount64(word)
	}
	return n
}

// toolCalls reports how many DISTINCT tool calls this stream has made: the
// index-keyed set plus the id-keyed fallback for entries that carried no
// index. It is the single source of truth for that count (see Analyzer).
func (a *Analyzer) toolCalls() int { return a.toolIdx.count() + len(a.toolCallIDs) }

// registerToolCalls adds the calls a located `tool_calls` VALUE carries, and
// reports that a call is in flight when the value is an array this byte run
// does not close (a legal split array, or a frame the relay only holds
// partially). The value is the provider's array, the shape every provider
// uses; a value that is an OBJECT instead is one entry, not a malformed frame
// - a gateway that serializes a lone call that way still made the call, and an
// uncounted call classifies the stream as the empty_tool_call void, which the
// relay then answers by DESTROYING a real tool call. A value that ARRIVES past
// maxTrackedToolCalls is not decoded at all: the count cannot grow, so decoding
// it would be pure cost.
func (a *Analyzer) registerToolCalls(value []byte) (inFlight bool) {
	if a.toolCalls() >= maxTrackedToolCalls {
		return false
	}
	value = value[metrics.SkipSpace(value, 0):]
	if len(value) == 0 {
		return false
	}
	open, close := byte('['), byte(']')
	if value[0] == '{' {
		open, close = '{', '}'
	}
	block, complete := extractContainer(value, open, close)
	if !complete {
		return true
	}
	if open == '{' {
		var one toolCallEntry
		if json.Unmarshal(block, &one) == nil {
			a.registerToolCallEntry(one)
		}
		return false
	}
	var many []toolCallEntry
	if json.Unmarshal(block, &many) != nil {
		return false
	}
	for _, call := range many {
		a.registerToolCallEntry(call)
	}
	return false
}

// toolCallEntry is the one shape both tool-call producers decode.
type toolCallEntry struct {
	Index    toolCallIndex `json:"index"`
	ID       string        `json:"id"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// toolCallIndex is a streamed tool call's array index, or ABSENT. Presence has
// to be distinguishable from index 0, but a *int field would allocate one int
// per entry of every tool_calls array the analyzer decodes - and it decodes a
// whole array at a time - so the value rides in the struct and presence is a
// flag (and two int32-sized fields rather than a pointer's own allocation, so a
// decoded array costs 8 bytes per entry more, not one allocation). A malformed
// or absurd index is treated as absent, which sends the entry to the id
// fallback rather than to a fabricated identity.
type toolCallIndex struct {
	value int32
	set   bool
}

// UnmarshalJSON accepts a bare JSON integer. A non-integer (a quoted "0", a
// float, a gateway's null) is not an error: the entry simply has no index.
func (i *toolCallIndex) UnmarshalJSON(raw []byte) error {
	v, ok := parseToolCallIndex(raw)
	if !ok {
		return nil
	}
	i.value, i.set = int32(v), true
	return nil
}

// parseToolCallIndex reads raw as a decimal integer, refusing anything that is
// not one and anything at or past the tracked range: such an index is a
// malformed frame, not an identity to track, and the entry falls back to its id.
func parseToolCallIndex(raw []byte) (int, bool) {
	neg, i := false, 0
	if len(raw) > 0 && (raw[0] == '-' || raw[0] == '+') {
		neg, i = raw[0] == '-', 1
	}
	if i >= len(raw) {
		return 0, false
	}
	n := 0
	for ; i < len(raw); i++ {
		c := raw[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n >= maxTrackedToolCalls {
			return 0, false
		}
	}
	if neg {
		n = -n
	}
	return n, true
}

// registerToolCallEntry folds one decoded entry into the stream's tool-call
// set: the index is the identity, the id is the fallback for an entry that
// carries no usable index, and the name joins the record's distinct-name set.
func (a *Analyzer) registerToolCallEntry(call toolCallEntry) {
	switch {
	case call.Index.set && call.Index.value >= 0:
		// parseToolCallIndex already refused anything past the tracked range.
		a.toolIdx.add(int(call.Index.value))
	case call.ID != "":
		// No usable index: the id is the identity. Only a provider that
		// indexes nothing reaches here, so the bounded slice is enough.
		if !slices.Contains(a.toolCallIDs, call.ID) && a.toolCalls() < maxTrackedToolCalls {
			a.toolCallIDs = append(a.toolCallIDs, call.ID)
		}
	}
	a.retainToolName(call.Function.Name)
}

// registerFunctionCall counts a deprecated function_call delta as one tool
// call, from the key's VALUE (leading whitespace skipped, so a value that
// continues on the next data line of the event is read whole). A null/empty
// decoy - providers send "function_call":null on the role chunk - registers
// nothing, so it can neither count nor name a call.
func (a *Analyzer) registerFunctionCall(value []byte) {
	value = value[metrics.SkipSpace(value, 0):]
	if !valueIsNonEmpty(value) {
		return
	}
	if a.toolCalls() < maxTrackedToolCalls && !slices.Contains(a.toolCallIDs, deprecatedFunctionCallID) {
		a.toolCallIDs = append(a.toolCallIDs, deprecatedFunctionCallID)
	}
	a.retainToolName(jsonStringValue(value, "name"))
}

// retainToolName records a first-seen tool name, bounded by the same record
// cap Fill would apply. Dedupe is one map probe; once the cap is reached new
// names are dropped while call counting continues.
func (a *Analyzer) retainToolName(name string) {
	if name == "" || len(a.toolNames) >= metrics.RetainedToolNamesMax {
		return
	}
	if a.toolNameSet == nil {
		a.toolNameSet = make(map[string]struct{}, metrics.RetainedToolNamesMax)
	}
	if _, duplicate := a.toolNameSet[name]; duplicate {
		return
	}
	a.toolNameSet[name] = struct{}{}
	a.toolNames = append(a.toolNames, name)
}

// FeedFragment ingests a byte run that is NOT a whole `data:` line: a fragment
// of one line the relay's newline-free guardrail split, or the tail of one whose
// head the relay already relayed. Feed owns a whole data line and returns
// immediately for anything else, so a call whose key sits past the relay's 1 MiB
// line guardrail was invisible to the analyzer - every fragment after the first
// begins mid-JSON with no `data:` prefix. That blindness is not cosmetic: it
// left the relay believing the client held no completion-contract bytes, so a
// rescue re-sent the request behind tool-call bytes already on the wire, and a
// void class was reported for a call that was in flight.
//
// The shape test is Feed's own, key for key, so the same logical call is
// recognized whether it arrived whole or split: a non-empty `tool_calls` array, a
// lone call serialized as an object, a `function_call` with a present value, and
// - the fragment twin of a value that continues on the next data line - a
// container this run does not close. A null or empty decoy registers nothing, so
// a provider's role chunk cannot withhold a region or suppress a void class. A
// run that IS a whole data line is left to Feed, which owns those bytes and
// would otherwise decode one array twice. Nothing else is inferred from a
// fragment: content presence, TTFT, finish_reason and the chatlike gate stay
// with whole lines, because a fragment carries no event boundary to attribute
// them to.
func (a *Analyzer) FeedFragment(run []byte) {
	if _, ok := DataPayload(run); ok {
		return
	}
	forEachKeyValue(run, toolCallsKey, func(value []byte) {
		block, complete := extractContainer(value, '[', ']')
		switch {
		case len(block) > 0 && metrics.SkipSpace(block, 1) < len(block)-1:
			a.toolCallBytesSeen = true
			a.registerToolCalls(value)
		case !complete:
			// The array continues past this run: a call is in flight even
			// though no entry is complete, which is what opens the region.
			a.toolCallBytesSeen = true
		case valueIsObject(value):
			a.toolCallBytesSeen = true
			a.registerToolCalls(value)
		}
	})
	forEachKeyValue(run, functionCallKey, func(value []byte) {
		if valueIsNonEmpty(value) {
			a.toolCallBytesSeen = true
			a.registerFunctionCall(value)
		}
	})
}

// countEventToolCalls counts the tool calls in the REASSEMBLED event, and owns
// every byte the event buffer kept: Feed's per-line producer decodes only a
// line this buffer had to drop, so the same array is never decoded twice. A
// per-line scan alone cannot do this correctly, because a tool_calls array is
// legal SSE spread across several data lines of one event - and when it is, a
// per-line count misses every entry, so the relay would classify a stream that
// DID call a tool as the empty_tool_call degenerate class and substitute an
// error for a tool call it had just relayed. EVERY occurrence of a tool-call
// key is scanned, not just the shallowest: an event may legally carry two
// tool-call arrays (a gateway that drops the blank line between two tool-call
// frames merges them into one event). The outer chunk `"id":"1"` is excluded by
// reading only within each array.
func (a *Analyzer) countEventToolCalls() {
	forEachKeyValue(a.eventData, toolCallsKey, func(value []byte) {
		// An array this event does not close proves a tool call is in flight
		// even though no entry is complete. The per-line scan already opened
		// the withhold region on the line that carried the key; this is the
		// flush-time restatement for a line the event buffer never held.
		if a.registerToolCalls(value) {
			a.toolCallBytesSeen = true
		}
	})
	forEachKeyValue(a.eventData, functionCallKey, a.registerFunctionCall)
}

// forEachKeyValue calls fn with the bytes following every occurrence of
// `"key":` in raw. jsonKey returns only the shallowest match, which is not
// enough for a merged event; re-visiting a nested occurrence is harmless
// because every caller de-duplicates on a tool-call id. Like the per-line
// opener it is a bare-token scan, so a key inside a string literal (escaped
// quotes) can match: every caller then hands the value to a JSON decode, which
// rejects it, so a false positive counts nothing. An unterminated bracket in
// such a literal can still mark tool-call bytes seen - the conservative
// direction, since that only widens a withhold region.
func forEachKeyValue(raw []byte, key string, fn func(value []byte)) {
	quoted := make([]byte, 0, len(key)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, key...)
	quoted = append(quoted, '"')
	for off := 0; off+len(quoted) <= len(raw); {
		i := bytes.Index(raw[off:], quoted)
		if i < 0 {
			return
		}
		// Resume past the key itself, so every loop iteration advances.
		i += off + len(quoted)
		off = i
		colon := metrics.SkipSpace(raw, i)
		if colon >= len(raw) || raw[colon] != ':' {
			continue
		}
		fn(raw[metrics.SkipSpace(raw, colon+1):])
	}
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
	a.setStreamError(metrics.ParseErrorEnvelope(payload))
}

// captureResponseFailedError records the nested provider error of a Responses
// response.failed event. The same first-real-error rule applies.
func (a *Analyzer) captureResponseFailedError(payload []byte) {
	if a.errType != "" {
		return
	}
	a.setStreamError(metrics.ParseResponseFailedEnvelope(payload))
}

func (a *Analyzer) setStreamError(typ, code, msg string) {
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
// content/tool objects. The frame key probe records that top-level value once;
// ordinary chat frames return on the raw-prefix check without decoding.
func responseEventType(payload []byte) string {
	var keys frameKeys
	keys.scan(payload)
	return keys.responseEventType(payload)
}

// TerminalLine is the shared terminal-marker gate for analysis and relay
// hold release. A quoted marker in content is never an event type.
func TerminalLine(line []byte) bool {
	payload, ok := DataPayload(line)
	if !ok {
		return false
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
	// First reasoning-token time, persisted as absolute unix ms (0 = the
	// stream carried no reasoning block). The zero time must map to 0, never
	// to time.Time{}.UnixMilli()'s sentinel for year 1.
	if !a.firstReasoningAt.IsZero() {
		rec.FirstReasoningAt = a.firstReasoningAt.UnixMilli()
	}
	// The stream carried answer text content when a content chunk arrived
	// (firstAnswerAt set). This flags content PRESENCE for the tool-call-only
	// detection in FinalizeRecord - which must distinguish it from a mixed
	// content+tool_calls response on every path, streaming or not.
	rec.HadAnswerContent = !a.firstAnswerAt.IsZero()
	rec.FinishReason = a.finishReason
	rec.ToolCalls = a.toolCalls()
	// The raw count of content-bearing chunks of this stream, persisted as
	// its own record column; the token fallbacks below already derive from it.
	rec.Chunks = a.chunkCount
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
// Answer presence and reasoning presence are separate inputs: reasoning is
// auxiliary output, so a reasoning-only stop is its own degenerate class
// (metrics.CodeReasoningOnly), never a healthy completion and never the
// nothing-at-all void. Answer-shaped output includes content (chat
// delta.content, legacy choices[].text) and the visible non-content forms the
// rescue gates already protect - a non-empty refusal or deprecated
// function_call declaration; empty values of either are not answer output.
// "" = healthy; otherwise the error code to surface (or, on the passthrough
// path, the code whose in-band error chunk replaces the held terminal event).
//
// Tool-call bytes seen with no call countable is an UNPROVABLE count, and the
// void classes are the only ones that assert it: "the model declared a tool and
// never called it" is disproven by the call that was in flight, and "nothing
// was produced" likewise. So no void is reported for such a stream; a
// truncation still is, because it asserts only that the stream stopped, which
// is observed directly.
func (a *Analyzer) OutcomeCode() string {
	// The reassembled event is assembled here, not in Feed: it is the only
	// producer that can see a tool_calls array split across data lines, and
	// this is the one place a stream's events are all known to be over. A
	// stream whose last event never saw its blank-line boundary is still
	// covered, because the pending event is flushed below.
	a.flushEvent()
	hadAnswer := !a.firstAnswerAt.IsZero() || a.answerishSeen
	code := metrics.ClassifyOutcome(a.finishReason, a.terminatorSeen,
		hadAnswer, !a.firstReasoningAt.IsZero(), a.toolCalls(), a.chatlike)
	if code != metrics.CodeTruncated && code != "" && a.toolCallBytesSeen && a.toolCalls() == 0 {
		return ""
	}
	return code
}

// RetryableTruncation reports whether the finished stream is a truncation
// (metrics.CodeTruncated) the proxy may transparently re-send: a chatlike
// stream that hit clean EOF without any terminal marker while the client had
// received NO content-bearing chunk (no content, reasoning, or tool call)
// and no answer-shaped bytes (a refusal or deprecated function_call delta is
// visible output even though it is not metrics content) and no provider
// in-band error. The trailing event is flushed first so a final multi-line
// error frame that never saw its blank-line boundary is still caught.
// Anything the client already saw - generation content or the provider's own
// error - makes a re-send duplicate visible bytes and stays client-side.
// Client-disconnect gating is the caller's (it owns the record). This predicate
// speaks only about what the ANALYZER saw; a withheld region that overflowed its
// bound put tool-call bytes on the wire without the analyzer necessarily
// knowing, so the relay additionally refuses every rescue once it has committed
// a region (see streamBody's committed fact). A truncation CAN otherwise
// coexist with a tool-call withhold region: opening one requires no terminator.
func (a *Analyzer) RetryableTruncation() bool {
	// answerishSeen (a refusal or deprecated function_call delta) is
	// answer-shaped output the client saw: no rescue may append behind it,
	// even though it is not metrics content. Responses feeds are excluded once
	// any event was seen: their per-response sequence numbers are documented
	// ordering state, and a re-send would restart them mid-stream (the same
	// wire-safety gate RetryableReasoningTruncation applies).
	if !a.chatlike || a.terminatorSeen || a.answerishSeen || a.responsesStream {
		return false
	}
	a.flushEvent()
	return a.firstTokenAt.IsZero() && a.errType == ""
}

// ToolCallRegionOpened reports that a tool-call-bearing delta has been seen.
// Withholding a tool call from its first byte is what makes an UNCOMMITTED tool
// call impossible on the wire, which is the precondition atomic withholding
// depends on. No client generation waits for the model to say it is finished:
// every Vercel AI SDK generation checked finalizes its accumulated calls in an
// end-of-stream flush() - @ai-sdk/openai-compatible 2.x (the ai@6 generation),
// whose `hasFinished` flag is never assigned anywhere in the package, so its
// flush finalizes every call it holds; and the @ai-sdk/provider-utils 5.x
// tracker behind ai@7 and @ai-sdk/openai 4.x, whose finishToolCall is called
// from flush() and nowhere else - so both commit at the end of the response
// body, not at finish_reason. A stream that never terminates can therefore hand
// the client a call the model never finished, and appending a re-send behind it
// merges both attempts' arguments for the same tool-call index. The analyzer
// reports the fact; the relay combines it with the operator's atomic_tool_calls
// setting.
func (a *Analyzer) ToolCallRegionOpened() bool { return a.toolCallBytesSeen }

// RetryableReasoningTruncation reports whether the finished stream is a
// mid-thinking death the proxy may transparently re-send even though the
// client already saw reasoning bytes: a chat-completions-shaped stream that
// hit clean EOF or a read error without any terminal marker, carrying
// reasoning but NO answer-shaped output (no content delta, no tool call, no
// refusal or deprecated function_call) and no provider in-band error. The
// completion contract is answer content and tool calls; reasoning is
// auxiliary display text, so the fresh attempt's frames append cleanly to the
// committed SSE connection (chat chunks have no ordering fields and the
// official SDKs accumulate delta frames independently). Responses-API feeds
// are excluded: their per-response sequence numbers are documented ordering
// state, and a re-send would restart them mid-stream. finish_reason length
// never reaches here (it is a terminator, not a truncation). Client-disconnect
// gating is the caller's (it owns the record).
func (a *Analyzer) RetryableReasoningTruncation() bool {
	if !a.chatlike || a.terminatorSeen || a.responsesStream || a.answerishSeen {
		return false
	}
	// Flush BEFORE reading any event-owned fact. Tool-call accounting belongs to
	// the reassembled event, so a pending event's tool call is invisible until
	// the flush - and a multi-line provider error that never saw its
	// blank-line boundary is discovered only by the flush too, and the
	// provider's own failure statement must disqualify the rescue (mirrors
	// RetryableTruncation's ordering).
	a.flushEvent()
	return a.toolCalls() == 0 && !a.firstReasoningAt.IsZero() &&
		a.firstAnswerAt.IsZero() && a.errType == ""
}

// RetryableReasoningStop reports whether a classified reasoning-only clean
// stop (the caller has already seen metrics.CodeReasoningOnly) may be
// transparently re-sent while its terminal region is still withheld. This is
// the stop twin of RetryableReasoningTruncation's wire-safety gates: a
// refusal or deprecated function_call delta is answer-shaped output the
// client saw, and a Responses-API feed carries per-response sequence numbers
// a re-send would restart mid-stream - neither may ever be appended behind.
// The still-withheld check (the relay's hold state) is the caller's. The
// pending event is assembled here, not left to a caller's ordering: a tool call
// in the stream's last event is invisible to the count until the flush, and
// this predicate is exported, so it must be self-contained. The flush is
// idempotent once the event is empty, which is why the relay can call it after
// OutcomeCode has already run.
func (a *Analyzer) RetryableReasoningStop() bool {
	a.flushEvent()
	return a.chatlike && a.terminatorSeen && a.errType == "" && !a.responsesStream &&
		!a.answerishSeen && a.toolCalls() == 0
}

// RetryableUpstreamError reports whether the provider's in-band error was just
// discovered in a wire state where the relay may still transparently re-send
// the SAME request: the error registered on a data line that was the first
// data line of its SSE event (the single-line frame shape every
// OpenAI-compatible provider uses - the boundary-flush discovery of a
// multi-line error never qualifies, its earlier lines may already be
// relayed), the stream's terminal region never opened, and no answer-shaped
// output, content delta or tool call was relayed. A Responses-API feed is
// excluded once any of its events was seen: per-response sequence numbers
// are documented ordering state, and a re-send would restart them
// mid-stream (an error as the feed's first-ever frame is still rescuable -
// nothing was relayed). With no reasoning on the wire either, nothing
// distinguishes the delivered prefix for any remaining SSE protocol - the
// fresh attempt opens a clean new event stream, so no chat-shape gate
// applies (an error-only stream is exactly the transient-failure shape: the
// provider failing at first content). With reasoning frames already relayed
// the fresh attempt appends behind them, which is only clean behind
// chat-shaped deltas, so that variant keeps RetryableReasoningTruncation's
// shape gates and reports reasoning=true (the thinking budget owns the
// rescue, like a reasoning truncation). The report is one-shot: the first
// call consumes the mark, so a declined rescue (no budget, non-retryable
// class, client gone) can never re-fire on a later line. Error-class
// policy, budget and client-state gating stay with the caller, exactly like
// RetryableTruncation's.
//
// This is the ONE predicate the relay consults PER LINE, so it must not flush
// the pending event: a flush here would end an event at an arbitrary line,
// which is exactly where a tool_calls array (or a function_call) is legally
// still open - the client's own parser would then dispatch the orphaned prefix
// as a complete event holding a truncated JSON fragment. It does not need to.
// Feed's per-line scan registers a tool call from the RAW line as it reads it,
// so toolCalls() is already current for the single-line frame shape this
// predicate gates on; a call only the reassembled event can reveal is one the
// per-line scan saw as an opener (toolCallBytesSeen) and the relay already
// withholds, and an opener alone is not a relayed tool call.
func (a *Analyzer) RetryableUpstreamError() (typ, code, msg string, reasoning bool) {
	defer func() { a.errRescuableFrame = false }()
	if !a.errRescuableFrame || a.terminatorSeen || a.responsesStream ||
		a.answerishSeen || a.toolCalls() > 0 || !a.firstAnswerAt.IsZero() {
		return "", "", "", false
	}
	if !a.firstReasoningAt.IsZero() {
		if !a.chatlike {
			return "", "", "", false
		}
		return a.errType, a.errCode, a.errMsg, true
	}
	return a.errType, a.errCode, a.errMsg, false
}

// extractPreview extracts a bounded prefix (metrics.PreviewMaxBytes) of the
// first content delta's text from the probe's first content value, decoding
// just that value with a lightweight parse. Called only when CapturePreview is
// enabled.
func extractPreview(value []byte) []byte {
	text := jsonValueString(value)
	if text == "" {
		return nil
	}
	return []byte(metrics.TruncatePreview(text))
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

// responsesContentEvents are the OpenAI/xAI Responses-API stream events that
// carry user-visible payload in a generic "delta" field rather than a
// chat-completions "content" key. They exist only when something IS streaming,
// so their presence marks the stream as content-bearing - this keeps the
// degenerate-outcome classifier from ever flagging a healthy Responses stream
// as a void (its terminal events carry no finish_reason at all).
func responsesContentEvent(eventType string, keys *frameKeys) bool {
	switch eventType {
	case "response.output_text.delta", "response.output_item.delta", "response.web_search_call.delta", "response.function_call_arguments.delta":
		return keys.deltaNonEmpty
	}
	return false
}

// jsonValueString extracts the string text from a raw JSON value (the probe's
// value slice, already positioned after the colon and any whitespace). Returns
// "" when the value is not a string. Unescaped values are copied once; escaped
// values are decoded by encoding/json so every legal escape survives.
func jsonValueString(value []byte) string {
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

// tokenHasNonEmpty reports whether the payload carries `"key"` (exact key, not
// `"key_suffix"`) with a non-empty, non-null value. The value may be a string
// ("reasoning_content":"…"), an array ("reasoning_details":[…]), or an object -
// any present, non-empty value counts as a reasoning token. The key is matched
// with its closing quote, so "reasoning" does NOT also match "reasoning_content"
// / "reasoning_details" / "reasoning_effort".
func tokenHasNonEmpty(payload []byte, key string) bool {
	for value := jsonKey(payload, key); len(value) > 0; value = jsonKey(value, key) {
		if valueIsNonEmpty(value) {
			return true
		}
	}
	return false
}

// valueIsNonEmpty reports whether a located JSON value is a present, non-empty
// one. The value may be a string ("reasoning_content":"…"), an array
// ("reasoning_details":[…]) or an object - any present, non-empty value counts
// as content. A null/false decoy and an empty string/array/object do not, which
// is what keeps a provider's "function_call":null role chunk from counting as a
// tool call.
func valueIsNonEmpty(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	switch value[0] {
	case '"':
		return len(value) > 1 && value[1] != '"'
	case '[', '{':
		i := metrics.SkipSpace(value, 1)
		return i < len(value) && value[i] != ']' && value[i] != '}'
	case 'n', 'f':
		return false
	default:
		return true
	}
}

// valueIsObject reports whether a located JSON value is a present, non-empty
// OBJECT. It is the gate for the tool_calls-as-object shape, which
// extractContainer cannot see: that helper only opens on the array delimiter,
// so a lone call serialized as an object would otherwise read as an absent
// value - uncounted, and with no withhold region.
func valueIsObject(value []byte) bool {
	return len(value) > 0 && value[0] == '{' && valueIsNonEmpty(value)
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
	return jsonValueString(jsonKey(payload, key))
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

// toolCallValueOnNextLine reports that this data line ends immediately after a
// tool-call key and its colon, so the value's first byte is on the NEXT data
// line of the same event. JSONKey cannot see this: it returns nil both for "no
// such key" and for "key present, value not on this line", and the line that
// does carry the value body has no key in it at all. Without this the withhold
// region would open one line late, after tool-call bytes reached the client -
// and the same logical shape would be withheld or not depending on where the
// line break happened to fall. It walks the shared tool-call key list, so every
// shape that is counted is also a shape that withholds.
// The bare-token scan cannot false-positive on content text, because a JSON
// string can only contain the key with escaped quotes ("\"tool_calls\"").
func toolCallValueOnNextLine(payload []byte) bool {
	for _, key := range toolCallKeys {
		if keyValueOnNextLine(payload, key) {
			return true
		}
	}
	return false
}

// keyValueOnNextLine is toolCallValueOnNextLine's single-key half: does raw end
// immediately after `"key"` and its colon?
func keyValueOnNextLine(raw []byte, key string) bool {
	quoted := make([]byte, 0, len(key)+2)
	quoted = append(quoted, '"')
	quoted = append(quoted, key...)
	quoted = append(quoted, '"')
	i := bytes.Index(raw, quoted)
	if i < 0 {
		return false
	}
	j := metrics.SkipSpace(raw, i+len(quoted))
	if j >= len(raw) || raw[j] != ':' {
		return false
	}
	return metrics.SkipSpace(raw, j+1) >= len(raw)
}

// extractObject returns the {...} portion of raw starting at the opening
// brace, reading until braces balance. It is JSON-string-aware: braces inside
// a string literal do not affect depth, and a quote is escaped iff preceded
// by an odd number of consecutive backslashes. Returns nil if raw does not
// start with '{' (e.g. "usage":null) so a non-object value is never treated
// as a usage blob.
func extractObject(raw []byte) json.RawMessage {
	blob, _ := extractContainer(raw, '{', '}')
	return blob
}

// extractContainer returns the {...} / [...] portion of raw starting at the
// opening delimiter, reading until the delimiters balance, plus whether this
// line actually CLOSED it. It is JSON-string-aware: delimiters inside a string
// literal do not affect depth, and a quote is escaped iff preceded by an odd
// number of consecutive backslashes. Returns nil if raw does not start with the
// opening delimiter (e.g. "usage":null) so a non-container value is never
// treated as a blob. complete=false means the container continues on a later
// data line of the same SSE event, which is legal SSE and which callers that
// gate on "does this line carry a tool call" must not mistake for "no tool
// call": the per-line scan sees an unterminated prefix, not an absent value.
func extractContainer(raw []byte, open, close byte) (json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != open {
		return nil, true
	}
	// One forward pass, tracking escape parity as it goes rather than
	// re-walking the backslashes before every quote: this scan runs over a whole
	// tool_calls array, which on a long call is the largest thing the analyzer
	// ever looks at.
	depth := 0
	inString, escaped := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return raw[:i+1], true
			}
		}
	}
	return raw, false
}
