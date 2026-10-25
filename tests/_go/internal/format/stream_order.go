package format

// Batch 17 regressions: the translated streams are ordered event streams, not
// bags of substrings. Each test parses the emitted frames and asserts the
// per-index lifecycle its encoder owns.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type wireFrame struct {
	event string
	data  map[string]any
}

func parseWireFrames(t *testing.T, out string) []wireFrame {
	t.Helper()
	var frames []wireFrame
	for _, raw := range strings.Split(out, "\n\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var event, payload string
		for _, line := range strings.Split(raw, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				event = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				payload = v
			}
		}
		if payload == "" {
			t.Fatalf("frame has no data line: %q\n%s", raw, out)
		}
		if payload == "[DONE]" {
			frames = append(frames, wireFrame{event: "[DONE]"})
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("frame %q: %v", raw, err)
		}
		if event == "" {
			if typ, ok := obj["type"].(string); ok {
				event = typ
			}
		}
		frames = append(frames, wireFrame{event: event, data: obj})
	}
	return frames
}

func intField(t *testing.T, frame wireFrame, key string) int {
	t.Helper()
	v, ok := frame.data[key]
	if !ok {
		t.Fatalf("%s frame has no %s: %v", frame.event, key, frame.data)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("%s frame %s = %v, want a number", frame.event, key, v)
	}
	return int(n)
}

func eventPosition(t *testing.T, frames []wireFrame, event string, outputIndex int) int {
	t.Helper()
	for i, frame := range frames {
		if frame.event != event {
			continue
		}
		if outputIndex < 0 {
			return i
		}
		if intField(t, frame, "output_index") == outputIndex {
			return i
		}
	}
	t.Fatalf("no %s frame for output_index %d in:\n%v", event, outputIndex, frames)
	return -1
}

func assertOrder(t *testing.T, what string, positions ...int) {
	t.Helper()
	for i := 1; i < len(positions); i++ {
		if positions[i] <= positions[i-1] {
			t.Fatalf("%s is out of order at position %d: %v", what, i, positions)
		}
	}
}

// TestAnthropicEncoderFrameOrder pins the Anthropic lifecycle: every index is
// opened before its deltas and closed before message_delta, and every started
// index is closed exactly once before message_stop.
func TestAnthropicEncoderFrameOrder(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	var buf bytes.Buffer
	collectStream(t, NewChatStreamDecoder(strings.NewReader(in)), NewAnthropicStreamEncoder(&buf, nil))
	frames := parseWireFrames(t, buf.String())
	if len(frames) < 4 {
		t.Fatalf("frames = %v", frames)
	}
	if frames[0].event != "message_start" {
		t.Fatalf("first event = %q, want message_start", frames[0].event)
	}
	if last := frames[len(frames)-1].event; last != "message_stop" {
		t.Fatalf("last event = %q, want message_stop", last)
	}
	messageDelta := eventPosition(t, frames, "message_delta", -1)
	state := map[int]string{}
	for i, frame := range frames {
		switch frame.event {
		case "content_block_start":
			idx := intField(t, frame, "index")
			if state[idx] != "" {
				t.Fatalf("duplicate start for index %d: %v", idx, frames)
			}
			state[idx] = "open"
		case "content_block_delta":
			idx := intField(t, frame, "index")
			if state[idx] != "open" {
				t.Fatalf("delta for index %d in state %q: %v", idx, state[idx], frames)
			}
		case "content_block_stop":
			idx := intField(t, frame, "index")
			if state[idx] != "open" {
				t.Fatalf("stop for index %d in state %q: %v", idx, state[idx], frames)
			}
			state[idx] = "closed"
			if i > messageDelta {
				t.Fatalf("stop for index %d follows message_delta: %v", idx, frames)
			}
		}
	}
	if len(state) == 0 {
		t.Fatalf("no content blocks in: %v", frames)
	}
	for idx, st := range state {
		if st != "closed" {
			t.Fatalf("index %d ended in state %q: %v", idx, st, frames)
		}
	}
	if messageDelta < 0 || messageDelta >= len(frames)-1 {
		t.Fatalf("message_delta position = %d of %d", messageDelta, len(frames))
	}
}

// TestResponsesEncoderFrameOrder pins the Responses lifecycle: each output
// item is added before its parts and deltas, its parts close in order, and the
// single terminal event follows every item.
func TestResponsesEncoderFrameOrder(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	var buf bytes.Buffer
	collectStream(t, NewChatStreamDecoder(strings.NewReader(in)), NewResponsesStreamEncoder(&buf, nil))
	frames := parseWireFrames(t, buf.String())
	if len(frames) < 6 {
		t.Fatalf("frames = %v", frames)
	}
	if frames[0].event != "response.created" || frames[1].event != "response.in_progress" {
		t.Fatalf("stream did not open with created/in_progress: %v", frames)
	}
	if last := frames[len(frames)-1].event; last != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", last)
	}

	// Text item 0: added, part added, delta, text done, part done, item done.
	assertOrder(t, "text item lifecycle",
		eventPosition(t, frames, "response.output_item.added", 0),
		eventPosition(t, frames, "response.content_part.added", 0),
		eventPosition(t, frames, "response.output_text.delta", 0),
		eventPosition(t, frames, "response.output_text.done", 0),
		eventPosition(t, frames, "response.content_part.done", 0),
		eventPosition(t, frames, "response.output_item.done", 0),
	)

	// Tool item 1: added, argument delta, argument done, item done.
	assertOrder(t, "function item lifecycle",
		eventPosition(t, frames, "response.output_item.added", 1),
		eventPosition(t, frames, "response.function_call_arguments.delta", 1),
		eventPosition(t, frames, "response.function_call_arguments.done", 1),
		eventPosition(t, frames, "response.output_item.done", 1),
	)

	completed := eventPosition(t, frames, "response.completed", -1)
	if completed != len(frames)-1 {
		t.Fatalf("response.completed at %d of %d", completed, len(frames))
	}
}

// TestChatEncoderFrameOrder pins the Chat lifecycle: the role chunk comes
// before any content, exactly one finish_reason chunk follows every delta, and
// the [DONE] sentinel is last.
func TestChatEncoderFrameOrder(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	collectStream(t, NewAnthropicStreamDecoder(strings.NewReader(in), nil), NewChatStreamEncoder(&buf, nil, false))
	frames := parseWireFrames(t, buf.String())
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	if last := frames[len(frames)-1].event; last != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE]: %v", last, frames)
	}

	rolePos, contentPos, toolPos, finishPos, finishCount := -1, -1, -1, -1, 0
	for i, frame := range frames {
		if frame.event == "[DONE]" {
			continue
		}
		choices, _ := frame.data["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta["role"] == "assistant" && rolePos < 0 {
			rolePos = i
		}
		if delta["content"] == "Hello" {
			contentPos = i
		}
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			call := calls[0].(map[string]any)
			if call["id"] == "toolu_1" || toolPos < 0 {
				toolPos = i
			}
		}
		if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
			finishCount++
			finishPos = i
		}
	}
	if rolePos < 0 || contentPos < 0 || toolPos < 0 || finishPos < 0 {
		t.Fatalf("role=%d content=%d tool=%d finish=%d: %v", rolePos, contentPos, toolPos, finishPos, frames)
	}
	if finishCount != 1 {
		t.Fatalf("finish_reason chunks = %d, want 1: %v", finishCount, frames)
	}
	assertOrder(t, "chat frames", rolePos, contentPos)
	assertOrder(t, "chat frames", rolePos, toolPos)
	assertOrder(t, "chat frames", contentPos, finishPos)
	assertOrder(t, "chat frames", toolPos, finishPos)
	if finishPos >= len(frames)-1 {
		t.Fatalf("finish chunk is after the sentinel: %v", frames)
	}
}

// TestChatChunkOpensReasoningBeforeText pins H16: when one provider chunk
// carries content and reasoning, the canonical order is thinking-first.
func TestChatChunkOpensReasoningBeforeText(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think","content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var buf bytes.Buffer
	collectStream(t, NewChatStreamDecoder(strings.NewReader(in)), NewAnthropicStreamEncoder(&buf, nil))
	frames := parseWireFrames(t, buf.String())
	reasoningStart, textStart := -1, -1
	for i, frame := range frames {
		if frame.event != "content_block_start" {
			continue
		}
		block, _ := frame.data["content_block"].(map[string]any)
		switch block["type"] {
		case "thinking":
			reasoningStart = i
		case "text":
			textStart = i
		}
	}
	if reasoningStart < 0 || textStart < 0 {
		t.Fatalf("missing block starts: %v", frames)
	}
	if reasoningStart >= textStart {
		t.Fatalf("text block opened before reasoning: reasoning=%d text=%d", reasoningStart, textStart)
	}
	if got := intField(t, frames[reasoningStart], "index"); got != 0 {
		t.Fatalf("reasoning index = %d, want 0", got)
	}
	if got := intField(t, frames[textStart], "index"); got != 1 {
		t.Fatalf("text index = %d, want 1", got)
	}
}
