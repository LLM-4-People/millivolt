package format

// Batch 2 regressions: Anthropic and Responses client streams must open with
// their mandatory preamble even when the source never delivered a start event,
// a Responses output_index sequence must stay contiguous, and a block that
// carries its whole payload in the start event must not lose it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// responseFrameTypes returns the ordered event names or data payload types of a
// translated stream, plus its raw text.
func responseFrameTypes(t *testing.T, out string) ([]string, []map[string]any) {
	t.Helper()
	var names []string
	var payloads []map[string]any
	for _, frame := range strings.Split(out, "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				name = value
			}
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				data = value
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		if name == "" {
			if typ, ok := obj["type"].(string); ok {
				name = typ
			}
		}
		names = append(names, name)
		payloads = append(payloads, obj)
	}
	return names, payloads
}

func anthropicEventData(t *testing.T, out string) ([]string, []map[string]any) {
	t.Helper()
	var names []string
	var payloads []map[string]any
	for _, frame := range strings.Split(out, "\n\n") {
		data := ""
		for _, line := range strings.Split(frame, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
				data = value
			}
		}
		if data == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		typ, _ := obj["type"].(string)
		names = append(names, typ)
		payloads = append(payloads, obj)
	}
	return names, payloads
}

// TestAnthropicBareDoneSynthesizesMessageStart pins C3: a bare [DONE] source
// still opens the Anthropic stream before its terminal frames.
func TestAnthropicBareDoneSynthesizesMessageStart(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader("data: [DONE]\n\n"), nil, nil); err != nil {
		t.Fatalf("TranslateStreamBetween = %v", err)
	}
	names, payloads := anthropicEventData(t, buf.String())
	if len(names) == 0 || names[0] != "message_start" {
		t.Fatalf("first Anthropic event = %v, want message_start: %s", names, buf.String())
	}
	message, _ := payloads[0]["message"].(map[string]any)
	if id, _ := message["id"].(string); id == "" {
		t.Fatalf("synthesized message_start has no id: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"type":"message_stop"`) {
		t.Fatalf("bare DONE stream has no message_stop: %s", buf.String())
	}
}

// TestResponsesBareDoneSynthesizesCreated pins C3 for a Responses client: the
// created/in_progress pair precedes the completed terminal.
func TestResponsesBareDoneSynthesizesCreated(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Chat, &buf, strings.NewReader("data: [DONE]\n\n"), nil, nil); err != nil {
		t.Fatalf("TranslateStreamBetween = %v", err)
	}
	names, payloads := responseFrameTypes(t, buf.String())
	if len(names) < 3 || names[0] != "response.created" || names[1] != "response.in_progress" {
		t.Fatalf("Responses preamble = %v, want created, in_progress: %s", names, buf.String())
	}
	response, _ := payloads[0]["response"].(map[string]any)
	if id, _ := response["id"].(string); !strings.HasPrefix(id, "resp_") {
		t.Fatalf("synthesized response id = %q, want resp_ prefix", id)
	}
	if strings.Contains(buf.String(), "data: [DONE]") {
		t.Fatalf("Responses stream leaked a Chat sentinel: %s", buf.String())
	}
}

// TestErrorFirstTranslatedStreamOpensTargetStream pins C3 on the error path:
// an error-first source stream still opens the target stream before its error.
func TestErrorFirstTranslatedStreamOpensTargetStream(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"error":{"message":"boom","type":"server_error"}}

data: [DONE]

`
	for _, tc := range []struct {
		name    string
		inbound ir.Protocol
		first   string
	}{
		{"anthropic", ir.Anthropic, "message_start"},
		{"responses", ir.Responses, "response.created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(tc.inbound, ir.Chat, &buf, strings.NewReader(in), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween = %v", err)
			}
			var names []string
			if tc.inbound == ir.Anthropic {
				names, _ = anthropicEventData(t, buf.String())
			} else {
				names, _ = responseFrameTypes(t, buf.String())
			}
			if len(names) == 0 || names[0] != tc.first {
				t.Fatalf("first event = %v, want %s: %s", names, tc.first, buf.String())
			}
		})
	}
}

// TestRedactedReasoningKeepsResponsesOutputIndexContiguous pins C4: an opaque
// Anthropic redacted_thinking block must be represented as a Responses
// reasoning item instead of consuming an output index that no item fills.
func TestRedactedReasoningKeepsResponsesOutputIndexContiguous(t *testing.T) {
	in := `data: {"type":"message_start","message":{"id":"m1","model":"m"}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(in), nil, nil); err != nil {
		t.Fatalf("TranslateStreamBetween = %v", err)
	}
	var indices []int
	for _, payload := range mustResponsePayloads(t, buf.String()) {
		if typ, _ := payload["type"].(string); typ != "response.output_item.added" {
			continue
		}
		index, ok := payload["output_index"].(float64)
		if !ok {
			t.Fatalf("output_item.added has no output_index: %v", payload)
		}
		indices = append(indices, int(index))
	}
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 1 {
		t.Fatalf("output indices = %v, want [0 1]: %s", indices, buf.String())
	}
	if !strings.Contains(buf.String(), `"encrypted_content":"opaque"`) {
		t.Fatalf("opaque redacted reasoning was not preserved: %s", buf.String())
	}
}

func mustResponsePayloads(t *testing.T, out string) []map[string]any {
	t.Helper()
	_, payloads := responseFrameTypes(t, out)
	return payloads
}

// TestBlockStartPayloadPreservedWithoutDeltas pins C6: a tool call whose whole
// argument object arrives in the block-start event must reach the client even
// when no argument delta follows.
func TestBlockStartPayloadPreservedWithoutDeltas(t *testing.T) {
	events := []*ir.Event{
		{Type: ir.EventStart, Response: &ir.Response{ID: "s1", Model: "m", Created: 1}},
		{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
			ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":1}`),
		}}},
		{Type: ir.EventBlockEnd, Index: 0},
		{Type: ir.EventDone},
	}

	t.Run("anthropic", func(t *testing.T) {
		var buf bytes.Buffer
		enc := NewAnthropicStreamEncoder(&buf, nil)
		for _, ev := range events {
			if err := enc.Write(ev); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		var args strings.Builder
		for _, payload := range mustAnthropicPayloads(t, buf.String()) {
			delta, _ := payload["delta"].(map[string]any)
			if partial, ok := delta["partial_json"].(string); ok {
				args.WriteString(partial)
			}
		}
		if args.String() != `{"q":1}` {
			t.Fatalf("Anthropic partial_json = %q, want the block-start payload: %s", args.String(), buf.String())
		}
	})

	t.Run("responses", func(t *testing.T) {
		var buf bytes.Buffer
		enc := NewResponsesStreamEncoder(&buf, nil)
		for _, ev := range events {
			if err := enc.Write(ev); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		var done string
		for _, payload := range mustResponsePayloads(t, buf.String()) {
			if typ, _ := payload["type"].(string); typ == "response.function_call_arguments.done" {
				done, _ = payload["arguments"].(string)
			}
		}
		if done != `{"q":1}` {
			t.Fatalf("Responses arguments.done = %q, want the block-start payload: %s", done, buf.String())
		}
	})
}

func mustAnthropicPayloads(t *testing.T, out string) []map[string]any {
	t.Helper()
	_, payloads := anthropicEventData(t, out)
	return payloads
}

// TestChatEncoderInitialBlockPayloads pins the Chat encoder's start-only
// payload path: text, refusal and reasoning delivered in BlockStart must reach
// the client when no matching delta follows.
func TestChatEncoderInitialBlockPayloads(t *testing.T) {
	cases := []struct {
		name  string
		block *ir.Block
		want  string
	}{
		{"text", &ir.Block{Type: ir.BlockText, Text: "hello"}, `"content":"hello"`},
		{"refusal", &ir.Block{Type: ir.BlockRefusal, Refusal: "no"}, `"refusal":"no"`},
		{"reasoning", &ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: "think", Summary: []string{" more"}}}, `"reasoning_content":"think more"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewChatStreamEncoder(&buf, nil)
			for _, ev := range []*ir.Event{
				{Type: ir.EventStart, Response: &ir.Response{ID: "c1", Model: "m", Created: 1}},
				{Type: ir.EventBlockStart, Index: 0, Block: tc.block},
				{Type: ir.EventBlockEnd, Index: 0},
				{Type: ir.EventDone},
			} {
				if err := enc.Write(ev); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			if err := enc.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("start-only %s missing %s: %s", tc.name, tc.want, buf.String())
			}
		})
	}

	t.Run("text followed by delta", func(t *testing.T) {
		var buf bytes.Buffer
		enc := NewChatStreamEncoder(&buf, nil)
		for _, ev := range []*ir.Event{
			{Type: ir.EventStart, Response: &ir.Response{ID: "c1", Model: "m", Created: 1}},
			{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText, Text: "hello"}},
			{Type: ir.EventBlockDelta, Index: 0, Delta: &ir.Delta{Text: " world"}},
			{Type: ir.EventBlockEnd, Index: 0},
			{Type: ir.EventDone},
		} {
			if err := enc.Write(ev); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := strings.Count(buf.String(), `"content":"hello"`); got != 1 {
			t.Fatalf("initial text emitted %d times: %s", got, buf.String())
		}
		if !strings.Contains(buf.String(), `"content":" world"`) {
			t.Fatalf("delta missing after initial text: %s", buf.String())
		}
	})
}

// TestResponsesInitialToolArguments pins C6/N10 on output_item.added
// arguments. A partial prefix must never reach the target alone: the decoder
// buffers it until the first delta or the done/terminal full value decides
// the continuation. Every target must receive one complete JSON object.
func TestResponsesInitialToolArguments(t *testing.T) {
	const created = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}

`
	added := func(args string) string {
		return fmt.Sprintf(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":%q}}

`, args)
	}
	delta := func(deltaArgs, doneArgs string) string {
		return fmt.Sprintf(`event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":%q}}

`, deltaArgs, doneArgs)
	}
	done := func(args string) string {
		return fmt.Sprintf(`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":%q}}

`, args)
	}
	terminal := func(args string) string {
		return fmt.Sprintf(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":%q}]}}

`, args)
	}
	variants := []struct {
		name   string
		stream string
		want   string
	}{
		{"start-only-then-terminal", added(`{"q":1}`) + terminal(`{"q":1}`), `{"q":1}`},
		{"partial-then-done", added(`{"q":1`) + done(`{"q":1}`) + terminal(`{"q":1}`), `{"q":1}`},
		{"partial-then-terminal", added(`{"q":1`) + terminal(`{"q":1}`), `{"q":1}`},
		{"full-then-done", added(`{"q":1}`) + done(`{"q":1}`) + terminal(`{"q":1}`), `{"q":1}`},
		{"full-then-repeated-delta", added(`{"q":1}`) + delta(`{"q":1}`, `{"q":1}`) + terminal(`{"q":1}`), `{"q":1}`},
		{"partial-then-suffix-delta", added(`{"q":1`) + delta(`}`, `{"q":1}`) + terminal(`{"q":1}`), `{"q":1}`},
		{"partial-no-full-value", added(`{"q":1`) + done("") + terminal(""), `{"q":1`},
	}
	for _, proto := range []struct {
		name     string
		inbound  ir.Protocol
		evidence func(t *testing.T, out string) string
	}{
		{"chat", ir.Chat, chatToolArguments},
		{"anthropic", ir.Anthropic, anthropicToolArguments},
		{"responses", ir.Responses, responsesToolArguments},
	} {
		for _, variant := range variants {
			t.Run(proto.name+"/"+variant.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(proto.inbound, ir.Responses, &buf, strings.NewReader(created+variant.stream), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := proto.evidence(t, buf.String()); got != variant.want {
					t.Fatalf("tool arguments = %q, want %q: %s", got, variant.want, buf.String())
				}
			})
		}
	}
}

func chatToolArguments(t *testing.T, out string) string {
	t.Helper()
	var args strings.Builder
	for _, frame := range strings.Split(out, "\n\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		choices, _ := obj["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, c := range calls {
			fn, _ := c.(map[string]any)["function"].(map[string]any)
			if v, ok := fn["arguments"].(string); ok {
				args.WriteString(v)
			}
		}
	}
	return args.String()
}

func anthropicToolArguments(t *testing.T, out string) string {
	t.Helper()
	var args strings.Builder
	for _, payload := range mustAnthropicPayloads(t, out) {
		delta, _ := payload["delta"].(map[string]any)
		if partial, ok := delta["partial_json"].(string); ok {
			args.WriteString(partial)
		}
	}
	return args.String()
}

func responsesToolArguments(t *testing.T, out string) string {
	t.Helper()
	for _, payload := range mustResponsePayloads(t, out) {
		if typ, _ := payload["type"].(string); typ != "response.completed" {
			continue
		}
		response, _ := payload["response"].(map[string]any)
		output, _ := response["output"].([]any)
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] != "function_call" {
				continue
			}
			args, _ := item["arguments"].(string)
			return args
		}
		return ""
	}
	return ""
}
