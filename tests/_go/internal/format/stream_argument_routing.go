package format

// Round 11 C3 regression: Responses argument events are routed only to an
// announced, still-open function-call item. itemBlock is an index cache that a
// reasoning or message announcement also fills, so argument bytes for that
// index used to become input_json_delta on a thinking block.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

type anthropicTestEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

func anthropicTestEvents(t *testing.T, out string) []anthropicTestEvent {
	t.Helper()
	var events []anthropicTestEvent
	for _, frame := range strings.Split(out, "\n\n") {
		var data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		var ev anthropicTestEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		events = append(events, ev)
	}
	return events
}

func TestResponsesArgumentDeltasRequireToolItem(t *testing.T) {
	reasoningAdded := responsesEvent("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`)
	reasoningTerminal := `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}`
	toolTerminal := `{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"b\":2}"}`
	terminal := func(items string) string {
		return responsesEvent("response.completed",
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+items+`]}}`)
	}

	cases := []struct {
		name      string
		source    string
		wantOther string
	}{
		{
			name:      "delta on a reasoning item",
			wantOther: "thinking",
			source: reasoningAdded + responsesEvent("response.function_call_arguments.delta",
				`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"rs_1","delta":"{\"a\":1}"}`) +
				terminal(reasoningTerminal+","+toolTerminal),
		},
		{
			name:      "done on a reasoning item",
			wantOther: "thinking",
			source: reasoningAdded + responsesEvent("response.function_call_arguments.done",
				`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"rs_1","arguments":"{\"a\":1}"}`) +
				terminal(reasoningTerminal+","+toolTerminal),
		},
		{
			name:      "delta on a message item",
			wantOther: "text",
			source: responsesEvent("response.output_item.added",
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`) +
				responsesEvent("response.function_call_arguments.delta",
					`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"msg_1","delta":"{\"a\":1}"}`) +
				terminal(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]},`+toolTerminal),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, tc.source)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			var other, tools int
			for _, ev := range anthropicTestEvents(t, out) {
				switch ev.Type {
				case "content_block_start":
					switch ev.ContentBlock.Type {
					case tc.wantOther:
						other++
					case "tool_use":
						tools++
						if ev.Index != 1 {
							t.Fatalf("tool_use opened at index %d, want 1: %s", ev.Index, out)
						}
					}
				case "content_block_delta":
					if ev.Delta.Type == "input_json_delta" && ev.Index == 0 {
						t.Fatalf("argument delta routed to the non-tool block: %s", out)
					}
				}
			}
			if other != 1 {
				t.Fatalf("%s blocks = %d, want 1: %s", tc.wantOther, other, out)
			}
			if tools != 1 {
				t.Fatalf("tool_use blocks = %d, want 1: %s", tools, out)
			}
			if !strings.Contains(out, `{\"b\":2}`) {
				t.Fatalf("terminal tool arguments missing: %s", out)
			}
			if strings.Contains(out, `{\"a\":1}`) {
				t.Fatalf("stray argument bytes reached the wire: %s", out)
			}
		})
	}
}

// TestResponsesArgumentDeltasToolControl keeps the legitimate route: an
// announced function call at the same index still receives its delta and done
// payload.
func TestResponsesArgumentDeltasToolControl(t *testing.T) {
	source := responsesEvent("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}}`) +
		responsesEvent("response.function_call_arguments.delta",
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"call_1","delta":"{\"a\":1}"}`) +
		responsesEvent("response.completed",
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"a\":1}"}]}}`)
	out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, source)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var tools int
	for _, ev := range anthropicTestEvents(t, out) {
		if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" {
			tools++
			if ev.Index != 0 {
				t.Fatalf("tool_use opened at index %d, want 0: %s", ev.Index, out)
			}
		}
	}
	if tools != 1 {
		t.Fatalf("tool_use blocks = %d, want 1: %s", tools, out)
	}
	if !strings.Contains(out, `{\"a\":1}`) {
		t.Fatalf("legitimate tool arguments missing: %s", out)
	}
}
