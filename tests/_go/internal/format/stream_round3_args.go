package format

// Round 3 batch 4 regressions: one authoritative Anthropic terminal delta and
// tool arguments that follow the source's final value without prefix loss.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicMultipleMessageDeltaUsesFinalStopReason(t *testing.T) {
	source := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{},"usage":{"output_tokens":1}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	finishes := 0
	for _, payload := range chatPayloads(t, buf.String()) {
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		reason, _ := choices[0].(map[string]any)["finish_reason"].(string)
		if reason == "" {
			continue
		}
		if reason != "length" {
			t.Fatalf("finish = %q, want the final length reason: %s", reason, buf.String())
		}
		finishes++
	}
	if finishes != 1 {
		t.Fatalf("finish reasons = %d, want 1: %s", finishes, buf.String())
	}
}

func responsesFunctionCallDoneStream(initial, delta, done string) string {
	return terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"` + initial + `"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"` + delta + `"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"` + done + `"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"` + done + `"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
}

func TestResponsesToolArgumentsDoneExtendsDelta(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(
		responsesFunctionCallDoneStream("", `{\"q\":`, `{\"q\":1}`)), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	got := chatToolArguments(t, buf.String())
	if got != `{"q":1}` {
		t.Fatalf("arguments = %q, want the valid full value", got)
	}
}

func TestResponsesToolArgumentsDeltaAppendsAfterInitial(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(
		responsesFunctionCallDoneStream(`{\"a\":`, `{\"a\":1}}`, `{\"a\":{\"a\":1}}`)), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	got := chatToolArguments(t, buf.String())
	if got != `{"a":{"a":1}}` {
		t.Fatalf("arguments = %q, want the verbatim incremental value", got)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("arguments are not valid JSON: %q", got)
	}
}

func TestResponsesToolArgumentsDivergenceFailsClosed(t *testing.T) {
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(
		responsesFunctionCallDoneStream("", `{\"q\":`, `{\"other\":1}`)), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "tool arguments are not valid JSON") {
		t.Fatalf("divergent arguments error = %v", err)
	}
}
