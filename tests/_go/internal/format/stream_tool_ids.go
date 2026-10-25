package format

// Round 11 C2 regression: every streaming decoder reserves the tool-call ids
// it commits. A synthesized id that a later source id repeats, or two source
// ids that are equal, must fail closed instead of emitting two target blocks
// with one correlation key.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// translateStreamSource decodes sourceProtocol and encodes targetProtocol.
// TranslateStreamBetween names its first two arguments from the proxy's
// perspective: inbound is the client (target encoder), outbound the upstream
// (source decoder).
func translateStreamSource(t *testing.T, sourceProtocol, targetProtocol ir.Protocol, source string) (string, error) {
	t.Helper()
	var buf strings.Builder
	err := TranslateStreamBetween(targetProtocol, sourceProtocol, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
	return buf.String(), err
}

func chatToolStream(id0, id1 string) string {
	chunk := func(extra string) string {
		return `data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,"choices":[{"index":0,"delta":{` + extra + `}}]}` + "\n\n"
	}
	tool := func(index int, id, name string) string {
		idPart := ""
		if id != "" {
			idPart = `"id":"` + id + `",`
		}
		return chunk(fmt.Sprintf(`"tool_calls":[{%s"index":%d,"type":"function","function":{"name":%q,"arguments":"{}"}}]`, idPart, index, name))
	}
	return chunk(`"role":"assistant"`) +
		tool(0, id0, "f") +
		tool(1, id1, "g") +
		chunk(`"finish_reason":"tool_calls"`) +
		"data: [DONE]\n\n"
}

func anthropicToolStream(id0, id1 string) string {
	block := func(index int, id, name string) string {
		return fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%q,\"input\":{}}}\n\n", index, id, name)
	}
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		block(0, id0, "f") +
		block(1, id1, "g") +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func responsesAnnouncedToolStream(callID0, callID1 string) string {
	added := func(index int, callID, name string) string {
		return fmt.Sprintf("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":{\"type\":\"function_call\",\"call_id\":%q,\"name\":%q,\"arguments\":\"{}\"}}\n\n", index, callID, name)
	}
	return added(0, callID0, "f") + added(1, callID1, "g") +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"
}

func TestStreamToolIDsAreUnique(t *testing.T) {
	targets := []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses}
	cases := []struct {
		name    string
		inbound ir.Protocol
		source  string
		repeat  string
	}{
		{
			name:    "chat synthesized then explicit",
			inbound: ir.Chat,
			source:  chatToolStream("", "call_proxy_0"),
			repeat:  "call_proxy_0",
		},
		{
			name:    "chat two explicit ids",
			inbound: ir.Chat,
			source:  chatToolStream("dup", "dup"),
			repeat:  "dup",
		},
		{
			name:    "anthropic two source ids",
			inbound: ir.Anthropic,
			source:  anthropicToolStream("dup", "dup"),
			repeat:  "dup",
		},
		{
			name:    "anthropic synthesized then explicit",
			inbound: ir.Anthropic,
			source:  anthropicToolStream("", "toolu_proxy_0"),
			repeat:  "toolu_proxy_0",
		},
		{
			name:    "responses synthesized then explicit",
			inbound: ir.Responses,
			source:  responsesAnnouncedToolStream("", "call_proxy_0"),
			repeat:  "call_proxy_0",
		},
		{
			name:    "responses terminal-only collision",
			inbound: ir.Responses,
			source: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[" +
				`{"type":"function_call","name":"f","arguments":"{}"},` +
				`{"type":"function_call","call_id":"call_proxy_0","name":"g","arguments":"{}"}]}}` + "\n\n",
			repeat: "call_proxy_0",
		},
	}
	for _, tc := range cases {
		for _, target := range targets {
			t.Run(tc.name+"/"+string(target), func(t *testing.T) {
				out, err := translateStreamSource(t, tc.inbound, target, tc.source)
				if err == nil {
					t.Fatalf("duplicate tool id accepted for target %s: %s", target, out)
				}
				if !strings.Contains(err.Error(), "duplicate tool call id") {
					t.Fatalf("error = %v, want the duplicate tool id rejection", err)
				}
				if n := strings.Count(out, tc.repeat); n > 1 {
					t.Fatalf("emitted %d blocks carrying %q: %s", n, tc.repeat, out)
				}
			})
		}
	}
}

func TestStreamToolIDsDistinctControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound ir.Protocol
		source  string
	}{
		{
			name:    "chat distinct source ids",
			inbound: ir.Chat,
			source:  chatToolStream("one", "two"),
		},
		{
			name:    "chat synthesized suffix",
			inbound: ir.Chat,
			source:  chatToolStream("", ""),
		},
		{
			name:    "anthropic distinct source ids",
			inbound: ir.Anthropic,
			source:  anthropicToolStream("one", "two"),
		},
		{
			name:    "anthropic synthesized distinct ids",
			inbound: ir.Anthropic,
			source:  anthropicToolStream("", ""),
		},
		{
			name:    "responses distinct synthesized ids",
			inbound: ir.Responses,
			source:  responsesAnnouncedToolStream("", ""),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := translateStreamSource(t, tc.inbound, ir.Anthropic, tc.source)
			if err != nil {
				t.Fatalf("distinct ids rejected: %v", err)
			}
			if n := strings.Count(out, `"type":"tool_use"`); n != 2 {
				t.Fatalf("emitted %d tool_use blocks, want 2: %s", n, out)
			}
		})
	}
}
