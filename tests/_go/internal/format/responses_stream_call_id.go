package format

// Round 8 C1 regression: a streamed Responses function_call may omit call_id
// while carrying an item id, or omit both. The stream decoder used call_id
// verbatim, so the target block started with an empty id: Chat wrote it to the
// wire, Anthropic aborted, and parallel calls collided. One helper now falls
// back to the item id and then to a stable synthetic id.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesAddedCallStream(item string) string {
	return terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":` + item + `}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
}

func responsesTerminalCallStream(item string) string {
	return terminalStreamCreated + `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[` + item + `]}}

`
}

func streamToolCallIDs(t *testing.T, target ir.Protocol, out string) []string {
	t.Helper()
	var ids []string
	if target == ir.Chat {
		for _, payload := range chatPayloads(t, out) {
			choices, _ := payload["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, raw := range calls {
				id, _ := raw.(map[string]any)["id"].(string)
				if id != "" {
					ids = append(ids, id)
				}
			}
		}
		return ids
	}
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "content_block_start" {
			continue
		}
		block, _ := payload["content_block"].(map[string]any)
		if block["type"] == "tool_use" {
			id, _ := block["id"].(string)
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func TestResponsesStreamMissingCallIDUsesItemID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "added item id",
			source: responsesAddedCallStream(`{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{\"q\":1}"}`),
			want:   "fc_1",
		},
		{
			name:   "terminal item id",
			source: responsesTerminalCallStream(`{"type":"function_call","id":"fc_2","name":"lookup","arguments":"{\"q\":2}"}`),
			want:   "fc_2",
		},
		{
			name:   "added both absent",
			source: responsesAddedCallStream(`{"type":"function_call","name":"lookup","arguments":"{\"q\":1}"}`),
			want:   "call_proxy_0",
		},
		{
			name:   "terminal both absent",
			source: responsesTerminalCallStream(`{"type":"function_call","name":"lookup","arguments":"{\"q\":2}"}`),
			want:   "call_proxy_0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
				t.Run(string(target), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(tc.source), nil, nil, StreamOptions{}); err != nil {
						t.Fatalf("stream translation failed: %v", err)
					}
					out := buf.String()
					ids := streamToolCallIDs(t, target, out)
					if len(ids) != 1 {
						t.Fatalf("tool call ids = %v, want one id: %s", ids, out)
					}
					if ids[0] != tc.want {
						t.Fatalf("tool call id = %q, want %q: %s", ids[0], tc.want, out)
					}
				})
			}
		})
	}
}
