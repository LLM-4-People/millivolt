// Package format translates between OpenAI Chat Completions and native
// provider wire formats. It is used only when a client explicitly opts in
// (X-Proxy-Format header); the default proxy path is byte-transparent.
package format

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const (
	// sseLineBufInit / sseLineBufMax bound a single SSE event line while
	// scanning an upstream Anthropic stream (initial buffer / hard cap).
	// Safety guardrail against pathological lines, not user-tunable.
	sseLineBufInit = 64 * 1024
	sseLineBufMax  = 1024 * 1024
	// sseFrameCap pre-sizes the reusable emit buffer to a typical single SSE
	// frame; it grows as needed.
	sseFrameCap = 512
)

// TranslateRequest converts an OpenAI Chat Completions request body into the
// Anthropic Messages request body. Unknown/unsupported fields are dropped;
// the goal is a working translation for the common case, not lossless
// fidelity. defaultMaxTokens is sent when the client provides neither
// max_tokens nor max_completion_tokens (Anthropic requires max_tokens); it
// comes from config.
func TranslateRequest(openaiBody []byte, defaultMaxTokens int) ([]byte, error) {
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
		return nil, fmt.Errorf("parse openai body: %w", err)
	}

	system := in.System
	var messages []anthropicMessage
	for _, m := range in.Messages {
		// Extract system messages into the top-level system field.
		if m.Role == "system" || m.Role == "developer" {
			if s, ok := m.ContentString(); ok {
				if system != "" {
					system += "\n\n"
				}
				system += s
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
	switch {
	case in.MaxCompletionToken != nil:
		out["max_tokens"] = *in.MaxCompletionToken
	case in.MaxTokens != nil:
		out["max_tokens"] = *in.MaxTokens
	default:
		out["max_tokens"] = defaultMaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if system != "" {
		out["system"] = system
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
	return json.Marshal(out)
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
			if s, ok := m.ContentString(); ok && s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": json.RawMessage(tc.Function.Arguments),
				})
			}
			return anthropicMessage{Role: "assistant", Content: blocks}, nil
		}
		return anthropicMessage{Role: "assistant", Content: rawOrString(m.Content)}, nil
	case "tool", "function":
		// A tool result becomes a user message with a tool_result block.
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			s = string(m.Content)
		}
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
	return anthropicTool{
		Name:        t.Function.Name,
		Description: t.Function.Description,
		InputSchema: t.Function.Parameters,
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

func parseToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none":
			return map[string]any{"type": s}
		case "required":
			return map[string]any{"type": "any"}
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		if fn, ok := obj["function"].(map[string]any); ok {
			return map[string]any{"type": "tool", "name": fn["name"]}
		}
	}
	return nil
}

// TranslateResponse converts an Anthropic Messages (non-streaming) response
// into an OpenAI Chat Completions response. An Anthropic error body
// ({"type":"error",...}) is passed through as a JSON object with an error key.
func TranslateResponse(anthropicBody []byte) ([]byte, error) {
	// Detect Anthropic error bodies and surface them as OpenAI-style errors.
	var errProbe struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(anthropicBody, &errProbe) == nil && errProbe.Type == "error" {
		return json.Marshal(map[string]any{
			"error": map[string]any{
				"message": errProbe.Error.Message,
				"type":    errProbe.Error.Type,
			},
		})
	}

	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string      `json:"stop_reason"`
		Usage      nativeUsage `json:"usage"`
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

	var content string
	var toolCalls []map[string]any
	for _, block := range in.Content {
		switch block.Type {
		case "text":
			content += block.Text
		case "tool_use":
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

	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      in.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   in.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": mapFinishReason(in.StopReason),
			},
		},
		"usage": usage,
	}
	return json.Marshal(out)
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
		toolCalls     = map[int]*nativeTool{}
		nextToolIndex int
		usage         nativeUsage
		doneSent      bool
		// sawError records that the provider already delivered its own failure
		// statement (event: error). The truncation signal must never stack on
		// top of it: an errored stream terminates without message_stop, which
		// alone would look like a truncation.
		sawStop        bool
		sawMessageStop bool
		sawError       bool
	)
	// frame is reused across emits: one buffer build, one Write per chunk.
	frame := bytes.NewBuffer(make([]byte, 0, sseFrameCap))
	emit := func(obj any) error {
		b, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		frame.Reset()
		frame.WriteString("data: ")
		frame.Write(b)
		frame.WriteString("\n\n")
		if _, err := dst.Write(frame.Bytes()); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	writeDone := func() error {
		if _, err := io.WriteString(dst, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		doneSent = true
		return nil
	}
	emitTool := func(call map[string]any) error {
		return emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{call}}, "finish_reason": nil}},
		})
	}

	var event string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")

		if event == "" {
			// Some Anthropic-compatible gateways omit the event: line and
			// carry the event type only in the payload.
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(payload), &probe) == nil {
				event = probe.Type
			}
		}

		// Consume the event for THIS data line, then reset it before dispatch so
		// a `continue` in any branch can never leave a stale event to misroute
		// the next data line (e.g. an unknown signature_delta followed by a
		// data-only message_stop).
		ev := event
		event = ""
		// Costs belong to the source usage-bearing/terminal document, not a
		// transformed content token. Observation never decodes every token.
		if observe != nil && (ev == "message_start" || ev == "message_delta" || ev == "message_stop" || ev == "error") {
			observe([]byte(payload))
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
			if err := json.Unmarshal([]byte(payload), &m); err != nil {
				return fmt.Errorf("native message start: %w", err)
			}
			id = m.Message.ID
			model = m.Message.Model
			usage = m.Message.Usage
			if _, err := usage.openAI(); err != nil {
				return err
			}
			if err := emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created,
				"model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": role, "content": ""}, "finish_reason": nil}},
			}); err != nil {
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
			if json.Unmarshal([]byte(payload), &c) == nil {
				if c.ContentBlock.Type == "tool_use" {
					if _, exists := toolCalls[c.Index]; exists {
						return fmt.Errorf("duplicate native tool index %d", c.Index)
					}
					initial := string(c.ContentBlock.Input)
					if initial == "" || initial == "null" {
						initial = "{}"
					}
					toolCalls[c.Index] = &nativeTool{index: nextToolIndex, initial: initial}
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
			if json.Unmarshal([]byte(payload), &d) != nil {
				continue
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
				continue
			}
			if err := emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created,
				"model": model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}},
			}); err != nil {
				return err
			}
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal([]byte(payload), &d) != nil {
				continue
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
			fr := mapFinishReason(d.Delta.StopReason)
			if err := emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created,
				"model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": fr}},
			}); err != nil {
				return err
			}
			// Emit a final usage chunk. Anthropic provides input_tokens in
			// message_start and output_tokens in message_delta.
			if wireUsage != nil {
				if err := emit(map[string]any{
					"id": id, "object": "chat.completion.chunk", "created": created,
					"model": model, "choices": []map[string]any{},
					"usage": wireUsage,
				}); err != nil {
					return err
				}
			}
		case "content_block_stop":
			var c struct {
				Index int `json:"index"`
			}
			if json.Unmarshal([]byte(payload), &c) == nil {
				if tc := toolCalls[c.Index]; tc != nil && !tc.arguments {
					if err := emitTool(map[string]any{"index": tc.index, "function": map[string]any{"arguments": tc.initial}}); err != nil {
						return err
					}
				}
				delete(toolCalls, c.Index)
			}
		case "message_stop":
			// Terminate the stream.
			sawMessageStop = true
			if err := writeDone(); err != nil {
				return err
			}
		case "error":
			// Forward the error payload inline. A malformed payload is skipped
			// (consistent with the other branches) - never emit an empty error.
			var e struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &e); err != nil {
				continue
			}
			if e.Error.Type != "" || e.Error.Message != "" {
				sawError = true
			}
			if err := emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created,
				"model": model, "choices": []map[string]any{},
				"error": map[string]string{"type": e.Error.Type, "message": e.Error.Message},
			}); err != nil {
				return err
			}
		}

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
		if err := emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": model, "choices": []map[string]any{},
			"error": map[string]any{
				"message": metrics.DegenerateMessage(metrics.CodeTruncated),
				"type":    "upstream_error",
				"param":   nil,
				"code":    metrics.CodeTruncated,
			},
		}); err != nil {
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
