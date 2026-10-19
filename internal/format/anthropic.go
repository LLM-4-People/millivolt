// Package format translates between OpenAI Chat Completions and native
// provider wire formats. It is used only when a client explicitly opts in
// (X-Proxy-Format header); the default proxy path is byte-transparent.
package format

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

const (
	// sseLineBufInit / sseLineBufMax bound a single SSE event line while
	// scanning an upstream Anthropic stream (initial buffer / hard cap). The
	// small initial buffer fits the ordinary event and grows on demand, so a
	// stream that never exceeds it retains 4 KiB instead of 64. Safety
	// guardrail against pathological lines, not user-tunable.
	sseLineBufInit = 4 * 1024
	sseLineBufMax  = 1024 * 1024

	// nativeIdentityMaxBytes bounds provider-controlled identity strings
	// (message id/model and tool-call id/name) at the translation boundary
	// before they are echoed into every emitted frame. It matches the record
	// retention bound for provider text and model names (512 bytes), so a
	// normal identifier passes unchanged while a body-sized value cannot
	// multiply across a stream. Internal safety guardrail, not user-tunable.
	nativeIdentityMaxBytes = 512

	// nativeToolBlocksMax bounds the concurrent tool_use blocks tracked while
	// translating one Anthropic stream. Each open block retains its initial
	// JSON until its stop; real parallel tool calls are single-digit, so 1024
	// leaves ample headroom while failing a stream that opens unique blocks
	// without ever closing them. Internal safety guardrail, not user-tunable.
	nativeToolBlocksMax = 1024

	// nativeToolInitialBytesMax bounds the total initial input JSON retained
	// by the currently open tool blocks. Each start may carry up to one SSE
	// line (sseLineBufMax), so the block count alone would allow a gigabyte of
	// retained strings; normal starts carry {} or a small inline object and
	// large arguments stream through input_json_delta. Internal safety
	// guardrail, not user-tunable.
	nativeToolInitialBytesMax = 4 << 20

	// wireJSONSourceByteMax is the largest encoded size of one accepted
	// source byte after translation: a raw invalid UTF-8 byte becomes the
	// three-byte U+FFFD replacement, U+2028/U+2029 expand from three bytes to
	// six, and a C0 control only appears in accepted JSON as an escape whose
	// six-byte output spelling is no larger than its six-byte source spelling.
	// sse.MarshalWireJSON disables HTML escaping, so <, > and & stay one byte.
	wireJSONSourceByteMax = 3

	// wireJSONDecodedByteMax is the largest wire encoding of one decoded
	// identity byte: a C0 control becomes a six-byte \u00XX escape.
	wireJSONDecodedByteMax = 6

	// nativeStreamFrameEnvelopeMax covers the fixed chunk-envelope text
	// around one event: the id/object/created/model skeleton, one
	// choice/delta/finish skeleton, and the largest fixed extra object (usage
	// or error keys). Provider-controlled text inside those objects is
	// counted separately.
	nativeStreamFrameEnvelopeMax = 2 * 1024

	// NativeStreamFrameMaxBytes bounds one SSE data line emitted by
	// StreamToOpenAI from a source line the translator accepts. The scanner
	// admits at most sseLineBufMax bytes per source line; re-encoding a
	// decoded string or retained raw tool input can at most triple each
	// source byte, and the retained message id/model are echoed into every
	// chunk, each bounded at nativeIdentityMaxBytes and escaped to at most
	// wireJSONDecodedByteMax bytes per byte. The envelope headroom covers the
	// fixed chunk skeleton around those values. The proxy's translated relay
	// sizes its line scanner from this bound, so every accepted source frame
	// relays instead of failing as a translation_error. Safety guardrail, not
	// user-tunable.
	NativeStreamFrameMaxBytes = wireJSONSourceByteMax*sseLineBufMax +
		2*wireJSONDecodedByteMax*nativeIdentityMaxBytes + nativeStreamFrameEnvelopeMax
)

var (
	// errNativeIdentityTooLong is the stable fail-closed reason for an
	// over-bound provider identity field.
	errNativeIdentityTooLong = errors.New("anthropic: identity field exceeds limit")
	// errNativeToolBlocks is the stable fail-closed reason for opening more
	// concurrent tool blocks than nativeToolBlocksMax.
	errNativeToolBlocks = errors.New("anthropic: concurrent tool block limit exceeded")
	// errNativeToolInitialBytes is the stable fail-closed reason for retaining
	// more initial tool input than nativeToolInitialBytesMax across open blocks.
	errNativeToolInitialBytes = errors.New("anthropic: retained tool input limit exceeded")
	// errNativeContentParts is the stable fail-closed reason for a response
	// content-block array over metrics.ContentPartMax.
	errNativeContentParts = errors.New("anthropic: content block limit exceeded")
)

// checkNativeIdentity enforces nativeIdentityMaxBytes on one provider-controlled
// identity value before the translator echoes or retains it.
func checkNativeIdentity(field, value string) error {
	if len(value) <= nativeIdentityMaxBytes {
		return nil
	}
	return fmt.Errorf("%w (%s is %d bytes, limit %d)",
		errNativeIdentityTooLong, field, len(value), nativeIdentityMaxBytes)
}

// MergeTokenCaps owns the token-ceiling precedence of the OpenAI wire's cap
// pair: max_completion_tokens wins over max_tokens (the pair's documented
// semantics), and neither present means no ceiling. TranslateRequest composes
// it with its deref and the configured default (Anthropic carries exactly
// one max_tokens upstream), and the proxy's request decodes call it too, so
// every reader of the pair resolves the same ceiling.
func MergeTokenCaps(maxCompletion, maxTokens *int) *int {
	switch {
	case maxCompletion != nil:
		return maxCompletion
	case maxTokens != nil:
		return maxTokens
	}
	return nil
}

// TranslateRequest converts an OpenAI Chat Completions request body into the
// Anthropic Messages request body. Unknown/unsupported fields are dropped;
// the goal is a working translation for the common case, not lossless
// fidelity. defaultMaxTokens is sent when the client provides neither
// max_tokens nor max_completion_tokens (Anthropic requires max_tokens); it
// comes from config. promptCacheKey, when non-empty, is injected as
// Anthropic's native top-level prompt_cache_key field - the proxy's
// sub-conversation tracking value, extracted from the client's own spelling
// before translation (the translator's allowlist would drop that spelling).
// The whole body must be valid UTF-8: a raw invalid byte is rejected before the
// typed decode so the standard decoder's three-byte U+FFFD replacement cannot
// expand a body-sized string in memory.
func TranslateRequest(openaiBody []byte, defaultMaxTokens int, promptCacheKey string) ([]byte, error) {
	// Reject raw invalid UTF-8 before the typed decode. Valid UTF-8 is the
	// decoded-output bound: every decoded string is no larger than its raw
	// spelling, so the typed decode cannot grow the request. The standard
	// decoder would instead replace each invalid byte with U+FFFD (three
	// bytes), tripling a body-sized string here and again while the translated
	// body is re-encoded. The proxy's metadata boundary rejects the same input
	// before translation; this keeps direct callers of the translator bounded
	// too. Escaped lone surrogates stay accepted: their raw bytes are ASCII and
	// their decoded form is already one replacement rune.
	if !utf8.Valid(openaiBody) {
		return nil, errParseOpenAIBody(errors.New("request body is not valid UTF-8"))
	}
	var in struct {
		Model              string          `json:"model"`
		Messages           []openaiMessage `json:"messages"`
		MaxTokens          *int            `json:"max_tokens"`
		MaxCompletionToken *int            `json:"max_completion_tokens"`
		Stream             bool            `json:"stream"`
		Temperature        *float64        `json:"temperature"`
		TopP               *float64        `json:"top_p"`
		Stop               json.RawMessage `json:"stop"`
		Tools              []openaiTool    `json:"tools"`
		ToolChoice         json.RawMessage `json:"tool_choice"`
		System             string          `json:"system"`
		ResponseFormat     json.RawMessage `json:"response_format"`
	}
	if err := json.Unmarshal(openaiBody, &in); err != nil {
		return nil, errParseOpenAIBody(err)
	}

	var system strings.Builder
	system.WriteString(in.System)
	var messages []anthropicMessage
	for _, m := range in.Messages {
		// Extract system messages into the top-level system field.
		if m.Role == "system" || m.Role == "developer" {
			if s, ok := m.ContentText(); ok {
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(s)
			}
			continue
		}
		am, err := m.toAnthropic()
		if err != nil {
			return nil, err
		}
		messages = append(messages, am)
	}

	out := map[string]any{
		"model":    in.Model,
		"messages": messages,
		"stream":   in.Stream,
	}
	// max_tokens is required by Anthropic; prefer max_completion_tokens and
	// inject a sane default when the client sent neither.
	if cap := MergeTokenCaps(in.MaxCompletionToken, in.MaxTokens); cap != nil {
		out["max_tokens"] = *cap
	} else {
		out["max_tokens"] = defaultMaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if system.Len() > 0 {
		out["system"] = system.String()
	}
	if stops := parseStop(in.Stop); len(stops) > 0 {
		out["stop_sequences"] = stops
	}
	if len(in.Tools) > 0 {
		tools := make([]anthropicTool, 0, len(in.Tools))
		for _, t := range in.Tools {
			tools = append(tools, t.toAnthropic())
		}
		out["tools"] = tools
		if choice := parseToolChoice(in.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if promptCacheKey != "" {
		out["prompt_cache_key"] = promptCacheKey
	}
	return sse.MarshalWireJSON(out)
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	Name       string           `json:"name"`
	ToolCalls  []openaiToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ContentString returns the message content as a single string when the
// content is a plain string (the common case).
func (m openaiMessage) ContentString() (string, bool) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, true
	}
	return "", false
}

// ContentText accepts both the string form and the OpenAI content-array form,
// joining text parts in order. Other part types are ignored rather than
// stringified; callers that require structured content use the raw message.
// An array over metrics.ContentPartMax is skipped rather than decoded, so one
// hostile message cannot expand into a body-sized typed slice.
func (m openaiMessage) ContentText() (string, bool) {
	if !metrics.ContentPartsWithinLimit(m.Content) {
		return "", false
	}
	if s, ok := m.ContentString(); ok {
		return s, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(part.Text)
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

func (m openaiMessage) toAnthropic() (anthropicMessage, error) {
	switch m.Role {
	case "user":
		// Content may be a string or an array of content parts; pass through
		// as-is (Anthropic accepts both for user messages).
		return anthropicMessage{Role: "user", Content: rawOrString(m.Content)}, nil
	case "assistant":
		// Assistant messages with tool calls map to content blocks.
		if len(m.ToolCalls) > 0 {
			blocks := []map[string]any{}
			// ContentText owns both accepted history shapes: the plain string
			// and the bounded content-array text parts.
			if s, ok := m.ContentText(); ok && s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" || args == "null" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return anthropicMessage{}, errParseOpenAIBody(errors.New("tool call arguments must be valid JSON"))
				}
				// Anthropic's ToolUseBlock.input is an object. A valid JSON
				// array/string/number/bool would be forwarded as an invalid
				// upstream request, so reject it in the request translator.
				if args[0] != '{' {
					return anthropicMessage{}, errParseOpenAIBody(errors.New("tool call arguments must be a JSON object"))
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": json.RawMessage(args),
				})
			}
			return anthropicMessage{Role: "assistant", Content: blocks}, nil
		}
		return anthropicMessage{Role: "assistant", Content: rawOrString(m.Content)}, nil
	case "tool", "function":
		// A tool result becomes a user message with a tool_result block.
		s, _ := m.ContentText()
		block := map[string]any{
			"type":        "tool_result",
			"tool_use_id": m.ToolCallID,
			"content":     s,
		}
		return anthropicMessage{Role: "user", Content: []any{block}}, nil
	default:
		return anthropicMessage{Role: "user", Content: rawOrString(m.Content)}, nil
	}
}

func rawOrString(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return json.RawMessage(raw)
}

func (t openaiTool) toAnthropic() anthropicTool {
	schema := t.Function.Parameters
	if len(schema) == 0 || string(schema) == "null" {
		schema = json.RawMessage(`{}`)
	}
	return anthropicTool{
		Name:        t.Function.Name,
		Description: t.Function.Description,
		InputSchema: schema,
	}
}

func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

// parseToolChoice maps OpenAI's tool_choice shapes to Anthropic's. The typed
// decode retains only function.name: unknown, wide, or deeply nested members
// are skipped instead of being materialized as map[string]any per node, so a
// body-sized tool_choice cannot amplify the translation. Non-string name
// values keep their original JSON rather than decoding an unbounded value
// tree; the name is the only member the translator ever reads.
func parseToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var shorthand string
	if err := json.Unmarshal(raw, &shorthand); err == nil {
		switch shorthand {
		case "auto", "none":
			return map[string]any{"type": shorthand}
		case "required":
			return map[string]any{"type": "any"}
		}
		return nil
	}
	var obj struct {
		Function *struct {
			Name json.RawMessage `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Function == nil {
		return nil
	}
	return map[string]any{"type": "tool", "name": toolChoiceName(obj.Function.Name)}
}

// toolChoiceName preserves the established tool_choice name behavior: absent
// or null becomes nil, a JSON string becomes its Go string, and any other
// JSON value passes through verbatim without materializing its tree.
func toolChoiceName(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return s
	}
	return raw
}

// anthropicErrorEvent is Anthropic's error payload shape as both surfaces
// carry it: the non-streaming error body ({"type":"error","error":{…}}) and
// the SSE "error" event ({"error":{…}}). One owner of the anonymous
// {error:{type,message}} decode and its OpenAI-style re-render. Deliberately
// not metrics.ParseErrorEnvelope: that is the OpenAI/compatible envelope at
// the record trust boundary, a different contract from the provider wire
// shape decoded here.
type anthropicErrorEvent struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// openAIError re-renders the decoded event as the OpenAI error envelope
// object ({"error":{"message":…,"type":…}}) both surfaces emit.
func (e anthropicErrorEvent) openAIError() map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": e.Error.Message,
			"type":    e.Error.Type,
		},
	}
}

// nativeContentBlock is one Anthropic Messages response content block, the
// only provider response field whose array can expand into a typed slice.
type nativeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// nativeContentBlocks bounds the response block array before materializing it.
// The request-composition limit is reused because a provider response is the
// same untrusted-array shape: a body of tiny empty objects must not expand into
// hundreds of MiB of typed blocks.
type nativeContentBlocks []nativeContentBlock

func (b *nativeContentBlocks) UnmarshalJSON(raw []byte) error {
	if !metrics.ContentPartsWithinLimit(raw) {
		return fmt.Errorf("%w (limit %d parts)", errNativeContentParts, metrics.ContentPartMax)
	}
	type blocks []nativeContentBlock
	return json.Unmarshal(raw, (*blocks)(b))
}

// TranslateResponse converts an Anthropic Messages (non-streaming) response
// into an OpenAI Chat Completions response. An Anthropic error body
// ({"type":"error",...}) is passed through as a JSON object with an error key.
func TranslateResponse(anthropicBody []byte) ([]byte, error) {
	// Detect Anthropic error bodies and surface them as OpenAI-style errors.
	var errProbe struct {
		Type string `json:"type"`
		anthropicErrorEvent
	}
	if json.Unmarshal(anthropicBody, &errProbe) == nil && errProbe.Type == "error" {
		return sse.MarshalWireJSON(errProbe.openAIError())
	}

	var in struct {
		ID         string              `json:"id"`
		Model      string              `json:"model"`
		Role       string              `json:"role"`
		Content    nativeContentBlocks `json:"content"`
		StopReason string              `json:"stop_reason"`
		Usage      nativeUsage         `json:"usage"`
	}
	if err := json.Unmarshal(anthropicBody, &in); err != nil {
		return nil, fmt.Errorf("parse anthropic body: %w", err)
	}
	// Fail closed: a 200 body that isn't a recognizable Anthropic Messages
	// response must not be fabricated into an empty success. Require at least
	// one Anthropic-specific marker - content blocks or a stop reason (an "id"
	// alone is not enough: an OpenAI-shaped body also carries one).
	if len(in.Content) == 0 && in.StopReason == "" {
		return nil, fmt.Errorf("not an anthropic response body")
	}
	usage, err := in.Usage.openAI()
	if err != nil {
		return nil, err
	}
	for _, field := range []struct{ name, value string }{
		{"id", in.ID},
		{"model", in.Model},
	} {
		if err := checkNativeIdentity(field.name, field.value); err != nil {
			return nil, err
		}
	}

	var content strings.Builder
	var toolCalls []map[string]any
	for _, block := range in.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			if err := checkNativeIdentity("tool id", block.ID); err != nil {
				return nil, err
			}
			if err := checkNativeIdentity("tool name", block.Name); err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": string(block.Input),
				},
			})
		}
	}

	out := sse.Completion(in.ID, time.Now().Unix(), in.Model,
		sse.AssistantMessage(content.String(), toolCalls), mapFinishReason(in.StopReason), usage)
	return sse.MarshalWireJSON(out)
}

// Native input_tokens excludes cache reads and writes; OpenAI prompt_tokens
// includes both. One converter owns this accounting for both bridge paths.
// Pointer fields allow partial streaming usage updates without erasing fields
// omitted by message_delta. Impossible counts fail translation, never wrap.
type nativeUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

func (u nativeUsage) openAI() (map[string]any, error) {
	count := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	read, write := count(u.CacheReadInputTokens), count(u.CacheCreationInputTokens)
	input, err := metrics.SumCounts(count(u.InputTokens), read, write)
	if err != nil {
		return nil, fmt.Errorf("native usage: %w", err)
	}
	output := count(u.OutputTokens)
	total, err := metrics.SumCounts(input, output)
	if err != nil {
		return nil, fmt.Errorf("native usage: %w", err)
	}
	return map[string]any{
		"prompt_tokens": input, "completion_tokens": output, "total_tokens": total,
		"prompt_tokens_details": map[string]any{"cached_tokens": read, "cache_write_tokens": write},
	}, nil
}

func mapFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		// Pass unknown reasons (e.g. "pause_turn", "refusal") through
		// verbatim so their signal isn't lost.
		return reason
	}
}

// StreamToOpenAI reads an Anthropic SSE stream and writes an OpenAI-compatible
// SSE stream to dst. It is a streaming transformer: events are converted
// one-by-one with no full-buffer. Each JSON event currently must fit on one
// data: line; general multi-line SSE event assembly is not implemented here.
// observe, when non-nil, receives original usage-bearing/terminal payloads
// synchronously; it must not retain the borrowed bytes. This lets the caller
// capture dynamic source-only accounting without changing the wire document.
// A terminal source event (message_stop or a valid provider error) closes the
// translation: later source events are ignored so nothing follows the one
// [DONE] marker.
func StreamToOpenAI(dst io.Writer, src io.Reader, flusher interface{ Flush() }, observe func([]byte)) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, sseLineBufInit), sseLineBufMax)

	var (
		id    string
		model string
		role  = "assistant"
		// One completion has one created timestamp for the whole stream.
		created = time.Now().Unix()
		// Anthropic addresses parallel tool_use blocks by index; track each
		// block's identity so interleaved deltas get the right id/name.
		toolCalls = map[int]*nativeTool{}
		// toolInitialBytes is the sum of every open block's retained initial
		// JSON, released as blocks stop (or all at once on a terminal event).
		toolInitialBytes int
		nextToolIndex    int
		usage            nativeUsage
		doneSent         bool
		// terminal records that a terminal source event (message_stop or a
		// valid provider error) has been translated. Everything after it is
		// protocol debris and must not emit frames behind the terminal marker.
		terminal bool
		// sawError records that the provider already delivered its own failure
		// statement (event: error). The truncation signal must never stack on
		// top of it: an errored stream terminates without message_stop, which
		// alone would look like a truncation.
		sawStop        bool
		sawMessageStop bool
		sawError       bool
	)
	emit := func(obj any) error {
		return sse.EmitFrame(dst, flusher, obj)
	}
	writeDone := func() error {
		// The terminal marker is written at most once. Providers/gateways
		// sometimes repeat message_stop, and a second [DONE] after the stream
		// is already closed is malformed wire output.
		if doneSent {
			return nil
		}
		if _, err := io.WriteString(dst, sse.DoneFrame); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		doneSent = true
		return nil
	}
	emitTool := func(call map[string]any) error {
		return emit(sse.Chunk(id, created, model,
			[]map[string]any{sse.DeltaChoice(map[string]any{"tool_calls": []map[string]any{call}}, nil)}, nil))
	}

	// SSE event type is block-scoped: it belongs to the blank-line-delimited
	// block carrying it, never to the next block. A data line that arrives
	// before its block's event: field is held in pending until the event field
	// or the block boundary reveals the type (SSE field order is not
	// significant); the payload's own type remains the fallback.
	var event string
	var pending []byte
	pendingSeen := false

	dispatch := func(ev string, payload []byte) error {
		if ev == "" {
			// Some Anthropic-compatible gateways omit the event: line and
			// carry the event type only in the payload.
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &probe) == nil {
				ev = probe.Type
			}
		}
		// Costs belong to the source usage-bearing/terminal document, not a
		// transformed content token. Observation never decodes every token.
		if observe != nil && (ev == "message_start" || ev == "message_delta" || ev == "message_stop" || ev == "error") {
			observe(payload)
		}

		switch ev {
		case "message_start":
			var m struct {
				Message struct {
					ID    string      `json:"id"`
					Model string      `json:"model"`
					Usage nativeUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(payload, &m); err != nil {
				return fmt.Errorf("native message start: %w", err)
			}
			if err := checkNativeIdentity("id", m.Message.ID); err != nil {
				return err
			}
			if err := checkNativeIdentity("model", m.Message.Model); err != nil {
				return err
			}
			id = m.Message.ID
			model = m.Message.Model
			usage = m.Message.Usage
			if _, err := usage.openAI(); err != nil {
				return err
			}
			if err := emit(sse.Chunk(id, created, model,
				[]map[string]any{sse.DeltaChoice(map[string]any{"role": role, "content": ""}, nil)}, nil)); err != nil {
				return err
			}
		case "content_block_start":
			var c struct {
				ContentBlock struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content_block"`
				Index int `json:"index"`
			}
			if json.Unmarshal(payload, &c) == nil {
				if c.ContentBlock.Type == "tool_use" {
					if err := checkNativeIdentity("tool id", c.ContentBlock.ID); err != nil {
						return err
					}
					if err := checkNativeIdentity("tool name", c.ContentBlock.Name); err != nil {
						return err
					}
					if _, exists := toolCalls[c.Index]; exists {
						return fmt.Errorf("duplicate native tool index %d", c.Index)
					}
					if len(toolCalls) >= nativeToolBlocksMax {
						return fmt.Errorf("%w (limit %d)", errNativeToolBlocks, nativeToolBlocksMax)
					}
					// Check the retained size before converting the raw JSON to
					// the string the open block keeps. One over-budget start
					// fails the translation instead of adding its bytes to the
					// open set; the decoder's own line-sized copy is bounded by
					// sseLineBufMax.
					retained := len(c.ContentBlock.Input)
					if retained == 0 || bytes.Equal(c.ContentBlock.Input, []byte("null")) {
						retained = len("{}")
					}
					if retained > nativeToolInitialBytesMax-toolInitialBytes {
						return fmt.Errorf("%w (limit %d bytes)", errNativeToolInitialBytes, nativeToolInitialBytesMax)
					}
					initial := string(c.ContentBlock.Input)
					if initial == "" || initial == "null" {
						initial = "{}"
					}
					toolCalls[c.Index] = &nativeTool{index: nextToolIndex, initial: initial}
					toolInitialBytes += len(initial)
					nextToolIndex++
					if err := emitTool(map[string]any{
						"index": toolCalls[c.Index].index, "id": c.ContentBlock.ID, "type": "function",
						"function": map[string]any{"name": c.ContentBlock.Name, "arguments": ""},
					}); err != nil {
						return err
					}
				}
			}
		case "content_block_delta":
			var d struct {
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
				Index int `json:"index"`
			}
			if json.Unmarshal(payload, &d) != nil {
				return nil
			}
			delta := map[string]any{}
			switch d.Delta.Type {
			case "text_delta":
				delta["content"] = d.Delta.Text
			case "input_json_delta":
				tc, ok := toolCalls[d.Index]
				if !ok {
					return fmt.Errorf("unknown native tool index %d", d.Index)
				}
				tc.arguments = tc.arguments || d.Delta.PartialJSON != ""
				delta["tool_calls"] = []map[string]any{
					{
						"index":    tc.index,
						"function": map[string]any{"arguments": d.Delta.PartialJSON},
					},
				}
			default:
				return nil
			}
			if err := emit(sse.Chunk(id, created, model,
				[]map[string]any{sse.DeltaChoice(delta, nil)}, nil)); err != nil {
				return err
			}
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(payload, &d) != nil {
				return nil
			}
			var wireUsage map[string]any
			if len(d.Usage) > 0 && string(d.Usage) != "null" {
				if err := json.Unmarshal(d.Usage, &usage); err != nil {
					return fmt.Errorf("native usage: %w", err)
				}
				var err error
				wireUsage, err = usage.openAI()
				if err != nil {
					return err
				}
			}
			// A non-null stop_reason means the content is complete (the
			// terminator guarantee is on message_delta, not message_stop).
			if d.Delta.StopReason != "" {
				sawStop = true
			}
			if err := emit(sse.Chunk(id, created, model,
				[]map[string]any{sse.DeltaChoice(map[string]any{}, mapFinishReason(d.Delta.StopReason))}, nil)); err != nil {
				return err
			}
			// Emit a final usage chunk. Anthropic provides input_tokens in
			// message_start and output_tokens in message_delta.
			if wireUsage != nil {
				if err := emit(sse.Chunk(id, created, model, []map[string]any{}, map[string]any{"usage": wireUsage})); err != nil {
					return err
				}
			}
		case "content_block_stop":
			var c struct {
				Index int `json:"index"`
			}
			if json.Unmarshal(payload, &c) == nil {
				if tc := toolCalls[c.Index]; tc != nil {
					toolInitialBytes -= len(tc.initial)
					if !tc.arguments {
						if err := emitTool(map[string]any{"index": tc.index, "function": map[string]any{"arguments": tc.initial}}); err != nil {
							return err
						}
					}
				}
				delete(toolCalls, c.Index)
			}
		case "message_stop":
			// Terminate the stream and release any blocks a malformed provider
			// left open (their terminal stop events may never arrive).
			sawMessageStop = true
			terminal = true
			clear(toolCalls)
			toolInitialBytes = 0
			if err := writeDone(); err != nil {
				return err
			}
		case "error":
			// Forward the error payload inline. A malformed or empty payload is
			// skipped (consistent with the other branches) - never emit an
			// empty error, and never claim a failure statement the provider did
			// not make: the truncation path below then owns the single frame.
			clear(toolCalls)
			toolInitialBytes = 0
			var e anthropicErrorEvent
			if err := json.Unmarshal(payload, &e); err != nil {
				return nil
			}
			if e.Error.Type == "" && e.Error.Message == "" {
				return nil
			}
			sawError = true
			terminal = true
			if err := emit(sse.Chunk(id, created, model, []map[string]any{}, e.openAIError())); err != nil {
				return err
			}
			// A provider error is a terminal source event: close the translated
			// stream now with the single [DONE] instead of waiting for an
			// upstream that may hold the connection open forever.
			return writeDone()
		}
		return nil
	}

	// flushPending dispatches the block's held data line at its blank-line
	// boundary, using the payload type fallback when the block carried no
	// event: field. It also resets a bare event-only block.
	flushPending := func() error {
		if !pendingSeen {
			event = ""
			return nil
		}
		ev := event
		payload := pending
		event = ""
		pendingSeen = false
		return dispatch(ev, payload)
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || bytes.Equal(line, []byte("\r")) {
			if err := flushPending(); err != nil {
				return err
			}
			if terminal {
				return nil
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			ev := strings.TrimPrefix(strings.TrimPrefix(string(line), "event:"), " ")
			if pendingSeen {
				// Field order is not significant: the data arrived first, so
				// this event field types that held payload.
				payload := pending
				pendingSeen = false
				if err := dispatch(ev, payload); err != nil {
					return err
				}
				if terminal {
					return nil
				}
				continue
			}
			event = ev
			continue
		}
		// Payload extraction is the frames owner's DataPayload: the full
		// "data:" prefix is required, one optional leading space is consumed,
		// ordinary payload bytes stay verbatim.
		payload, ok := sse.DataPayload(line)
		if !ok {
			continue
		}
		if event != "" {
			ev := event
			event = ""
			if err := dispatch(ev, payload); err != nil {
				return err
			}
			if terminal {
				return nil
			}
			continue
		}
		// Hold the data line until its block boundary (or a later event:
		// field) reveals the type; the payload type is the fallback.
		pending = append(pending[:0], payload...)
		pendingSeen = true
	}
	if err := flushPending(); err != nil {
		return err
	}
	if terminal {
		return nil
	}

	// Truncation: the stream ended without message_delta ever carrying a
	// stop_reason AND without message_stop AND without the provider's own error
	// event - the content is incomplete. Signal it in-band (a proper OpenAI
	// error chunk the client treats as a retryable failure) instead of
	// fabricating a clean [DONE] that would mask the truncation as a complete
	// stop. A stop_reason delivered without message_stop, or message_stop
	// without stop_reason, is the SERVER closing the turn - never flagged; an
	// error event is the provider's own failure - never stacked on.
	if !doneSent && !sawError && !sawStop && !sawMessageStop {
		if err := emit(sse.Chunk(id, created, model, []map[string]any{},
			map[string]any{"error": sse.ErrorObject(sse.TypeUpstreamError, metrics.CodeTruncated,
				metrics.DegenerateMessage(metrics.CodeTruncated))})); err != nil {
			return err
		}
	}
	if !doneSent {
		if err := writeDone(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type nativeTool struct {
	index     int
	initial   string
	arguments bool
}
