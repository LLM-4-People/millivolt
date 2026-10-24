package format

// C3 regressions: a streamed tool call with an empty name fails closed at the
// decoder that first sees it, before any target can emit "name":"". A valid
// call is unchanged.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatStreamMissingToolNameFailsClosed(t *testing.T) {
	const source = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(target.String(), func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(target, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
			if err == nil {
				t.Fatalf("missing tool name was accepted: %s", buf.String())
			}
			if strings.Contains(buf.String(), `"name":""`) {
				t.Fatalf("empty tool name reached the target wire: %s", buf.String())
			}
		})
	}
}

// TestStreamedToolNameRequiredAtEachDecoder covers the owner that sees the
// invalid name: the Chat opening fragment, the Anthropic content_block_start,
// the Responses output_item.added announcement, and a Responses terminal-only
// call. Each source is schema-valid except for the missing name.
func TestStreamedToolNameRequiredAtEachDecoder(t *testing.T) {
	chatSource := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: [DONE]

`
	anthropicSource := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
	responsesAnnouncement := terminalStreamCreated +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"","arguments":"{\"a\":1}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}` + "\n\n"
	responsesTerminalOnly := terminalStreamCreated +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"","arguments":"{\"a\":1}"}]}}` + "\n\n"

	cases := []struct {
		name   string
		source ir.Protocol
		target ir.Protocol
		wire   string
	}{
		{"chat decoder", ir.Chat, ir.Responses, chatSource},
		{"anthropic decoder", ir.Anthropic, ir.Responses, anthropicSource},
		{"responses announcement", ir.Responses, ir.Responses, responsesAnnouncement},
		{"responses terminal emission", ir.Responses, ir.Responses, responsesTerminalOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(tc.target, tc.source, &buf, strings.NewReader(tc.wire), nil, nil, StreamOptions{})
			if err == nil {
				t.Fatalf("missing tool name was accepted: %s", buf.String())
			}
			if strings.Contains(buf.String(), `"name":""`) {
				t.Fatalf("empty tool name reached the target wire: %s", buf.String())
			}
		})
	}
}

func TestStreamedToolNameValidSourceUnchanged(t *testing.T) {
	const source = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(target.String(), func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("valid tool call was rejected: %v", err)
			}
			calls := streamedToolCalls(t, target, buf.String())
			if len(calls) != 1 || calls[0].name != "lookup" || calls[0].args.String() != `{"a":1}` {
				t.Fatalf("calls = %+v, want one lookup call: %s", calls, buf.String())
			}
		})
	}
}
