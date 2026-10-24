package format

// Round 6 F2 regression: provider-executed Responses tool items (custom, MCP,
// shell, computer) keep the tool finish in streaming translation, matching the
// non-stream decoder. The target still drops the unrepresentable block, but a
// tool-only turn must not report a clean stop.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func providerToolStream(terminalOnly bool) string {
	item := `{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"shell","input":"x"}`
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	if !terminalOnly {
		b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":` + item + `}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":` + item + `}

`)
	}
	b.WriteString(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[` + item + `],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}

`)
	return b.String()
}

func TestTranslatedResponsesProviderToolCallKeepsToolFinish(t *testing.T) {
	for _, terminalOnly := range []bool{false, true} {
		name := "streamed item"
		if terminalOnly {
			name = "terminal-only item"
		}
		t.Run(name, func(t *testing.T) {
			var chat bytes.Buffer
			if err := TranslateStreamBetween(ir.Chat, ir.Responses, &chat, strings.NewReader(providerToolStream(terminalOnly)), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(chat.String(), `"finish_reason":"tool_calls"`) {
				t.Fatalf("Chat stream does not finish tool_calls: %s", chat.String())
			}
			if strings.Contains(chat.String(), `"finish_reason":"stop"`) {
				t.Fatalf("Chat stream still reports a clean stop: %s", chat.String())
			}

			var anthropic bytes.Buffer
			if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &anthropic, strings.NewReader(providerToolStream(terminalOnly)), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(anthropic.String(), `"stop_reason":"tool_use"`) {
				t.Fatalf("Anthropic stream does not finish tool_use: %s", anthropic.String())
			}
		})
	}
}
