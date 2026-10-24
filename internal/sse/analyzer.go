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
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
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
	// are from chat delta.content, a non-empty Chat audio transcript,
	// Responses content/transcript deltas, and the legacy /v1/completions
	// choices[].text field.
	firstReasoningAt time.Time
	firstAnswerAt    time.Time

	toolCalls int
	// toolNames is the retained first-seen name list and toolNameSet is its
	// O(1) membership indexes. Names are bounded by
	// metrics.RetainedToolNamesMax and Responses call ids by
	// metrics.RetainedToolCallIDsMax while the stream is still running, so a
	// hostile stream cannot grow either map without limit. Every tool call is
	// counted independently; a call past the id bound is simply not deduped.
	toolNames   []string
	toolNameSet map[string]struct{}
	toolCallIDs map[string]struct{}
	// toolCallIndexes records the Chat delta indexes already counted, so an
	// id-less first fragment and its later id-bearing continuation count once.
	// It is bounded by metrics.RetainedToolCallIDsMax like the Responses id set.
	toolCallIndexes map[int]struct{}
	finishReason    string
	// responseID is the Responses terminal document's id, retained so a
	// synthetic failure frame uses a resp_-shaped identity instead of the
	// proxy request id.
	responseID string

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
	// anthropicWire marks an Anthropic Messages feed. It owns the truncation
	// classification (a stream without message_stop or a stop_reason) but never
	// the OpenAI degenerate substitution or rescue rules.
	anthropicWire bool

	// responsesWire marks a feed routed to the Responses API before any frame
	// arrives. A zero-event, comment-only or ping-only body carries no typed
	// event to set responsesStream, so the routed wire owns its terminal shape
	// (response.failed, never an OpenAI error chunk plus [DONE]).
	responsesWire bool

	// responsesStream marks a Responses-API-shaped feed (typed "response.*"
	// events). Those events carry a per-response monotonic sequence_number
	// used to order the stream, so appending a re-send's restarted event
	// sequence would break documented ordering: the thinking rescue must
	// never append after visible Responses events. chat-completions chunks
	// have no ordering fields and accumulate independently.
	responsesStream bool

	// answerishSeen marks answer-shaped output the streaming rescue must never
	// append after, even though it is not metrics content: a refusal message
	// (the official delta field for refusals), a deprecated function_call
	// declaration, or a non-empty text/refusal part in a Responses terminal
	// document. It also feeds OutcomeCode's answer presence (a clean stop
	// after any of these is a delivered answer, not a degenerate void); it
	// never changes the record's content accounting (HadAnswerContent stays
	// content-only).
	answerishSeen bool

	// terminalReasoningSeen marks reasoning delivered only inside a Responses
	// terminal document (summary or reasoning_text). OutcomeCode treats it as
	// reasoning presence exactly like a streamed reasoning delta.
	terminalReasoningSeen bool

	// responsesOutputSeen marks a Responses output item that is neither
	// answer text, reasoning, nor a model tool call (for example a web search,
	// file search, image generation or provider-specific item), or a
	// non-empty audio data delta. It is still official output, so a completed
	// terminal carrying it must never be replaced by the empty-completion
	// failure.
	responsesOutputSeen bool

	// toolChunks counts content-bearing chunks that carry tool-call deltas.
	// chunkCount (every content-bearing chunk: content, reasoning, AND tool
	// calls) feeds the output-token fallback when the provider sends no usage
	// blob - but tool-call argument chunks are not generation (reasoning or
	// answer) tokens, so the gen-TPS numerator must exclude them. toolChunks
	// lets Fill derive the content+reasoning-only chunk count.
	toolChunks int64

	// textChunks and reasoningChunks count streamed chunks that carry answer
	// text or reasoning. The terminal Responses document repeats the whole
	// answer and reasoning, so their terminal units must count only the units
	// that no streamed chunk already represents.
	textChunks      int64
	reasoningChunks int64

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
	// eventDataSeen records that the current event already produced a data
	// line, so a skipped-accumulation first line is not mistaken for the first
	// one on a later line. eventSkipAccum is set when that first payload is a
	// complete JSON object and therefore needs no reassembly.
	eventDataSeen  bool
	eventSkipAccum bool

	usage json.RawMessage

	// usageChunk holds the raw payload of the LAST chunk that carried a
	// "usage" object. Cost key paths are documented as relative to the
	// RESPONSE ROOT - on a stream that is the final chunk object, which may
	// carry the cost in a sibling pricing object beside usage (nano-gpt's
	// x_nanogpt_pricing) rather than inside usage itself. The usage object
	// stays the fallback root so paths written against it keep resolving.
	usageChunk []byte
	// cost and costSeen retain the last payload whose cost_keys path
	// resolved. A later usage-bearing or terminal chunk without a cost must
	// not erase the provider's earlier charge.
	cost     float64
	costSeen bool
	// usageSeen records that the source actually reported usage (a usage
	// object or a provider usage-key path), so Fill's chunk-count fallback is
	// distinguishable from a reported zero.
	usageSeen bool
	// usageInvalid records that a source usage document could not be summed
	// or represented. It is sticky: a later partial document must not restore
	// a refundable count for a turn already marked unavailable. The proxy
	// wrapper turns this signal into the record's UsageInvalid state.
	usageInvalid bool

	// Anthropic streams split usage across message_start and message_delta.
	// Custom usage/cost keys may live beside the standard usage object on
	// different events, so they are folded across every usage-bearing payload
	// instead of only the last chunk root.
	anthropicCustomUsage metrics.Usage
	anthropicCustomTotal int64
	anthropicCustomSeen  bool
	anthropicCost        float64
	anthropicCostSeen    bool

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

	// Anthropic usage accumulates across message_start (input/cache) and
	// message_delta (cumulative output); the synthesized Chat-shaped usage
	// document is rebuilt after each update so Fill's decoder stays the one
	// usage owner.
	anthropicInput      int64
	anthropicOutput     int64
	anthropicCacheRead  int64
	anthropicCacheWrite int64
	anthropicThinking   int64
}

// SetWire seeds the analyzer from the routed wire protocol before any frame
// arrives. A zero-frame, comment-only or ping-only feed must classify as a
// truncated stream of that wire instead of a healthy unknown protocol, and a
// non-Chat wire must never enter the OpenAI degenerate substitution rules.
func (a *Analyzer) SetWire(p ir.Protocol) {
	switch p {
	case ir.Anthropic:
		a.anthropicWire = true
	case ir.Responses:
		a.responsesWire = true
		a.chatlike = true
	case ir.Chat, ir.Cursor:
		a.chatlike = true
	}
}

// DoneTerminates reports whether the [DONE] sentinel is this wire's
// documented end-of-stream marker. It belongs to the Chat Completions family
// (and the unknown-protocol fallback); Anthropic Messages closes with
// message_stop or a stop_reason, and the Responses API closes with a typed
// terminal event. An observed Responses event identifies the feed even when
// the route did not seed it.
func (a *Analyzer) DoneTerminates() bool {
	return !a.anthropicWire && !a.responsesWire && !a.responsesStream
}

// ResponsesFeed reports whether this stream follows the Responses API: the
// routed wire before any event arrived, or an observed typed response.* event.
// It owns the Responses terminal substitution shape and the [DONE] hold
// decision, so a zero-event Responses body still closes with response.failed.
func (a *Analyzer) ResponsesFeed() bool {
	return a.responsesWire || a.responsesStream
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
		a.flushEvent(now)
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
	// through to the normal JSON scan. Only the Chat-completions family (and
	// the unknown-protocol fallback) treats the sentinel as its terminator;
	// Responses and Anthropic Messages have their own terminal events.
	if isDonePayload(payload) {
		if a.DoneTerminates() {
			a.terminatorSeen = true
		}
		return
	}

	// Anthropic Messages events have their own taxonomy; classify them before
	// the Chat/Responses lexical scan so one analyzer owns every wire family.
	if a.feedAnthropic(payload, now) {
		return
	}
	a.feedData(payload, "", now)
}

// FeedEvent ingests one complete SSE event whose data: lines were reassembled
// at the blank-line boundary. eventType is the authoritative type resolved
// from the event's data (or its terminal event name when the document omits
// the type). It is the boundary twin of Feed, for events the relay buffers as
// a unit; a single-line event already seen by Feed is not processed twice.
func (a *Analyzer) FeedEvent(eventType string, data []byte, now time.Time) {
	if len(data) == 0 {
		return
	}
	if isDonePayload(data) {
		if a.DoneTerminates() {
			a.terminatorSeen = true
		}
		return
	}
	// A terminal marker already signed the stream: a repeated terminal event
	// is provider noise and must not rewrite the record.
	if responsesTerminalEvent(eventType) && a.terminatorSeen {
		a.flushEvent(now)
		return
	}
	// Idempotence with line-oriented detection: the same payload already
	// accumulated for this event was processed by Feed.
	if len(a.eventData) > 0 && bytes.Equal(a.eventData, data) {
		a.flushEvent(now)
		return
	}
	if a.feedAnthropic(data, now) {
		a.flushEvent(now)
		return
	}
	a.feedData(data, eventType, now)
	a.flushEvent(now)
}

// FeedBufferedEvent ingests the raw lines of one incomplete SSE event that the
// relay had to release verbatim at its hold bound. It preserves the
// reassembled data for a later FlushEventWithName, then classifies each line
// without adding it twice. Later data lines of the same event append normally
// until the real blank-line boundary. A blank line that the relay already
// released with the event is that boundary, but the relay resolves it through
// FlushEventWithName (so a remembered terminal name can still classify the
// document); feeding it here would flush and discard the reassembled data.
func (a *Analyzer) FeedBufferedEvent(raw []byte, now time.Time) {
	data := EventData(raw)
	if len(data) > eventDataMax {
		data = data[:eventDataMax]
	}
	a.eventData = append(a.eventData[:0], data...)
	a.eventDataSeen = true
	a.eventSkipAccum = true
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\n' {
			continue
		}
		if line := raw[start:i]; !isBlankLine(line) {
			a.Feed(line, now)
		}
		start = i + 1
	}
	if start < len(raw) {
		if line := raw[start:]; !isBlankLine(line) {
			a.Feed(line, now)
		}
	}
	a.eventDataSeen = true
	a.eventSkipAccum = false
}

// FlushEventWithName resolves the current event at its blank-line boundary
// using the given SSE event name. A document that omits its top-level type is
// classified by that name; a document that carries a type uses its own type.
// It never re-feeds the event's data lines.
func (a *Analyzer) FlushEventWithName(name string, now time.Time) {
	if len(a.eventData) > 0 && !a.terminatorSeen {
		typ, _ := ResolveResponsesTerminal(name, a.eventData)
		if typ != "" {
			a.terminatorSeen = true
			a.responsesStream = true
			a.usageChunk = append(a.usageChunk[:0], a.eventData...)
			a.captureCost(a.eventData)
			// The reassembled document is the whole event, so its usage
			// object may span several data: lines that no per-line scan could
			// parse. Capture it here exactly like feedData does for a
			// single-line document.
			var keys frameKeys
			keys.scan(a.eventData)
			a.captureUsage(&keys, a.eventData)
			a.captureResponsesTerminalFinish(typ, a.eventData, now)
		}
	}
	a.flushEvent(now)
}

// captureUsage records the last usage object of one payload. The enclosing
// chunk is kept too (copied - payload aliases the reused line buffer):
// cost_keys paths are relative to the response root, and cost or token counts
// may live in a sibling of usage.
func (a *Analyzer) captureUsage(keys *frameKeys, payload []byte) {
	raw := extractObject(keys.usage)
	if raw == nil {
		return
	}
	a.usage = append(a.usage[:0], raw...)
	a.usageChunk = append(a.usageChunk[:0], payload...)
	a.usageSeen = true
	a.captureCost(payload)
}

// captureCost retains the last cost-bearing payload. Cost keys are relative to
// the response root and providers may place the charge on an earlier chunk
// than the usage they finish with, so the value is captured when it resolves
// instead of re-read from whichever chunk happens to be last.
func (a *Analyzer) captureCost(payload []byte) {
	if cost, ok := a.extractCost(payload); ok {
		a.cost, a.costSeen = cost, true
	}
}

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
	hasReasoning := keys.reasoningStart() || responsesReasoningEvent(eventType, &keys) ||
		responsesReasoningSummaryPart(eventType, payload)
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
		a.terminatorSeen = true
		a.usageChunk = append(a.usageChunk[:0], payload...)
		a.captureCost(payload)
		a.captureResponsesTerminalFinish(eventType, payload, now)
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
func (a *Analyzer) flushEvent(now time.Time) {
	if len(a.eventData) > 0 && a.errType == "" {
		var keys frameKeys
		keys.scan(a.eventData)
		if keys.hasError {
			a.captureStreamError(a.eventData)
		}
	}
	// A Responses terminal document may itself span several data: lines (a
	// pretty-printed or split JSON object). No single line is a complete
	// terminal event, so resolve the reassembled document at its boundary.
	// A terminal marker already seen by the line-oriented path is not
	// processed twice.
	if len(a.eventData) > 0 && !a.terminatorSeen {
		var keys frameKeys
		keys.scan(a.eventData)
		if typ := keys.responseEventType(a.eventData); responsesTerminalEvent(typ) {
			a.terminatorSeen = true
			a.usageChunk = append(a.usageChunk[:0], a.eventData...)
			a.captureCost(a.eventData)
			a.captureResponsesTerminalFinish(typ, a.eventData, now)
		}
	}
	// A standalone error event ({"type":"error","code":...,"message":...})
	// carries no error object and never decodes line by line when it is
	// pretty-printed. Resolve the reassembled document at this same boundary
	// so its provider error is recorded and the stream stops as failed instead
	// of being re-sent as a truncation.
	if len(a.eventData) > 0 {
		a.captureStandaloneError(a.eventData)
	}
	// A pretty-printed non-terminal event carries the same visible presence as
	// its single-line twin. The summary-part announcement keeps its text under
	// "part", which the flat key probe cannot see, so a reassembled
	// announcement needs this boundary owner to record reasoning presence.
	// Only a genuine multi-line document reaches this twin: a single-line
	// event is already classified by feedData, including the one that
	// FeedBufferedEvent seeds on overflow, and counting it here would record
	// the same presence twice.
	if len(a.eventData) > 0 && bytes.IndexByte(a.eventData, '\n') >= 0 {
		var keys frameKeys
		keys.scan(a.eventData)
		if typ := keys.responseEventType(a.eventData); responsesReasoningSummaryPart(typ, a.eventData) {
			a.recordPresence(false, true, false, false, now)
		}
	}
	a.eventData = a.eventData[:0]
	a.eventDataSeen = false
	a.eventSkipAccum = false
}

// completeJSONObject reports whether payload is one whole JSON object: an
// opening brace to a closing brace with nothing but space outside. Only whole
// objects skip event reassembly; anything ambiguous keeps accumulating.
func completeJSONObject(payload []byte) bool {
	i := metrics.SkipSpace(payload, 0)
	if i >= len(payload) || payload[i] != '{' {
		return false
	}
	end := len(payload)
	for end > i+1 && (payload[end-1] == ' ' || payload[end-1] == '\t' || payload[end-1] == '\r' || payload[end-1] == '\n') {
		end--
	}
	return payload[end-1] == '}'
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

// captureStandaloneError classifies one reassembled standalone error event:
// a document whose code and message are top-level siblings of "type":"error"
// (the Responses error shape) instead of a nested error object. A per-line
// scan cannot see a pretty-printed event, and the document has no top-level
// "error" key for captureStreamError, so only the event-boundary owner can
// classify it. An error event is a terminal statement on every wire that
// carries one; the first real error still wins.
func (a *Analyzer) captureStandaloneError(payload []byte) {
	var ev struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "error" {
		return
	}
	a.recordStandaloneError(ev.Code, ev.Message)
}

// recordStandaloneError registers the top-level code/message of an error
// event that carries no nested error object. The error event itself is the
// stream's terminal statement even when it names neither field.
func (a *Analyzer) recordStandaloneError(code, message json.RawMessage) {
	a.terminatorSeen = true
	if a.errType != "" {
		return
	}
	var msg string
	_ = json.Unmarshal(message, &msg)
	codeString := errorCodeString(code)
	if codeString == "" && msg == "" {
		// The event itself is the provider's terminal statement even when it
		// names neither field (an absent or null nested object, or an empty
		// Responses error event). Treat it as an upstream failure rather than
		// leaving a healthy record behind.
		a.setStreamError(TypeUpstreamError, "", "upstream response failed")
		return
	}
	a.setStreamError("provider_error", codeString, msg)
}

// SetLocalFailure records a proxy-minted failure as an upstream_error with the
// specific internal code. The synthetic frames the proxy substitutes for a
// withheld terminal are not provider statements, so the standalone-error
// classifier must not relabel them provider_error. The first-real-error rule
// still applies: a provider error that already owns the record is never
// overwritten.
func (a *Analyzer) SetLocalFailure(code, msg string) {
	if a.errType != "" {
		return
	}
	a.setStreamError(TypeUpstreamError, code, msg)
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

// ResponsesStream reports whether the feed carried Responses-API typed events.
// A Responses stream needs its own terminal substitution rather than the
// OpenAI Chat error chunk.
func (a *Analyzer) ResponsesStream() bool { return a.responsesStream }

// ResponseID returns the Responses terminal document's id, or "" when the
// feed carried none.
func (a *Analyzer) ResponseID() string { return a.responseID }

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
	if isDonePayload(payload) || responsesTerminalEvent(responseEventType(payload)) {
		return true
	}
	// Anthropic Messages closes with message_stop. A content string that
	// happens to contain the same text is not a top-level type, so decode the
	// discriminator rather than scanning for the bytes.
	if !bytes.Contains(payload, []byte(`"type"`)) {
		return false
	}
	var ev struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(payload, &ev) == nil && ev.Type == "message_stop"
}

// Fill populates rec with the analyzer's accumulated state. Call after the
// stream has ended.
// retainToolName records one distinct tool name in first-seen order. The
// retained list and its dedupe set are bounded by RetainedToolNamesMax, so a
// hostile stream cannot grow either; every wire family feeds this owner.
func (a *Analyzer) retainToolName(name string) {
	name = metrics.RetainToolName(name)
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

func (a *Analyzer) Fill(rec *metrics.Record) {
	// Flush a trailing event that ended at EOF without its blank-line
	// terminator (a stream can close right after the last data: line). This
	// surfaces an in-band error the provider sent as its final event.
	a.flushEvent(time.Now())

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
	rec.ToolCalls = a.toolCalls
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
	fallbackOutput := rec.Usage.OutputTokens
	restoredOutput := false
	if a.usage != nil {
		decodeUsage(a.usage, rec, a.UsageKeys)
		// A present usage object may omit output_tokens (a truncated
		// Anthropic stream reported only message_start) or be the translated
		// wire's all-zero placeholder. The streamed chunks are then the only
		// output count; restore both the output and the tool-excluded
		// generation count so FinalizeRecord cannot inflate either with
		// tool-argument chunks.
		if rec.Usage.OutputTokens == 0 && fallbackOutput > 0 {
			rec.Usage.OutputTokens = fallbackOutput
			rec.GenTokens = a.chunkCount - a.toolChunks
			restoredOutput = true
		}
		// Provider usage-key paths may live beside the usage object in the
		// raw chunk root; merge them without clobbering normalized counts.
		if len(a.usageChunk) > 0 {
			if m := mergeChunkUsage(a.usageChunk, rec, a.UsageKeys); m.merged {
				if m.outputLanded {
					rec.GenTokens = 0
				}
				a.usageSeen = true
			}
		}
	} else if len(a.usageChunk) > 0 {
		if m := mergeChunkUsage(a.usageChunk, rec, a.UsageKeys); m.merged {
			if m.outputLanded {
				rec.GenTokens = 0
			}
			a.usageSeen = true
		}
	}
	if a.anthropicCustomSeen {
		m := mergeUsageValues(&rec.Usage, a.anthropicCustomUsage)
		if a.anthropicCustomTotal > 0 {
			rec.Usage.TotalTokens = a.anthropicCustomTotal
			if m.outputLanded {
				rec.GenTokens = 0
			}
			a.usageSeen = true
		} else if total, err := metrics.SumCounts(rec.Usage.InputTokens, rec.Usage.OutputTokens); err != nil {
			rec.Usage = metrics.Usage{}
			a.usageSeen = false
			a.usageInvalid = true
		} else {
			rec.Usage.TotalTokens = total
			if m.outputLanded {
				rec.GenTokens = 0
			}
			a.usageSeen = true
		}
	}
	// A restored output count is part of the turn's usage, so a provider
	// total captured before that restoration must not remain below
	// input+restored output. A custom Anthropic total captured at
	// message_start is similarly stale once a completed message_delta reports
	// output, even though that output was not "restored". The custom-total
	// merge above can overwrite the earlier value, so the consistency guard
	// runs once after every usage owner. A zero total stays zero (settlement
	// derives input+output), and a provider total at or above the sum stays
	// authoritative.
	if (restoredOutput || a.anthropicCustomTotal > 0) && rec.Usage.TotalTokens > 0 {
		if total, err := metrics.SumCounts(rec.Usage.InputTokens, rec.Usage.OutputTokens); err != nil {
			a.usageInvalid = true
		} else if rec.Usage.TotalTokens < total {
			rec.Usage.TotalTokens = total
		}
	}
	// Cost is read straight from the provider's response (dynamic), so it
	// reflects what the provider actually charged for this request. Every
	// usage-bearing or terminal chunk is scanned at capture time and the last
	// cost-bearing value is retained; a later usage-less chunk therefore
	// cannot erase an earlier charge. The bare usage object is the fallback,
	// so paths written against usage keep working.
	if a.costSeen {
		rec.Cost = a.cost
	} else if cost, ok := a.extractCost(a.usage); ok {
		rec.Cost = cost
	}
	if a.anthropicCostSeen {
		rec.Cost = a.anthropicCost
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

// UsageReported reports whether the source actually supplied usage, including
// a custom usage-key path beside the usage object. It distinguishes an
// unreported stream (whose output count is the chunk fallback) from a
// provider-reported zero.
func (a *Analyzer) UsageReported() bool { return a.usageSeen }

// UsageInvalid reports whether a source usage document could not be summed or
// represented. The signal is sticky and independent of UsageReported, so the
// proxy can invalidate the record even when a later partial document did land.
func (a *Analyzer) UsageInvalid() bool { return a.usageInvalid }

// HasInBandError reports whether an in-band error payload was observed (a
// provider failure streamed inside a 200 - no synthetic stream-outcome
// signaling should stack on top of the provider's own).
func (a *Analyzer) HasInBandError() bool { return a.errType != "" }

// TailTerminates reports whether one unterminated trailing line (the bytes
// after the last newline, without their newline) carries a completion terminal
// marker for the chatlike wires: a Chat [DONE], a non-null Chat finish_reason,
// or a Responses terminal event. It reads the same frame grammar as Feed and
// never mutates analyzer state; the relay uses it to decide whether a
// truncation-class tail is dropped or fed. Standalone in-band error terminals
// and foreign-wire terminals such as Anthropic message_stop are deliberately
// excluded: an unterminated in-band error line is dropped rather than
// delivered, so it is not a delivery terminal. A fed tail is delivered only
// when it supplies a complete answer or tool call; an empty or reasoning-only
// completion resolves through the degenerate boundary, which withholds or
// replaces the terminal bytes.
func (a *Analyzer) TailTerminates(line []byte) bool {
	payload, ok := DataPayload(line)
	if !ok {
		return false
	}
	if isDonePayload(payload) {
		return a.DoneTerminates()
	}
	var keys frameKeys
	keys.scan(payload)
	if jsonValueString(keys.finishReason) != "" {
		return true
	}
	return responsesTerminalEvent(keys.responseEventType(payload))
}

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
func (a *Analyzer) OutcomeCode() string {
	if a.anthropicWire {
		if !a.terminatorSeen {
			return metrics.CodeTruncated
		}
		return ""
	}
	hadAnswer := !a.firstAnswerAt.IsZero() || a.answerishSeen
	hadReasoning := !a.firstReasoningAt.IsZero() || a.terminalReasoningSeen
	return metrics.ClassifyOutcome(a.finishReason, a.terminatorSeen,
		hadAnswer, hadReasoning, a.responsesOutputSeen, a.toolCalls, a.chatlike)
}

// ResponsesToolCallItem reports whether one Responses output item is a model
// tool invocation. These items are tool activity even when they carry no
// function-call arguments, and they must never be classified as an empty
// completion. Provider-executed output items (web search, file search, image
// generation, code interpreter) are output presence instead. The streaming
// analyzer and the non-stream Responses decoder share this one vocabulary.
func ResponsesToolCallItem(typ string) bool {
	switch typ {
	case "function_call", "custom_tool_call", "local_shell_call", "mcp_call",
		"shell_call", "apply_patch_call", "computer_call", "tool_search_call":
		return true
	}
	return false
}

// ChatLike reports whether the analyzed feed follows the Chat/Responses
// completion contract. The relay uses it to keep OpenAI degenerate
// substitution off foreign wires.
func (a *Analyzer) ChatLike() bool { return a.chatlike }

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
// Client-disconnect gating is the caller's (it owns the record). When this
// reports true the terminal region was never opened (holding requires a seen
// terminator), so no withheld bytes are pending.
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
	a.flushEvent(time.Now())
	return a.firstTokenAt.IsZero() && a.errType == ""
}

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
	if !a.chatlike || a.terminatorSeen || a.responsesStream ||
		a.answerishSeen || a.toolCalls > 0 {
		return false
	}
	// Flush the trailing event BEFORE the in-band-error gate: a multi-line
	// provider error that never saw its blank-line boundary is discovered
	// only by the flush, and the provider's own failure statement must
	// disqualify the rescue (mirrors RetryableTruncation's ordering).
	a.flushEvent(time.Now())
	return !a.firstReasoningAt.IsZero() && a.firstAnswerAt.IsZero() && a.errType == ""
}

// RetryableReasoningStop reports whether a classified reasoning-only clean
// stop (the caller has already seen metrics.CodeReasoningOnly) may be
// transparently re-sent while its terminal region is still withheld. This is
// the stop twin of RetryableReasoningTruncation's wire-safety gates: a
// refusal or deprecated function_call delta is answer-shaped output the
// client saw, and a Responses-API feed carries per-response sequence numbers
// a re-send would restart mid-stream - neither may ever be appended behind.
// The still-withheld check (the relay's hold state) is the caller's.
func (a *Analyzer) RetryableReasoningStop() bool {
	return a.chatlike && a.terminatorSeen && a.errType == "" && !a.responsesStream &&
		!a.answerishSeen && a.toolCalls == 0
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
func (a *Analyzer) RetryableUpstreamError() (typ, code, msg string, reasoning bool) {
	defer func() { a.errRescuableFrame = false }()
	if !a.errRescuableFrame || a.terminatorSeen || a.responsesStream ||
		a.answerishSeen || a.toolCalls > 0 || !a.firstAnswerAt.IsZero() {
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
func mergeChunkUsage(raw []byte, rec *metrics.Record, usageKeys map[string]string) usageMerge {
	var obj any
	if json.Unmarshal(raw, &obj) != nil {
		return usageMerge{}
	}
	return mergeUsageValues(&rec.Usage, metrics.ParseUsage(obj, usageKeys))
}

// usageMerge reports what one merge actually landed. OutputLanded gates the
// chunk-derived GenTokens fallback: only a provider output count may displace
// it, so a custom path that reports input alone keeps the tool-excluded chunk
// count.
type usageMerge struct {
	merged       bool
	outputLanded bool
}

// mergeUsageValues folds only positive counts into dst. A custom path that
// resolves to zero never clobbers a standard count or the chunk fallback.
func mergeUsageValues(dst *metrics.Usage, src metrics.Usage) usageMerge {
	var out usageMerge
	for _, f := range []struct {
		got   int64
		dst   *int64
		field string
	}{
		{src.InputTokens, &dst.InputTokens, "input_tokens"},
		{src.OutputTokens, &dst.OutputTokens, "output_tokens"},
		{src.TotalTokens, &dst.TotalTokens, "total_tokens"},
		{src.CacheReadTokens, &dst.CacheReadTokens, "cache_read_tokens"},
		{src.CacheWrite, &dst.CacheWrite, "cache_write_tokens"},
		{src.ReasoningTokens, &dst.ReasoningTokens, "reasoning_tokens"},
	} {
		if f.got > 0 {
			*f.dst = f.got
			out.merged = true
			if f.field == "output_tokens" {
				out.outputLanded = true
			}
		}
	}
	return out
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
