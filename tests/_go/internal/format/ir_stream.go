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
// TestChatStreamUsagePositions pins one terminal MessageDelta regardless of
// where the provider puts usage, and treats a finish_reason followed by EOF
// as a complete stream (the [DONE] sentinel is optional in practice).
// TestResponsesTerminalClosesOpenBlocks pins the Anthropic ordering invariant
// when a Responses provider omits item/content done events and carries the
// function arguments only in the terminal response.
func TestResponsesTerminalClosesOpenBlocks(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":4,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"","status":"in_progress"}}

event: response.completed
data: {"type":"response.completed","sequence_number":5,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}

`
	var buf bytes.Buffer
	collectStream(t, NewResponsesStreamDecoder(strings.NewReader(in)), NewAnthropicStreamEncoder(&buf, nil))
	out := buf.String()
	if n := strings.Count(out, "event: content_block_stop"); n != 2 {
		t.Fatalf("content_block_stop count = %d, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, `"partial_json":"{\"q\":1}"`) {
		t.Fatalf("terminal function arguments were not flushed:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("finish reason = not tool_use:\n%s", out)
	}
}

func TestResponsesStreamRetainedBound(t *testing.T) {
	var buf bytes.Buffer
	enc := NewResponsesStreamEncoder(&buf, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{ID: "r", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	err := enc.Write(&ir.Event{Type: ir.EventBlockDelta, Index: 0,
		Delta: &ir.Delta{Text: strings.Repeat("x", ResponsesStreamRetainedMaxBytes+1)}})
	if err == nil {
		t.Fatalf("a %d-byte item was retained beyond the bound", ResponsesStreamRetainedMaxBytes+1)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("bound error = %v", err)
	}
}

// TestAnthropicTerminalClosesOpenBlocks pins the target-encoder invariant when
// an Anthropic provider omits content_block_stop: the decoder closes the block
// before the finish event, and a late stop does not close it twice.
func TestAnthropicTerminalClosesOpenBlocks(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	collectStream(t, NewAnthropicStreamDecoder(strings.NewReader(in), nil), NewResponsesStreamEncoder(&buf, nil))
	out := buf.String()
	if n := strings.Count(out, "event: response.content_part.done"); n != 1 {
		t.Fatalf("content_part.done count = %d, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "event: response.completed"); n != 1 {
		t.Fatalf("response.completed count = %d, want 1:\n%s", n, out)
	}
}

func TestChatStreamUsagePositions(t *testing.T) {
	const (
		head   = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}` + "\n\n"
		usage  = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n"
		finish = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	)
	cases := map[string]string{
		"usage before finish":   head + usage + finish + "data: [DONE]\n\n",
		"usage after finish":    head + finish + usage + "data: [DONE]\n\n",
		"usage in finish":       head + `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}` + "\n\n" + "data: [DONE]\n\n",
		"finish then eof":       head + finish,
		"usage then finish eof": head + usage + finish,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			collectStream(t, NewChatStreamDecoder(strings.NewReader(in)), NewAnthropicStreamEncoder(&buf, nil))
			out := buf.String()
			if n := strings.Count(out, "event: message_delta"); n != 1 {
				t.Fatalf("message_delta count = %d, want 1:\n%s", n, out)
			}
			if strings.Contains(in, `"completion_tokens":5`) && !strings.Contains(out, `"output_tokens":5`) {
				t.Fatalf("usage lost:\n%s", out)
			}
			if !strings.Contains(out, `"stop_reason":"end_turn"`) || !strings.Contains(out, "event: message_stop") {
				t.Fatalf("missing terminal:\n%s", out)
			}
		})
	}
}

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

// TestAnthropicCachedUsageAcrossInputDelta pins C2: message_start establishes
// cache read/write totals, and a later message_delta may repeat only the
// non-cached input_tokens. The decoder must keep the cache totals, replace the
// base input, and recompute the full prompt count instead of aborting.
func TestAnthropicCachedUsageAcrossInputDelta(t *testing.T) {
	in := `data: {"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":0}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":5}}

data: {"type":"message_stop"}

`
	dec := NewAnthropicStreamDecoder(strings.NewReader(in), nil)
	var usage *ir.Usage
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("no message_delta usage")
	}
	if usage.InputTokens != 62 || usage.CacheReadTokens != 20 || usage.CacheWriteTokens != 30 ||
		usage.OutputTokens != 5 || usage.TotalTokens != 67 {
		t.Fatalf("usage = %+v, want input 62 (12 base + 20 read + 30 write), output 5, total 67", usage)
	}
}
