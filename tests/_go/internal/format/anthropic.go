package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/sse"
)

func TestTranslateRequestBasic(t *testing.T) {
	in := `{"model":"model-b","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}],"max_completion_tokens":100,"temperature":0.7,"stream":true}`
	out, err := TranslateRequest([]byte(in), 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	if got["model"] != "model-b" {
		t.Errorf("model = %v", got["model"])
	}
	if got["max_tokens"] != float64(100) {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	if got["temperature"] != 0.7 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	if got["system"] != "You are helpful" {
		t.Errorf("system = %v", got["system"])
	}
	// messages should contain only the user message (system was lifted out).
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	user := msgs[0].(map[string]any)
	if user["role"] != "user" || user["content"] != "hi" {
		t.Errorf("user message = %v", user)
	}
}

func TestTranslateRequestContentArraysAndToolDefaults(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"system","content":[{"type":"text","text":"be nice"},{"type":"image"}]},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"noargs","arguments":""}}]},
		{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"result"}]}
	],"tools":[{"type":"function","function":{"name":"noargs","description":"d"}}]}`
	out, err := TranslateRequest([]byte(in), 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["system"] != "be nice" {
		t.Fatalf("array system = %v, want be nice", got["system"])
	}
	msgs := got["messages"].([]any)
	assistant := msgs[0].(map[string]any)
	block := assistant["content"].([]any)[0].(map[string]any)
	if input, ok := block["input"].(map[string]any); !ok || len(input) != 0 {
		t.Fatalf("empty tool arguments input = %#v, want {}", block["input"])
	}
	toolResult := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["content"] != "result" {
		t.Fatalf("array tool result = %#v, want result", toolResult["content"])
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	if schema, ok := tool["input_schema"].(map[string]any); !ok || len(schema) != 0 {
		t.Fatalf("omitted parameters schema = %#v, want {}", tool["input_schema"])
	}
}

func TestTranslateRequestToolCalls(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NY\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`

	out, err := TranslateRequest([]byte(in), 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3", len(msgs))
	}

	// Assistant message with tool call -> content blocks with tool_use.
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	toolUse := blocks[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call_1" || toolUse["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", toolUse)
	}

	// Tool result -> user message with tool_result block.
	toolResult := msgs[2].(map[string]any)
	trBlocks := toolResult["content"].([]any)
	tr := trBlocks[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["content"] != "sunny" {
		t.Errorf("tool_result block = %v", tr)
	}
}

// TestTranslateRequestAssistantToolCallText pins that assistant history text
// survives beside tool_calls in both accepted content shapes: the content-array
// form (which the tool-call branch used to drop because it only read the string
// form) and the plain string form as the control. The text block precedes the
// tool_use blocks and argument normalization is unchanged.
func TestTranslateRequestAssistantToolCallText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"array text", `[{"type":"text","text":"checking the tool"}]`, "checking the tool"},
		{"string control", `"checking the tool"`, "checking the tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"m","messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":%s,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NY\"}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"sunny"}
			]}`, tc.content)
			out, err := TranslateRequest([]byte(body), 4096, "")
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Messages) != 3 {
				t.Fatalf("messages len = %d, want 3: %s", len(got.Messages), out)
			}
			var blocks []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(got.Messages[1].Content, &blocks); err != nil {
				t.Fatalf("assistant content is not a block array: %v (%s)", err, out)
			}
			if len(blocks) != 2 {
				t.Fatalf("assistant blocks = %#v, want text + tool_use: %s", blocks, out)
			}
			if blocks[0].Type != "text" || blocks[0].Text != tc.want {
				t.Fatalf("assistant text block = %#v, want %q: %s", blocks[0], tc.want, out)
			}
			if blocks[1].Type != "tool_use" || blocks[1].ID != "call_1" || blocks[1].Name != "get_weather" {
				t.Fatalf("assistant tool_use block = %#v: %s", blocks[1], out)
			}
			if string(blocks[1].Input) != `{"city":"NY"}` {
				t.Fatalf("tool_use input = %s, want the call's arguments", blocks[1].Input)
			}
		})
	}
}

func TestTranslateResponse(t *testing.T) {
	in := `{"id":"msg_1","model":"claude","role":"assistant","content":[{"type":"text","text":"hello world"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3}}`
	out, err := TranslateResponse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	if got["object"] != "chat.completion" {
		t.Errorf("object = %v", got["object"])
	}
	choices := got["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Errorf("content = %v", msg["content"])
	}
	finish := choices[0].(map[string]any)["finish_reason"]
	if finish != "stop" {
		t.Errorf("finish_reason = %v", finish)
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(13) || usage["completion_tokens"] != float64(5) {
		t.Errorf("usage = %v", usage)
	}
}

// A non-Anthropic 200 body must fail closed (error), never be fabricated into
// an empty success. Regression test for the silent empty-completion.
func TestTranslateResponseRejectsNonAnthropic(t *testing.T) {
	for _, body := range []string{
		`{"id":"x","choices":[]}`, // OpenAI-shaped, not Anthropic
		`{"foo":"bar"}`,           // unrecognized
		`{}`,                      // empty
	} {
		if _, err := TranslateResponse([]byte(body)); err == nil {
			t.Errorf("TranslateResponse(%s) = nil error, want fail-closed rejection", body)
		}
	}
}

func TestStreamToOpenAI(t *testing.T) {
	anthropicStream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"input_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	err := StreamToOpenAI(&out, strings.NewReader(anthropicStream), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", got)
	}
	if !strings.Contains(got, `"content":"Hello"`) {
		t.Errorf("missing first delta: %q", got)
	}
	if !strings.Contains(got, `"content":" world"`) {
		t.Errorf("missing second delta: %q", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason: %q", got)
	}
	// Usage chunk should be present.
	if !strings.Contains(got, `"completion_tokens":2`) {
		t.Errorf("missing usage chunk: %q", got)
	}
	// Exactly one [DONE] even though message_stop was received.
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1: %q", n, got)
	}
}

func TestStreamToOpenAISingleDone(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1: %q", n, out.String())
	}
}

// TestStreamToOpenAIDuplicateMessageStopKeepsOneDone pins the terminal
// idempotence: providers/gateways occasionally repeat message_stop, and a
// second terminal event must not write a second [DONE] after the stream is
// already closed.
func TestStreamToOpenAIDuplicateMessageStopKeepsOneDone(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
	}
	if !strings.HasSuffix(got, sse.DoneFrame) {
		t.Fatalf("terminal frame must be the single [DONE]: %q", got)
	}
}

// TestStreamToOpenAIPostTerminalEventsIgnored pins that the terminal event is
// truly final. After message_stop has written [DONE], later content, error or
// duplicate terminal events must emit nothing. A valid provider error is also
// terminal: it is forwarded once, later source events are ignored, and the
// single [DONE] still closes the translated stream.
func TestStreamToOpenAIPostTerminalEventsIgnored(t *testing.T) {
	const (
		contentAfter = `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after"}}`
		errorEvent   = `data: {"type":"error","error":{"type":"overloaded_error","message":"late"}}`
		toolAfter    = `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-after","name":"late"}}`
		messageStop  = `data: {"type":"message_stop"}`
	)
	for _, tc := range []struct {
		name      string
		terminal  string
		wantError int
	}{
		{"message_stop", messageStop, 0},
		{"provider error", errorEvent, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join([]string{
				`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"before"}}`,
				tc.terminal,
				contentAfter,
				errorEvent,
				messageStop,
				toolAfter,
			}, "\n\n") + "\n\n"

			var out strings.Builder
			if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
				t.Fatalf("StreamToOpenAI = %v", err)
			}
			got := out.String()
			if !strings.Contains(got, `"content":"before"`) {
				t.Fatalf("pre-terminal content lost: %q", got)
			}
			if strings.Contains(got, `"content":"after"`) || strings.Contains(got, "tool-after") {
				t.Fatalf("post-terminal content or tool block was emitted: %q", got)
			}
			if n := strings.Count(got, `"error"`); n != tc.wantError {
				t.Fatalf("error frames = %d, want %d: %q", n, tc.wantError, got)
			}
			if n := strings.Count(got, "data: [DONE]"); n != 1 {
				t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
			}
			if !strings.HasSuffix(got, sse.DoneFrame) {
				t.Fatalf("stream does not end at the single [DONE]: %q", got)
			}
		})
	}
}

func TestStreamToOpenAIParallelToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A","name":"get_weather"}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_B","name":"get_time"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"zone\":"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}

	type toolCall struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	byIndex := map[int]toolCall{}
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []toolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", line, err)
		}
		for _, tc := range chunk.Choices[0].Delta.ToolCalls {
			acc := byIndex[tc.Index]
			acc.ID += tc.ID
			acc.Function.Name += tc.Function.Name
			acc.Function.Arguments += tc.Function.Arguments
			byIndex[tc.Index] = acc
		}
	}

	if tc := byIndex[0]; tc.ID != "toolu_A" || tc.Function.Name != "get_weather" {
		t.Errorf("index-0 delta identity = %q/%q, want toolu_A/get_weather", tc.ID, tc.Function.Name)
	}
	if tc := byIndex[1]; tc.ID != "toolu_B" || tc.Function.Name != "get_time" {
		t.Errorf("index-1 delta identity = %q/%q, want toolu_B/get_time", tc.ID, tc.Function.Name)
	}
}

func TestStreamToOpenAIDataOnly(t *testing.T) {
	// Valid SSE without event: lines - type comes from the payload only.
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"content":"hi"`) {
		t.Errorf("data-only stream dropped content: %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1: %q", n, got)
	}
}

// The Anthropic bridge pins ONE created timestamp per stream (the opposite
// of the cursor bridge's per-chunk policy, pinned in internal/proxy). The
// stream below pauses more than a second between events, so the two delta
// chunks cannot share a Unix second by accident: a per-chunk mutation
// produces different created values and reddens here deterministically.
func TestStreamToOpenAIPinsOneCreatedPerStream(t *testing.T) {
	first := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
	}, "\n") + "\n"
	second := strings.Join([]string{
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"
	src := &pausingReader{chunks: [][]byte{[]byte(first), []byte(second)}, pause: 1100 * time.Millisecond}

	var out strings.Builder
	if err := StreamToOpenAI(&out, src, nil, nil); err != nil {
		t.Fatal(err)
	}
	var created []int64
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Created int64 `json:"created"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", line, err)
		}
		created = append(created, chunk.Created)
	}
	if len(created) < 3 {
		t.Fatalf("chunks = %d, want at least 3: %q", len(created), out.String())
	}
	for _, c := range created[1:] {
		if c != created[0] {
			t.Fatalf("created = %v, want ONE stream-pinned timestamp (anthropic policy)", created)
		}
	}
	if created[0] == 0 {
		t.Fatalf("created = 0, want a real timestamp")
	}
}

// pausingReader yields one chunk per Read, sleeping between chunks so a test
// stream spans a Unix-second boundary deterministically.
type pausingReader struct {
	chunks [][]byte
	i      int
	pause  time.Duration
}

func (r *pausingReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	if r.i > 0 {
		time.Sleep(r.pause)
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// TestAnthropicStopReasonToChatFinishReason pins the public Anthropic-to-Chat
// mapping to the documented Chat wire enum. A provider-only reason is
// approximated: refusal keeps its meaning as content_filter, every other
// non-enum reason becomes stop, and the non-streaming response records the
// loss. The canonical record keeps the source spelling.
func TestAnthropicStopReasonToChatFinishReason(t *testing.T) {
	for reason, want := range map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"tool_use":                      "tool_calls",
		"pause_turn":                    "stop",
		"refusal":                       "content_filter",
		"provider_unknown":              "stop",
	} {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"` + reason + `",` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`
		out, warnings, err := TranslateResponseBetween(ir.Chat, ir.Anthropic, []byte(body))
		if err != nil {
			t.Fatalf("TranslateResponse(%q): %v", reason, err)
		}
		var got struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("decode translated response: %v", err)
		}
		if len(got.Choices) != 1 {
			t.Fatalf("stop_reason %q: choices = %d", reason, len(got.Choices))
		}
		if got.Choices[0].FinishReason != want {
			t.Errorf("stop_reason %q -> finish_reason %q, want %q", reason, got.Choices[0].FinishReason, want)
		}
		approximated := reason == "pause_turn" || reason == "refusal" || reason == "provider_unknown"
		found := false
		for _, warning := range warnings {
			if warning.Field == "finish_reason" {
				found = true
			}
		}
		if found != approximated {
			t.Errorf("stop_reason %q warning = %v, want %v (warnings %+v)", reason, found, approximated, warnings)
		}
	}
}

// anthropicErrorEvent is the shared owner of the {error:{type,message}}
// decode + OpenAI re-render on both Anthropic surfaces (the non-streaming
// error body and the SSE "error" event). Pin each surface's rendered shape.
func TestAnthropicErrorEventSurfaces(t *testing.T) {
	// Non-streaming: an Anthropic error body passes through as an
	// OpenAI-style error object.
	out, err := TranslateResponse([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Type != "overloaded_error" || got.Error.Message != "Overloaded" {
		t.Errorf("translated error body = %s", out)
	}

	// Streaming: the "error" event is forwarded as an in-band error chunk.
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"
	var sseOut strings.Builder
	if err := StreamToOpenAI(&sseOut, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sseOut.String(), `"error":{"message":"Overloaded","type":"overloaded_error"}`) {
		t.Errorf("stream error event not re-rendered as the shared OpenAI envelope: %q", sseOut.String())
	}
}

// TestTranslateRequestPromptCacheKey pins the sub-conversation injection: a
// non-empty tracked value lands as a top-level prompt_cache_key tracking
// extension (not an Anthropic cache field), and an empty one adds nothing.
func TestTranslateRequestPromptCacheKey(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	out, err := TranslateRequest([]byte(in), 4096, "task-7")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["prompt_cache_key"] != "task-7" {
		t.Errorf("prompt_cache_key = %v, want task-7 (body %s)", got["prompt_cache_key"], out)
	}

	out, err = TranslateRequest([]byte(in), 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["prompt_cache_key"]; ok {
		t.Errorf("untracked request gained a prompt_cache_key: %s", out)
	}
}

// allocDelta measures bytes allocated by fn without timing it.
func allocDelta(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestTranslateRequestSystemFragmentsExactOutput pins the exact system join:
// the "\n\n" separator between lifted non-empty fragments must survive the
// strings.Builder rewrite byte-for-byte. An empty fragment is dropped before
// the join because Anthropic text blocks require at least one character.
func TestTranslateRequestSystemFragmentsExactOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "leading system and mixed roles",
			body: `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"system","content":"a"},{"role":"developer","content":"b"},{"role":"user","content":"hi"}]}`,
			want: `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}],"model":"m","stream":false,"system":"s\n\na\n\nb"}`,
		},
		{
			name: "top-level system",
			body: `{"model":"m","system":"top","messages":[{"role":"system","content":"a"},{"role":"user","content":"hi"}]}`,
			want: `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}],"model":"m","stream":false,"system":"top\n\na"}`,
		},
		{
			name: "empty fragment skipped",
			body: `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"system","content":""},{"role":"system","content":"b"},{"role":"user","content":"hi"}]}`,
			want: `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}],"model":"m","stream":false,"system":"s\n\nb"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := TranslateRequest([]byte(tc.body), 64, "")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("translated request =\n%s\nwant\n%s", out, tc.want)
			}
		})
	}
}

// TestTranslateRequestSystemAllocationLinear proves the system join allocates
// linearly as the fragment count doubles; repeated += previously reallocated
// and copied the accumulated prefix every iteration.
func TestTranslateRequestSystemAllocationLinear(t *testing.T) {
	mk := func(n int) []byte {
		var b strings.Builder
		b.WriteString(`{"model":"m","messages":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"role":"system","content":"a"}`)
		}
		b.WriteString(`,{"role":"user","content":"hi"}]}`)
		return []byte(b.String())
	}
	wantFor := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteByte('a')
		}
		return b.String()
	}
	var prev uint64
	// The wire collection ceiling is 10_000 messages; doubling below it still
	// proves the join's allocation is linear rather than quadratic.
	for _, n := range []int{2000, 4000, 8000} {
		body := mk(n)
		var out []byte
		var err error
		alloc := allocDelta(func() { out, err = TranslateRequest(body, 64, "") })
		if err != nil {
			t.Fatalf("%d system fragments: %v", n, err)
		}
		var got struct {
			System string `json:"system"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.System != wantFor(n) {
			t.Fatalf("%d system fragments produced a changed join", n)
		}
		if prev > 0 && alloc > prev*5/2 {
			t.Fatalf("allocation growth %d -> %d bytes at %d fragments exceeds 2.5x", prev, alloc, n)
		}
		t.Logf("systems=%d input=%d alloc=%d", n, len(body), alloc)
		prev = alloc
	}
}

// TestTranslateResponseContentExactOutput pins the concatenated text content
// and the full non-streaming envelope (created normalized to zero).
func TestTranslateResponseContentExactOutput(t *testing.T) {
	out, err := TranslateResponse([]byte(`{"id":"msg","model":"m","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	doc["created"] = float64(0)
	normalized, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"choices":[{"finish_reason":"stop","index":0,"logprobs":null,"message":{"content":"ab","role":"assistant"}}],"created":0,"id":"msg","model":"m","object":"chat.completion","usage":{"completion_tokens":2,"prompt_tokens":1,"prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"total_tokens":3}}`
	if string(normalized) != want {
		t.Fatalf("translated response =\n%s\nwant\n%s", normalized, want)
	}
}

// TestTranslateResponseContentAllocationLinear mirrors the system join test for
// non-streaming text blocks, stopping at the shared composition bound.
func TestTranslateResponseContentAllocationLinear(t *testing.T) {
	mk := func(n int) []byte {
		var b strings.Builder
		b.WriteString(`{"id":"m","model":"m","content":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"type":"text","text":"a"}`)
		}
		b.WriteString(`],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		return []byte(b.String())
	}
	var prev uint64
	for _, n := range []int{1024, 2048, 4096} {
		body := mk(n)
		var out []byte
		var err error
		alloc := allocDelta(func() { out, err = TranslateResponse(body) })
		if err != nil {
			t.Fatalf("%d content blocks: %v", n, err)
		}
		var got struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Choices) != 1 || got.Choices[0].Message.Content != strings.Repeat("a", n) {
			t.Fatalf("%d content blocks produced changed content", n)
		}
		if prev > 0 && alloc > prev*5/2 {
			t.Fatalf("allocation growth %d -> %d bytes at %d blocks exceeds 2.5x", prev, alloc, n)
		}
		t.Logf("blocks=%d input=%d alloc=%d", n, len(body), alloc)
		prev = alloc
	}
}

// TestTranslateRequestToolChoiceShapes pins every accepted and rejected
// tool_choice shape at the translator owner. A restriction the target cannot
// represent (allowed_tools, an unknown type, a missing or non-string name)
// fails closed: dropping it would broaden tool access.
func TestTranslateRequestToolChoiceShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string // empty means the key is absent
		want    string // empty means the translated request carries no tool_choice
		wantErr bool
	}{
		{"absent", "", "", false},
		{"null", "null", "", false},
		{"auto", `"auto"`, `{"type":"auto"}`, false},
		{"none", `"none"`, `{"type":"none"}`, false},
		{"required", `"required"`, `{"type":"any"}`, false},
		{"unknown shorthand", `"sometimes"`, "", true},
		{"function object", `{"type":"function","function":{"name":"weather"}}`, `{"name":"weather","type":"tool"}`, false},
		{"object without type", `{"function":{"name":"weather"}}`, `{"name":"weather","type":"tool"}`, false},
		{"empty name", `{"function":{"name":""}}`, "", true},
		{"function without name", `{"function":{}}`, "", true},
		{"null name", `{"function":{"name":null}}`, "", true},
		{"non-string name", `{"function":{"name":7}}`, "", true},
		{"null function", `{"function":null}`, "", true},
		{"string function", `{"function":"weather"}`, "", true},
		{"array", `[1,2]`, "", true},
		{"number", `7`, "", true},
		{"boolean", `true`, "", true},
		{"allowed tools", `{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`, "", true},
		{"unknown members", `{"type":"function","function":{"name":"f"},"x":[1,{"y":2}]}`, `{"name":"f","type":"tool"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}}]`
			if tc.raw != "" {
				body += `,"tool_choice":` + tc.raw
			}
			body += `}`
			out, err := TranslateRequest([]byte(body), 64, "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("tool_choice %s was accepted: %s", tc.raw, out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ToolChoice json.RawMessage `json:"tool_choice"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(got.ToolChoice) != 0 {
					t.Fatalf("tool_choice = %s, want absent", got.ToolChoice)
				}
				return
			}
			if string(got.ToolChoice) != tc.want {
				t.Fatalf("tool_choice = %s, want %s", got.ToolChoice, tc.want)
			}
		})
	}
}

// hostileToolChoiceBody builds one MiB of tool_choice payload beside a valid
// tools array. The nested shape floods a top-level array member, the wide
// shape floods the top-level object, and the function shape floods a member
// of the function object itself. All three are amplification the former
// map[string]any decode paid for even though only function.name is retained.
func hostileToolChoiceBody(shape string) []byte {
	var body strings.Builder
	body.WriteString(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}}],"tool_choice":{"function":`)
	switch shape {
	case "function":
		body.WriteString(`{"name":"f","x":[`)
	case "nested":
		body.WriteString(`{"name":"f"},"x":[`)
	case "wide":
		body.WriteString(`{"name":"f"},"x":{`)
	}
	for i := 0; body.Len() < 1<<20; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		if shape == "wide" {
			fmt.Fprintf(&body, `"k%06d":0`, i)
		} else {
			body.WriteString(`{}`)
		}
	}
	switch shape {
	case "function":
		body.WriteString(`]}`)
	case "nested":
		body.WriteString(`]`)
	case "wide":
		body.WriteString(`}`)
	}
	body.WriteString(`}}`)
	return []byte(body.String())
}

// TestTranslateRequestToolChoiceDecodeAllocationBounded is the translator
// regression: a one MiB nested, wide, or function-local tool_choice must cost
// a small constant beyond the request bytes, not a per-node materialization.
// The former map[string]any decode allocated about 57x the value (~60 MiB).
func TestTranslateRequestToolChoiceDecodeAllocationBounded(t *testing.T) {
	for _, name := range []string{"nested", "wide", "function"} {
		t.Run(name, func(t *testing.T) {
			body := hostileToolChoiceBody(name)
			// Warm the decoder and prove the hostile shape still translates.
			out, err := TranslateRequest(body, 64, "")
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ToolChoice map[string]any `json:"tool_choice"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got.ToolChoice["type"] != "tool" || got.ToolChoice["name"] != "f" {
				t.Fatalf("translated tool_choice = %#v, want type tool name f", got.ToolChoice)
			}
			alloc := allocDelta(func() { _, _ = TranslateRequest(body, 64, "") })
			if alloc > 4<<20 {
				t.Fatalf("%d-byte tool_choice allocated %d bytes, want <= 4 MiB", len(body), alloc)
			}
			t.Logf("%s tool_choice: %d bytes, %d bytes allocated", name, len(body), alloc)
		})
	}
}

// TestNativeIdentityBound pins one 512-byte cap on provider identity fields on
// both translation surfaces. An overlong value fails with the stable sentinel
// before it can be echoed into output; values at the cap remain accepted.
func TestNativeIdentityBound(t *testing.T) {
	huge := strings.Repeat("i", 200<<10)
	if nativeIdentityMaxBytes != 512 {
		t.Fatalf("nativeIdentityMaxBytes = %d; the pinned record-consistent cap is 512", nativeIdentityMaxBytes)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"nonstream id", fmt.Sprintf(`{"id":%q,"model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}`, huge)},
		{"nonstream model", fmt.Sprintf(`{"id":"x","model":%q,"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}`, huge)},
		{"nonstream tool id", fmt.Sprintf(`{"id":"x","model":"m","content":[{"type":"tool_use","id":%q,"name":"n","input":{}}],"stop_reason":"tool_use"}`, huge)},
		{"nonstream tool name", fmt.Sprintf(`{"id":"x","model":"m","content":[{"type":"tool_use","id":"t","name":%q,"input":{}}],"stop_reason":"tool_use"}`, huge)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TranslateResponse([]byte(tc.body)); !errors.Is(err, errNativeIdentityTooLong) {
				t.Fatalf("overlong identity error = %v, want %v", err, errNativeIdentityTooLong)
			}
		})
	}

	for _, tc := range []struct {
		name string
		line string
	}{
		{"stream id", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"model":"m"}}`, huge)},
		{"stream model", fmt.Sprintf(`{"type":"message_start","message":{"id":"x","model":%q}}`, huge)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			err := StreamToOpenAI(&out, strings.NewReader("data: "+tc.line+"\n\n"), nil, nil)
			if !errors.Is(err, errNativeIdentityTooLong) {
				t.Fatalf("overlong identity error = %v, want %v", err, errNativeIdentityTooLong)
			}
			if out.Len() != 0 {
				t.Fatalf("overlong identity emitted %d bytes before failing", out.Len())
			}
		})
	}

	t.Run("stream tool identity", func(t *testing.T) {
		stream := "data: " + `{"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n" +
			"data: " + fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":"n"}}`, huge) + "\n\n"
		var out strings.Builder
		err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil)
		if !errors.Is(err, errNativeIdentityTooLong) {
			t.Fatalf("overlong tool identity error = %v, want %v", err, errNativeIdentityTooLong)
		}
		if out.Len() > 1<<10 {
			t.Fatalf("overlong tool identity emitted %d bytes, want a bounded prefix", out.Len())
		}
	})

	t.Run("at cap remains accepted", func(t *testing.T) {
		atCap := strings.Repeat("i", nativeIdentityMaxBytes)
		stream := "data: " + fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"model":%q}}`, atCap, atCap) + "\n\n" +
			"data: " + `{"type":"message_stop"}` + "\n\n"
		var out strings.Builder
		if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
			t.Fatalf("identity at the cap was rejected: %v", err)
		}
		if n := strings.Count(out.String(), atCap); n < 2 {
			t.Fatalf("identity at the cap was not echoed into the message_start frame: %d occurrences", n)
		}
	})
}

// TestNativeToolBlockCap pins the concurrent tool-block bound: unique starts
// without stops fail closed, while a terminal event closes the stream so a
// later start cannot emit behind the terminal marker.
func TestNativeToolBlockCap(t *testing.T) {
	if toolBlocksMax != 1024 {
		t.Fatalf("toolBlocksMax = %d; the pinned bound is 1024", toolBlocksMax)
	}
	var stream strings.Builder
	stream.WriteString("data: " + `{"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n")
	for i := 0; i <= toolBlocksMax; i++ {
		fmt.Fprintf(&stream, "data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t%d\",\"name\":\"n\"}}\n\n", i, i)
	}
	var out strings.Builder
	err := StreamToOpenAI(&out, strings.NewReader(stream.String()), nil, nil)
	if !errors.Is(err, errNativeToolBlocks) {
		t.Fatalf("unterminated tool blocks error = %v, want %v", err, errNativeToolBlocks)
	}
	if out.Len() > 1<<20 {
		t.Fatalf("tool-block overrun emitted %d bytes, want a bounded prefix", out.Len())
	}

	for _, terminal := range []struct{ name, event string }{
		{"message_stop", `{"type":"message_stop"}`},
		{"error", `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`},
	} {
		t.Run("ignored after "+terminal.name, func(t *testing.T) {
			var s strings.Builder
			s.WriteString("data: " + `{"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n")
			for i := 0; i < toolBlocksMax; i++ {
				fmt.Fprintf(&s, "data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t%d\",\"name\":\"n\"}}\n\n", i, i)
			}
			s.WriteString("data: " + terminal.event + "\n\n")
			s.WriteString("data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"again","name":"n"}}` + "\n\n")
			var o strings.Builder
			if err := StreamToOpenAI(&o, strings.NewReader(s.String()), nil, nil); err != nil {
				t.Fatalf("terminal event failed translation: %v", err)
			}
			if strings.Contains(o.String(), `"id":"again"`) {
				t.Fatal("post-terminal tool block was emitted")
			}
			if n := strings.Count(o.String(), "data: [DONE]"); n != 1 {
				t.Fatalf("[DONE] count = %d, want 1: %q", n, o.String())
			}
		})
	}
}
