package format

// N16 regressions: terminal synthesis must identify items by wire identity
// (call id / item id / content index), not by their position in the terminal
// output array. A shortened output array, a skipped output_index, or a
// duplicate announcement must not duplicate a function call or a text block.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalIdentityNotPosition(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{
			// A message was streamed at output 0 and the call at output 1;
			// the terminal array shortens to the call alone at position 0.
			name: "shortened output",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"q\":1}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]}}

`,
		},
		{
			// The stream starts at output_index 5, and the terminal array puts
			// the same call at position 0.
			name: "skipped output index",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":5,"delta":"{\"q\":1}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":5,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]}}

`,
		},
		{
			// A repeated announcement for one output_index must not allocate a
			// second target block.
			name: "duplicate announcement",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]}}

`,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := toolCallStarts(t, target.inbound, buf.String()); got != 1 {
					t.Fatalf("tool call starts = %d, want one: %s", got, buf.String())
				}
				if got := target.evidence(t, buf.String()); got != `{"q":1}` {
					t.Fatalf("tool arguments = %q, want {\"q\":1}: %s", got, buf.String())
				}
				if tc.name == "shortened output" {
					if got := textBlockStarts(t, target.inbound, buf.String()); got != 1 {
						t.Fatalf("text block starts = %d, want one: %s", got, buf.String())
					}
				}
			})
		}
	}
}

// toolCallStarts counts the target-protocol tool-call block openings.
func toolCallStarts(t *testing.T, protocol ir.Protocol, out string) int {
	t.Helper()
	count := 0
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] != "response.output_item.added" {
				continue
			}
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "function_call" {
				count++
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			if payload["type"] != "content_block_start" {
				continue
			}
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				count++
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			for _, call := range chatDeltaToolCalls(payload) {
				if call["id"] != nil {
					count++
				}
			}
		}
	}
	return count
}

// textBlockStarts counts target-protocol text block openings.
func textBlockStarts(t *testing.T, protocol ir.Protocol, out string) int {
	t.Helper()
	count := 0
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] != "response.content_part.added" {
				continue
			}
			part, _ := payload["part"].(map[string]any)
			if part["type"] == "output_text" {
				count++
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			if payload["type"] != "content_block_start" {
				continue
			}
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "text" {
				count++
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			if content, _ := chatDeltaContent(payload); content != "" {
				count++
			}
		}
	}
	return count
}

func chatPayloads(t *testing.T, out string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, frame := range strings.Split(out, "\n\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		payloads = append(payloads, obj)
	}
	return payloads
}

func chatDeltaContent(payload map[string]any) (string, bool) {
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return "", false
	}
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	content, ok := delta["content"].(string)
	return content, ok
}

func chatDeltaToolCalls(payload map[string]any) []map[string]any {
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	raw, _ := delta["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, call := range raw {
		if m, ok := call.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
