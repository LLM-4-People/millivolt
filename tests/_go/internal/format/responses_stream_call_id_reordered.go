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

// A terminal array can also put an id-less function call at the array position
// of an announced reasoning item while the real call was announced later.
// Positional matching must never treat a non-tool announcement as the call,
// which would flush the terminal arguments into the reasoning block and close
// the real call empty.
func TestResponsesReorderedIdlessTerminalSkipsNonToolAnnouncement(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":{"type":"function_call","name":"lookup","arguments":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","name":"lookup","arguments":"{\"q\":1}"},{"type":"reasoning","summary":[]}]}}

`
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			switch target {
			case ir.Chat:
				ids := streamToolCallIDs(t, target, out)
				if len(ids) != 1 || ids[0] != "call_proxy_5" {
					t.Fatalf("tool call ids = %v, want one call_proxy_5: %s", ids, out)
				}
				if args := chatToolArgumentValues(t, out); args != `{"q":1}` {
					t.Fatalf("Chat arguments = %q, want the terminal call's arguments", args)
				}
			case ir.Anthropic:
				ids := streamToolCallIDs(t, target, out)
				if len(ids) != 1 || ids[0] != "call_proxy_5" {
					t.Fatalf("tool call ids = %v, want one call_proxy_5: %s", ids, out)
				}
				thinkingIndex, toolIndex := -1, -1
				var toolArgs strings.Builder
				for _, payload := range mustAnthropicPayloads(t, out) {
					index, _ := payload["index"].(float64)
					switch payload["type"] {
					case "content_block_start":
						block, _ := payload["content_block"].(map[string]any)
						switch block["type"] {
						case "thinking":
							thinkingIndex = int(index)
						case "tool_use":
							toolIndex = int(index)
						}
					case "content_block_delta":
						delta, _ := payload["delta"].(map[string]any)
						if delta["type"] != "input_json_delta" {
							continue
						}
						if int(index) == thinkingIndex || (toolIndex >= 0 && int(index) != toolIndex) {
							t.Fatalf("input_json_delta landed on block %d (thinking=%d tool=%d): %s", int(index), thinkingIndex, toolIndex, out)
						}
						if value, _ := delta["partial_json"].(string); value != "" {
							toolArgs.WriteString(value)
						}
					}
				}
				if thinkingIndex < 0 || toolIndex < 0 {
					t.Fatalf("Anthropic stream missing thinking=%d or tool=%d block: %s", thinkingIndex, toolIndex, out)
				}
				if toolArgs.String() != `{"q":1}` {
					t.Fatalf("Anthropic tool arguments = %q, want the terminal object: %s", toolArgs.String(), out)
				}
			case ir.Responses:
				if !strings.Contains(out, `"arguments":"{\"q\":1}"`) {
					t.Fatalf("terminal arguments lost: %s", out)
				}
			}
			if strings.Contains(out, "call_proxy_0") {
				t.Fatalf("non-tool announcement was relabeled as a call: %s", out)
			}
		})
	}
}

type streamedToolCall struct {
	id   string
	name string
	args strings.Builder
}

// streamedToolCalls extracts the tool calls a translated stream opened, keyed
// by the target's block or tool index.
func streamedToolCalls(t *testing.T, target ir.Protocol, out string) []*streamedToolCall {
	t.Helper()
	byIndex := map[int]*streamedToolCall{}
	var order []int
	get := func(index int) *streamedToolCall {
		call := byIndex[index]
		if call == nil {
			call = &streamedToolCall{}
			byIndex[index] = call
			order = append(order, index)
		}
		return call
	}
	for _, frame := range parseWireFrames(t, out) {
		switch target {
		case ir.Chat:
			choices, _ := frame.data["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				index, _ := call["index"].(float64)
				function, _ := call["function"].(map[string]any)
				entry := get(int(index))
				if id, _ := call["id"].(string); id != "" {
					entry.id = id
				}
				if name, _ := function["name"].(string); name != "" {
					entry.name = name
				}
				if args, _ := function["arguments"].(string); args != "" {
					entry.args.WriteString(args)
				}
			}
		case ir.Anthropic:
			index, _ := frame.data["index"].(float64)
			switch frame.event {
			case "content_block_start":
				block, _ := frame.data["content_block"].(map[string]any)
				if block["type"] != "tool_use" {
					continue
				}
				entry := get(int(index))
				entry.id, _ = block["id"].(string)
				entry.name, _ = block["name"].(string)
			case "content_block_delta":
				delta, _ := frame.data["delta"].(map[string]any)
				if delta["type"] != "input_json_delta" {
					continue
				}
				if args, _ := delta["partial_json"].(string); args != "" {
					get(int(index)).args.WriteString(args)
				}
			}
		case ir.Responses:
			switch frame.event {
			case "response.output_item.added":
				item, _ := frame.data["item"].(map[string]any)
				if item["type"] != "function_call" {
					continue
				}
				index, _ := frame.data["output_index"].(float64)
				entry := get(int(index))
				entry.id, _ = item["call_id"].(string)
				if entry.id == "" {
					entry.id, _ = item["id"].(string)
				}
				entry.name, _ = item["name"].(string)
				if args, _ := item["arguments"].(string); args != "" && args != "{}" {
					entry.args.WriteString(args)
				}
			case "response.function_call_arguments.delta":
				index, _ := frame.data["output_index"].(float64)
				if args, _ := frame.data["delta"].(string); args != "" {
					get(int(index)).args.WriteString(args)
				}
			case "response.output_item.done":
				item, _ := frame.data["item"].(map[string]any)
				if item["type"] != "function_call" {
					continue
				}
				index, _ := frame.data["output_index"].(float64)
				entry := get(int(index))
				if entry.args.Len() == 0 {
					if args, _ := item["arguments"].(string); args != "" {
						entry.args.WriteString(args)
					}
				}
			}
		}
	}
	calls := make([]*streamedToolCall, 0, len(order))
	for _, index := range order {
		calls = append(calls, byIndex[index])
	}
	return calls
}

func chatCallID(t *testing.T, target ir.Protocol, call *streamedToolCall) string {
	t.Helper()
	if call.name == "" {
		t.Fatalf("%s call has no name", target)
	}
	return call.id
}

func TestResponsesReorderedIdlessTerminalMatchesIdentifiedAnnouncement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		announced string
		wantID    string
	}{
		{"announced item id", `{"type":"function_call","id":"fc_a","name":"lookup","arguments":""}`, "fc_a"},
		{"announced call id", `{"type":"function_call","call_id":"call_a","name":"lookup","arguments":""}`, "call_a"},
	} {
		source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":` + tc.announced + `}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","name":"lookup","arguments":"{\"q\":1}"}]}}

`
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
						t.Fatal(err)
					}
					out := buf.String()
					calls := streamedToolCalls(t, target, out)
					if len(calls) != 1 {
						t.Fatalf("tool calls = %d, want one: %s", len(calls), out)
					}
					if got := chatCallID(t, target, calls[0]); got != tc.wantID {
						t.Fatalf("tool id = %q, want %q: %s", got, tc.wantID, out)
					}
					if strings.TrimSpace(calls[0].args.String()) != `{"q":1}` {
						t.Fatalf("arguments = %q, want the terminal object: %s", calls[0].args.String(), out)
					}
				})
			}
		})
	}
}

func TestResponsesTerminalIdentitySpellingMatchesAnnouncement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		announced string
		terminal  string
		wantID    string
	}{
		{
			name:      "both to item id",
			announced: `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}`,
			terminal:  `{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{\"q\":1}"}`,
			wantID:    "call_1",
		},
		{
			name:      "both to call id",
			announced: `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}`,
			terminal:  `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}`,
			wantID:    "call_1",
		},
		{
			name:      "call id to item id",
			announced: `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}`,
			terminal:  `{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{\"q\":1}"}`,
			wantID:    "call_1",
		},
		{
			name:      "item id to call id",
			announced: `{"type":"function_call","id":"fc_1","name":"lookup","arguments":""}`,
			terminal:  `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}`,
			wantID:    "fc_1",
		},
	} {
		source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":` + tc.announced + `}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[` + tc.terminal + `]}}

`
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
						t.Fatal(err)
					}
					out := buf.String()
					calls := streamedToolCalls(t, target, out)
					if len(calls) != 1 {
						t.Fatalf("tool calls = %d, want one: %s", len(calls), out)
					}
					if got := chatCallID(t, target, calls[0]); got != tc.wantID {
						t.Fatalf("tool id = %q, want %q: %s", got, tc.wantID, out)
					}
					if strings.TrimSpace(calls[0].args.String()) != `{"q":1}` {
						t.Fatalf("arguments = %q, want the terminal object: %s", calls[0].args.String(), out)
					}
				})
			}
		})
	}
}

func TestResponsesReorderedIdlessTerminalMatchesByName(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"first","arguments":""}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":{"type":"function_call","name":"second","arguments":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","name":"second","arguments":"{\"s\":2}"},{"type":"function_call","name":"first","arguments":"{\"f\":1}"}]}}

`
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			calls := streamedToolCalls(t, target, out)
			if len(calls) != 2 {
				t.Fatalf("tool calls = %d, want two: %s", len(calls), out)
			}
			want := map[string]string{"first": `{"f":1}`, "second": `{"s":2}`}
			for _, call := range calls {
				if got, ok := want[call.name]; !ok {
					t.Fatalf("unexpected call name %q: %s", call.name, out)
				} else if strings.TrimSpace(call.args.String()) != got {
					t.Fatalf("%s arguments = %q, want %q: %s", call.name, call.args.String(), got, out)
				}
				delete(want, call.name)
			}
			if len(want) != 0 {
				t.Fatalf("missing calls: %v: %s", want, out)
			}
		})
	}
}

// A done event closes the announced block before the terminal document repeats
// it. The terminal may use the other identity spelling at another position; the
// alias must survive the close so the call is recognized, not synthesized again.
func TestResponsesTerminalIdentitySpellingAfterDoneMatchesAnnouncement(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":5,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{\"q\":1}"}]}}

`
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			calls := streamedToolCalls(t, target, out)
			if len(calls) != 1 {
				t.Fatalf("tool calls = %d, want one: %s", len(calls), out)
			}
			if got := chatCallID(t, target, calls[0]); got != "call_1" {
				t.Fatalf("tool id = %q, want call_1: %s", got, out)
			}
			if strings.TrimSpace(calls[0].args.String()) != `{"q":1}` {
				t.Fatalf("arguments = %q, want the terminal object: %s", calls[0].args.String(), out)
			}
		})
	}
}

// A terminal document repeats an announced call that its done event already
// closed. A different open same-name call must not receive that repeat's
// arguments through the name fallback.
func TestResponsesClosedCallRepeatDoesNotStealOpenSameNameCall(t *testing.T) {
	for _, tc := range []struct {
		name         string
		openItem     string
		terminalOpen string
		announcedA   string
		terminalA    string
		wantOpenID   string
	}{
		{
			name:         "open id-less call",
			openItem:     `{"type":"function_call","name":"lookup","arguments":""}`,
			terminalOpen: `{"type":"function_call","name":"lookup","arguments":"{\"b\":2}"}`,
			announcedA:   `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}`,
			terminalA:    `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"a\":1}"}`,
			wantOpenID:   "call_proxy_5",
		},
		{
			name:         "open item-id call",
			openItem:     `{"type":"function_call","id":"fc_b","name":"lookup","arguments":""}`,
			terminalOpen: `{"type":"function_call","id":"fc_b","name":"lookup","arguments":"{\"b\":2}"}`,
			announcedA:   `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}`,
			terminalA:    `{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"a\":1}"}`,
			wantOpenID:   "fc_b",
		},
		{
			name:         "open id-less call, alias repeat",
			openItem:     `{"type":"function_call","name":"lookup","arguments":""}`,
			terminalOpen: `{"type":"function_call","name":"lookup","arguments":"{\"b\":2}"}`,
			announcedA:   `{"type":"function_call","id":"fc_a","call_id":"call_1","name":"lookup","arguments":""}`,
			terminalA:    `{"type":"function_call","id":"fc_a","name":"lookup","arguments":"{\"a\":1}"}`,
			wantOpenID:   "call_proxy_5",
		},
	} {
		source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":` + tc.announcedA + `}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":5,"item":` + tc.openItem + `}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_1","name":"lookup","arguments":"{\"a\":1}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[` + tc.terminalA + `,` + tc.terminalOpen + `]}}

`
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
						t.Fatal(err)
					}
					out := buf.String()
					calls := streamedToolCalls(t, target, out)
					if len(calls) != 2 {
						t.Fatalf("tool calls = %d, want two: %s", len(calls), out)
					}
					args := map[string]string{}
					for _, call := range calls {
						args[call.id] = strings.TrimSpace(call.args.String())
					}
					if args["call_1"] != `{"a":1}` {
						t.Fatalf("closed call arguments = %q, want {\"a\":1}: %v\n%s", args["call_1"], args, out)
					}
					if args[tc.wantOpenID] != `{"b":2}` {
						t.Fatalf("open call %s arguments = %q, want {\"b\":2}: %v\n%s", tc.wantOpenID, args[tc.wantOpenID], args, out)
					}
				})
			}
		})
	}
}
