package format

// Round 8 verifier NEW-3 regression: a terminal output array may reorder an
// id-less function call relative to its output_index. The synthetic id must
// stay with the announced block instead of creating a second call at the
// terminal array position.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesReorderedIdlessCallStream() string {
	return terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":{"type":"function_call","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":5,"delta":"{\"q\":1}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","name":"lookup","arguments":"{\"q\":1}"}]}}

`
}

func TestResponsesReorderedIdlessTerminalMatchesAnnouncement(t *testing.T) {
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
		t.Run(string(target), func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(responsesReorderedIdlessCallStream()), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			ids := streamToolCallIDs(t, target, out)
			if len(ids) != 1 || ids[0] != "call_proxy_5" {
				t.Fatalf("tool call ids = %v, want one call_proxy_5: %s", ids, out)
			}
			if strings.Contains(out, "call_proxy_0") {
				t.Fatalf("reordered terminal synthesized a second call: %s", out)
			}
			if target == ir.Chat {
				if args := chatToolArgumentValues(t, out); args != `{"q":1}` {
					t.Fatalf("Chat arguments = %q, want the announced call's arguments", args)
				}
			} else {
				if inputs := anthropicToolInputs(t, out); len(inputs) != 1 {
					t.Fatalf("Anthropic inputs = %v, want one tool_use block", inputs)
				}
				if !strings.Contains(out, "partial_json") || !strings.Contains(out, `\"q\":1`) {
					t.Fatalf("Anthropic stream lost the announced arguments: %s", out)
				}
			}
		})
	}
}
