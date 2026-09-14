package format

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTranslateRequestBasic(t *testing.T) {
	in := `{"model":"model-b","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}],"max_completion_tokens":100,"temperature":0.7,"stream":true}`
	out, err := TranslateRequest([]byte(in), 4096)
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

func TestTranslateRequestToolCalls(t *testing.T) {
	in := `{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NY\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	]}`

	out, err := TranslateRequest([]byte(in), 4096)
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

func TestMapFinishReasonPassthrough(t *testing.T) {
	for reason, want := range map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"pause_turn":    "pause_turn",
		"refusal":       "refusal",
	} {
		if got := mapFinishReason(reason); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", reason, got, want)
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
