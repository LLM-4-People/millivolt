package format

// C2 regression: Anthropic's official content_block_start tool input is {}.
// A non-empty start carries a complete JSON object, so a following
// input_json_delta cannot be concatenated with it; the decoder fails closed
// instead of producing target-dependent arguments. Start-only and delta-only
// shapes keep working.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicStreamStart = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":%s}}

`

func TestAnthropicStreamNonEmptyStartPlusDeltaFailsClosed(t *testing.T) {
	source := anthropicStreamStart + `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"b\":2}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
	source = strings.Replace(source, "%s", `{"a":1}`, 1)
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(target.String(), func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(target, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
			if err == nil {
				t.Fatalf("non-empty start plus delta was accepted: %s", buf.String())
			}
			if strings.Contains(buf.String(), `{\"a\":1}{\"b\":2}`) {
				t.Fatalf("start and delta were concatenated: %s", buf.String())
			}
		})
	}
}

func TestAnthropicStreamToolInputShapes(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		deltas       []string
		want         string
		wantByTarget map[ir.Protocol]string
	}{
		{
			name:  "start only",
			input: `{"a":1}`,
			want:  `{"a":1}`,
		},
		{
			name:   "delta only from empty start",
			input:  `{}`,
			deltas: []string{`{"b":2}`},
			want:   `{"b":2}`,
		},
		{
			name:   "delta fragments from empty start",
			input:  `{}`,
			deltas: []string{`{"b":`, `2}`},
			want:   `{"b":2}`,
		},
		{
			name:   "whitespace empty start with delta",
			input:  `{ }`,
			deltas: []string{`{"b":2}`},
			want:   `{"b":2}`,
		},
		{
			name:   "whitespace empty start with empty delta",
			input:  `{ }`,
			deltas: []string{``},
			want:   `{}`,
			// The Anthropic encoder always announces input {} at
			// content_block_start, so its canonical empty value is that start
			// payload and no input_json_delta follows.
			wantByTarget: map[ir.Protocol]string{ir.Anthropic: ""},
		},
		{
			name:   "non-empty start with empty delta",
			input:  `{"a":1}`,
			deltas: []string{``},
			want:   `{"a":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(anthropicStreamStart, "%s", tc.input, 1)
			for _, delta := range tc.deltas {
				source += `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + strings.ReplaceAll(delta, `"`, `\"`) + `"}}

`
			}
			source += `event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(target.String(), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
						t.Fatalf("TranslateStreamBetween: %v", err)
					}
					want := tc.want
					if targetWant, ok := tc.wantByTarget[target]; ok {
						want = targetWant
					}
					calls := streamedToolCalls(t, target, buf.String())
					if len(calls) != 1 || calls[0].name != "lookup" || calls[0].args.String() != want {
						t.Fatalf("calls = %+v, want lookup/%s: %s", calls, want, buf.String())
					}
				})
			}
		})
	}
}

// TestAnthropicWhitespaceToolInputCanonical is the round 12 C3 regression:
// every empty tool-input spelling is the same value, so a start of "{ }" must
// not prefix a later delta with those characters on the target wires.
func TestAnthropicWhitespaceToolInputCanonical(t *testing.T) {
	source := strings.Replace(anthropicStreamStart, "%s", `{ }`, 1) + `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	var out strings.Builder
	if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &out, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	wire := out.String()
	if strings.Contains(wire, "{ }") {
		t.Fatalf("target wire kept the whitespace spelling: %s", wire)
	}
	var doneArgs, itemArgs, terminalArgs string
	for _, frame := range parseWireFrames(t, wire) {
		switch frame.event {
		case "response.function_call_arguments.done":
			doneArgs, _ = frame.data["arguments"].(string)
		case "response.output_item.done":
			item, _ := frame.data["item"].(map[string]any)
			itemArgs, _ = item["arguments"].(string)
		case "response.completed":
			response, _ := frame.data["response"].(map[string]any)
			output, _ := response["output"].([]any)
			for _, raw := range output {
				item, _ := raw.(map[string]any)
				if item["type"] == "function_call" {
					terminalArgs, _ = item["arguments"].(string)
				}
			}
		}
	}
	for name, got := range map[string]string{"arguments.done": doneArgs, "output_item.done": itemArgs, "completed": terminalArgs} {
		if got != `{"a":1}` {
			t.Fatalf("%s arguments = %q, want {\"a\":1}: %s", name, got, wire)
		}
	}

	// An empty delta leaves the Anthropic start input as the one canonical
	// empty object and emits no input_json_delta.
	empty := strings.Replace(anthropicStreamStart, "%s", `{ }`, 1) + `event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	var emptyOut strings.Builder
	if err := TranslateStreamBetween(ir.Anthropic, ir.Anthropic, &emptyOut, strings.NewReader(empty), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	emptyWire := emptyOut.String()
	if !strings.Contains(emptyWire, `"input":{}`) {
		t.Fatalf("Anthropic start input is not the canonical empty object: %s", emptyWire)
	}
	if strings.Contains(emptyWire, `"input_json_delta"`) {
		t.Fatalf("empty tool input emitted a delta: %s", emptyWire)
	}
}
