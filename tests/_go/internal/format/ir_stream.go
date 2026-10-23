package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func collectStream(t *testing.T, dec StreamDecoder, enc StreamEncoder) []string {
	t.Helper()
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := enc.Write(ev); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return nil
}

func streamFrames(buf *bytes.Buffer) []string {
	raw := strings.Split(buf.String(), "\n\n")
	out := make([]string, 0, len(raw))
	for _, frame := range raw {
		if strings.TrimSpace(frame) != "" {
			out = append(out, frame)
		}
	}
	return out
}

func chatDataFrames(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range streamFrames(buf) {
		if strings.TrimSpace(frame) == "data: [DONE]" {
			continue
		}
		payload, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		out = append(out, obj)
	}
	return out
}

func TestAnthropicStreamToChat(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

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
	dec := NewAnthropicStreamDecoder(strings.NewReader(in), nil)
	enc := NewChatStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	frames := chatDataFrames(t, &buf)
	if len(frames) < 5 {
		t.Fatalf("frames = %d: %s", len(frames), buf.String())
	}
	if !strings.Contains(buf.String(), "data: [DONE]") {
		t.Fatal("missing [DONE]")
	}
	var sawText, sawTool, sawFinish bool
	for _, f := range frames {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta["content"] == "Hello" {
			sawText = true
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			call := calls[0].(map[string]any)
			if call["id"] == "toolu_1" {
				sawTool = true
			}
		}
		if choice["finish_reason"] == "tool_calls" {
			sawFinish = true
		}
	}
	if !sawText || !sawTool || !sawFinish {
		t.Fatalf("text=%v tool=%v finish=%v\n%s", sawText, sawTool, sawFinish, buf.String())
	}
}

func TestChatStreamToAnthropic(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`
	var buf bytes.Buffer
	dec := NewChatStreamDecoder(strings.NewReader(in))
	enc := NewAnthropicStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	out := buf.String()
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"Hi"`,
		"event: content_block_start",
		`"type":"tool_use"`,
		`"partial_json":"{\"a\":1}"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestChatStreamToResponses(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`
	var buf bytes.Buffer
	dec := NewChatStreamDecoder(strings.NewReader(in))
	enc := NewResponsesStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	out := buf.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		`"delta":"Hi"`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"total_tokens":15`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestResponsesStreamToChat(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hi"}

event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"text":"Hi"}

event: response.content_part.done
data: {"type":"response.content_part.done","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hi"}}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi"}]}}

event: response.completed
data: {"type":"response.completed","sequence_number":7,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`
	var buf bytes.Buffer
	dec := NewResponsesStreamDecoder(strings.NewReader(in))
	enc := NewChatStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	frames := chatDataFrames(t, &buf)
	var sawText, sawFinish bool
	for _, f := range frames {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta["content"] == "Hi" {
			sawText = true
		}
		if choice["finish_reason"] == "stop" {
			sawFinish = true
		}
	}
	if !sawText || !sawFinish {
		t.Fatalf("text=%v finish=%v\n%s", sawText, sawFinish, buf.String())
	}
	if !strings.Contains(buf.String(), `"total_tokens":15`) {
		t.Fatalf("missing usage:\n%s", buf.String())
	}
}

func TestResponsesStreamToAnthropic(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hi"}

event: response.content_part.done
data: {"type":"response.content_part.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hi"}}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi"}]}}

event: response.completed
data: {"type":"response.completed","sequence_number":6,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`
	var buf bytes.Buffer
	dec := NewResponsesStreamDecoder(strings.NewReader(in))
	enc := NewAnthropicStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	out := buf.String()
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"Hi"`,
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestStreamRefusalKeepsItsOwnChannel pins refusal deltas across both
// directions: a refusal is never re-labeled as ordinary assistant content, and
// a Chat refusal stream reaches the Responses refusal event (not output_text).
func TestStreamRefusalKeepsItsOwnChannel(t *testing.T) {
	t.Run("responses to chat", func(t *testing.T) {
		in := `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}

event: response.refusal.delta
data: {"type":"response.refusal.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"no"}

event: response.completed
data: {"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}

`
		var buf bytes.Buffer
		collectStream(t, NewResponsesStreamDecoder(strings.NewReader(in)), NewChatStreamEncoder(&buf, nil))
		var sawRefusal bool
		for _, frame := range chatDataFrames(t, &buf) {
			choices, _ := frame["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			if delta["refusal"] == "no" {
				sawRefusal = true
			}
			if delta["content"] == "no" {
				t.Fatalf("refusal leaked into content: %s", buf.String())
			}
		}
		if !sawRefusal {
			t.Fatalf("missing refusal delta:\n%s", buf.String())
		}
	})

	t.Run("chat to responses", func(t *testing.T) {
		in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"refusal":"no"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
		var buf bytes.Buffer
		collectStream(t, NewChatStreamDecoder(strings.NewReader(in)), NewResponsesStreamEncoder(&buf, nil))
		if !strings.Contains(buf.String(), "event: response.refusal.delta") {
			t.Fatalf("missing refusal event:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "response.output_text.delta") {
			t.Fatalf("refusal leaked into output_text:\n%s", buf.String())
		}
		last := streamFrames(&buf)[len(streamFrames(&buf))-1]
		payload, ok := strings.CutPrefix(last, "event: response.completed\ndata: ")
		if !ok {
			t.Fatalf("last frame is not response.completed:\n%s", buf.String())
		}
		var completed struct {
			Response struct {
				Output []struct {
					Content []struct {
						Type    string `json:"type"`
						Refusal string `json:"refusal"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &completed); err != nil {
			t.Fatal(err)
		}
		if len(completed.Response.Output) != 1 || len(completed.Response.Output[0].Content) != 1 {
			t.Fatalf("completed output = %+v", completed.Response.Output)
		}
		part := completed.Response.Output[0].Content[0]
		if part.Type != "refusal" || part.Refusal != "no" {
			t.Fatalf("completed part = %+v, want refusal:no", part)
		}
	})
}

func TestAnthropicStreamTruncation(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`
	dec := NewAnthropicStreamDecoder(strings.NewReader(in), nil)
	for {
		_, err := dec.Next()
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrTruncatedStream) {
			t.Fatalf("err = %v, want ErrTruncatedStream", err)
		}
		break
	}
}

func TestAnthropicStreamThinking(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	dec := NewAnthropicStreamDecoder(strings.NewReader(in), nil)
	enc := NewAnthropicStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	out := buf.String()
	if !strings.Contains(out, `"type":"thinking_delta"`) || !strings.Contains(out, `"signature":"sig"`) {
		t.Fatalf("thinking not preserved:\n%s", out)
	}
}

func TestChatStreamToolCallWithoutArguments(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	var buf bytes.Buffer
	dec := NewChatStreamDecoder(strings.NewReader(in))
	enc := NewChatStreamEncoder(&buf, nil)
	collectStream(t, dec, enc)
	if !strings.Contains(buf.String(), `"arguments":"{}"`) {
		t.Fatalf("zero-argument tool call did not flush {}:\n%s", buf.String())
	}
}

func TestChatStreamDecoderUsage(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":2}}}

data: [DONE]

`
	dec := NewChatStreamDecoder(strings.NewReader(in))
	var usage *ir.Usage
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if usage == nil || usage.InputTokens != 10 || usage.CacheReadTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}
