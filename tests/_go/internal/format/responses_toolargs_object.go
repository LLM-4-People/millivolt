package format

// Round 4 batch 2 regression (C2): streamed and terminal-only tool arguments
// must satisfy the same JSON-object invariant as the non-stream IR.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func terminalFunctionCallStream(arguments string) string {
	return terminalStreamCreated + fmt.Sprintf(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":%q}]}}

`, arguments)
}

func anthropicToolInputs(t *testing.T, out string) []map[string]any {
	t.Helper()
	var inputs []map[string]any
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "content_block_start" {
			continue
		}
		block, _ := payload["content_block"].(map[string]any)
		if block["type"] != "tool_use" {
			continue
		}
		input, _ := block["input"].(map[string]any)
		inputs = append(inputs, input)
	}
	return inputs
}

func TestResponsesStreamToolArgumentsRequireJSONObject(t *testing.T) {
	translate := func(t *testing.T, target ir.Protocol, source string) (string, error) {
		t.Helper()
		var buf bytes.Buffer
		err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
		return buf.String(), err
	}

	t.Run("terminal not-json fails closed", func(t *testing.T) {
		_, err := translate(t, ir.Anthropic, terminalFunctionCallStream("not-json"))
		if err == nil || !strings.Contains(err.Error(), "tool call arguments must be") {
			t.Fatalf("terminal not-json error = %v", err)
		}
	})

	t.Run("streamed array fails closed", func(t *testing.T) {
		source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"[]"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"[]"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
		_, err := translate(t, ir.Anthropic, source)
		if err == nil || !strings.Contains(err.Error(), "tool call arguments must be") {
			t.Fatalf("streamed array error = %v", err)
		}
	})

	t.Run("streamed null fails closed", func(t *testing.T) {
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
			_, err := translate(t, target, responsesFunctionCallDoneStream("", "null", "null"))
			if err == nil || !strings.Contains(err.Error(), "tool call arguments must be a JSON object") {
				t.Fatalf("%s streamed null error = %v, want a JSON-object rejection", target, err)
			}
		}
	})

	t.Run("streamed empty array fails closed", func(t *testing.T) {
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
			_, err := translate(t, target, responsesFunctionCallDoneStream("", "[]", "[]"))
			if err == nil || !strings.Contains(err.Error(), "tool call arguments must be a JSON object") {
				t.Fatalf("%s streamed array error = %v, want a JSON-object rejection", target, err)
			}
		}
	})

	t.Run("terminal-only null normalizes to object", func(t *testing.T) {
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
			out, err := translate(t, target, terminalFunctionCallStream("null"))
			if err != nil {
				t.Fatalf("%s terminal-only null: %v", target, err)
			}
			if target == ir.Chat {
				if got := chatToolArguments(t, out); got != `{}` {
					t.Fatalf("terminal-only null arguments = %q, want {}", got)
				}
			} else {
				inputs := anthropicToolInputs(t, out)
				if len(inputs) != 1 || len(inputs[0]) != 0 {
					t.Fatalf("terminal-only null anthropic input = %v, want one empty object", inputs)
				}
			}
		}
	})

	t.Run("terminal object is emitted once", func(t *testing.T) {
		out, err := translate(t, ir.Chat, terminalFunctionCallStream(`{"q":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := chatToolArguments(t, out); got != `{"q":1}` {
			t.Fatalf("terminal object arguments = %q", got)
		}
	})

	t.Run("streamed object remains valid", func(t *testing.T) {
		out, err := translate(t, ir.Chat, responsesFunctionCallDoneStream("", `{\"q\":`, `{\"q\":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := chatToolArguments(t, out); got != `{"q":1}` {
			t.Fatalf("streamed object arguments = %q", got)
		}
	})

	t.Run("empty normalizes to object", func(t *testing.T) {
		anthropicOut, err := translate(t, ir.Anthropic, terminalFunctionCallStream(""))
		if err != nil {
			t.Fatal(err)
		}
		inputs := anthropicToolInputs(t, anthropicOut)
		if len(inputs) != 1 || len(inputs[0]) != 0 {
			t.Fatalf("empty anthropic tool input = %v, want one empty object", inputs)
		}
		responsesOut, err := translate(t, ir.Responses, terminalFunctionCallStream(""))
		if err != nil {
			t.Fatal(err)
		}
		if got := responsesToolArguments(t, responsesOut); got != `{}` {
			t.Fatalf("empty responses arguments = %q, want {}", got)
		}
	})
}
