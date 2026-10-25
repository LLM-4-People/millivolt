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
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
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
			a.captureTerminalEvent(typ, a.eventData, now)
		}
	}
	a.flushEvent(now)
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
			a.captureTerminalEvent(typ, a.eventData, now)
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
