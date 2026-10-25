package format

// C1/H2 regressions: the terminal-position fallback is tool-scoped. A
// reasoning or message announcement at the same output position must not
// claim an unannounced terminal function call, while an already-announced
// tool call still dedupes by identity and an id-present or position-1
// terminal call is still emitted.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalToolCallAfterForeignAnnouncement(t *testing.T) {
	const prefix = terminalStreamCreated +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}` + "\n\n"

	cases := []struct {
		name   string
		output string
	}{
		{
			name:   "foreign reasoning announcement at the same position",
			output: `[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"},{"type":"reasoning","id":"rs_1"}]`,
		},
		{
			name:   "id-present terminal call at a foreign position",
			output: `[{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{\"q\":1}"},{"type":"reasoning","id":"rs_1"}]`,
		},
		{
			name:   "terminal call at position 1",
			output: `[{"type":"reasoning","id":"rs_1"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]`,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				source := prefix + "event: response.completed\n" +
					`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":` + tc.output + `}}` + "\n\n"
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				out := buf.String()
				calls := streamedToolCalls(t, target.inbound, out)
				if len(calls) != 1 {
					t.Fatalf("tool calls = %d, want the one terminal call: %s", len(calls), out)
				}
				if calls[0].name != "lookup" || calls[0].args.String() != `{"q":1}` {
					t.Fatalf("tool call = %+v, want lookup/{\"q\":1}: %s", calls[0], out)
				}
				switch target.inbound {
				case ir.Chat:
					if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
						t.Fatalf("chat finish is not tool_calls: %s", out)
					}
				case ir.Anthropic:
					if !strings.Contains(out, `"stop_reason":"tool_use"`) {
						t.Fatalf("anthropic stop is not tool_use: %s", out)
					}
				case ir.Responses:
					if !strings.Contains(out, `"type":"function_call"`) {
						t.Fatalf("responses terminal has no function_call: %s", out)
					}
				}
			})
		}
	}
}

// TestResponsesTerminalToolCallStillDedupesAnnouncement pins the retained
// control: a terminal entry that matches an announced tool call by identity is
// not emitted a second time.
func TestResponsesTerminalToolCallStillDedupesAnnouncement(t *testing.T) {
	source := terminalStreamCreated +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]}}` + "\n\n"
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			out := buf.String()
			calls := streamedToolCalls(t, target.inbound, out)
			if len(calls) != 1 {
				t.Fatalf("tool calls = %d, want the single announced call: %s", len(calls), out)
			}
			if calls[0].args.String() != `{"q":1}` {
				t.Fatalf("arguments = %q, want one copy: %s", calls[0].args.String(), out)
			}
		})
	}
}
