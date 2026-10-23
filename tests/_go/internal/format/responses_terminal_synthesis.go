package format

// N8/N9 regressions: a Responses source may deliver its whole answer only in
// the terminal document. Translated targets must receive synthesized blocks,
// and an item that already streamed added/delta events must not be repeated.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const terminalStreamCreated = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}

`

func terminalStream(output string) string {
	return terminalStreamCreated + `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":` + output + `}}

`
}

var terminalTargets = []struct {
	name     string
	inbound  ir.Protocol
	evidence func(t *testing.T, out string) string
}{
	{"chat", ir.Chat, chatToolArguments},
	{"anthropic", ir.Anthropic, anthropicToolArguments},
	{"responses", ir.Responses, responsesToolArguments},
}

func TestResponsesTerminalOnlyToolCallTranslates(t *testing.T) {
	output := `[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStream(output)), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := target.evidence(t, buf.String()); got != `{"q":1}` {
				t.Fatalf("tool arguments = %q, want {\"q\":1}: %s", got, buf.String())
			}
		})
	}
}

func TestResponsesTerminalOnlyContentTranslates(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   map[ir.Protocol]string
	}{
		{
			name:   "text",
			output: `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]`,
			want: map[ir.Protocol]string{
				ir.Chat: `"content":"hello"`, ir.Anthropic: `"text":"hello"`, ir.Responses: `"text":"hello"`,
			},
		},
		{
			name:   "refusal",
			output: `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`,
			want: map[ir.Protocol]string{
				ir.Chat: `"refusal":"no"`, ir.Anthropic: `"text":"no"`, ir.Responses: `"refusal":"no"`,
			},
		},
		{
			name:   "summary reasoning",
			output: `[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}]`,
			want: map[ir.Protocol]string{
				ir.Chat: `"reasoning_content":"think"`, ir.Anthropic: `"thinking":"think"`, ir.Responses: `"text":"think"`,
			},
		},
		{
			name:   "content reasoning",
			output: `[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"deep"}]}]`,
			want: map[ir.Protocol]string{
				ir.Chat: `"reasoning_content":"deep"`, ir.Anthropic: `"thinking":"deep"`, ir.Responses: `"text":"deep"`,
			},
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStream(tc.output)), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if !strings.Contains(buf.String(), tc.want[target.inbound]) {
					t.Fatalf("missing %s in %s output: %s", tc.want[target.inbound], target.name, buf.String())
				}
			})
		}
	}
}

func TestResponsesTerminalMixedTextAndToolTranslates(t *testing.T) {
	output := `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStream(output)), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := target.evidence(t, buf.String()); got != `{"q":1}` {
				t.Fatalf("tool arguments = %q, want {\"q\":1}: %s", got, buf.String())
			}
			if !strings.Contains(buf.String(), "hello") {
				t.Fatalf("mixed terminal text was lost: %s", buf.String())
			}
		})
	}
}

func TestResponsesTerminalRepeatedItemNotDuplicated(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":1}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if target.inbound == ir.Responses {
				if got := completedFunctionCalls(t, buf.String()); got != 1 {
					t.Fatalf("terminal output has %d function calls, want one: %s", got, buf.String())
				}
			} else if got := strings.Count(buf.String(), "call_1"); got != 1 {
				t.Fatalf("tool call appeared %d times, want once: %s", got, buf.String())
			}
			if got := target.evidence(t, buf.String()); got != `{"q":1}` {
				t.Fatalf("tool arguments = %q, want {\"q\":1}", got)
			}
		})
	}
}

// completedFunctionCalls counts function_call items in the terminal response
// document of a translated Responses stream.
func completedFunctionCalls(t *testing.T, out string) int {
	t.Helper()
	for _, payload := range mustResponsePayloads(t, out) {
		if typ, _ := payload["type"].(string); typ != "response.completed" {
			continue
		}
		response, _ := payload["response"].(map[string]any)
		output, _ := response["output"].([]any)
		count := 0
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call" {
				count++
			}
		}
		return count
	}
	return 0
}
