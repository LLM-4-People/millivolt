package format

// Round 12 B1/B-U regression: an Anthropic response whose content carries a
// refusal or a tool call must not report end_turn. Anthropic's stop_reason is
// the field clients branch on: end_turn reads as a finished answer, so a tool
// call is never run and a refusal is read as normal content.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicWireStopReason(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.StopReason
}

func anthropicStreamStopReason(t *testing.T, out string) string {
	t.Helper()
	for _, frame := range parseWireFrames(t, out) {
		if frame.event != "message_delta" {
			continue
		}
		delta, _ := frame.data["delta"].(map[string]any)
		reason, _ := delta["stop_reason"].(string)
		return reason
	}
	t.Fatalf("no message_delta in the Anthropic stream: %s", out)
	return ""
}

func TestAnthropicContentDerivedStopReason(t *testing.T) {
	chatRefusal := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"stop"}]}`
	chatToolStop := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}]}`
	chatBoth := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"no","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}]}`
	responsesRefusal := `{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`

	t.Run("non-stream", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			source      ir.Protocol
			body        string
			want        string
			wantWarning bool
		}{
			{name: "chat refusal", source: ir.Chat, body: chatRefusal, want: "refusal", wantWarning: true},
			{name: "responses refusal", source: ir.Responses, body: responsesRefusal, want: "refusal", wantWarning: true},
			{name: "chat stop plus tool calls", source: ir.Chat, body: chatToolStop, want: "tool_use", wantWarning: true},
			{name: "refusal wins over tool call", source: ir.Chat, body: chatBoth, want: "refusal", wantWarning: true},
			{name: "tool_calls control", source: ir.Chat, body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, want: "tool_use"},
			{name: "length control", source: ir.Chat, body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`, want: "max_tokens"},
			{name: "content_filter control", source: ir.Chat, body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"no"},"finish_reason":"content_filter"}]}`, want: "refusal"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, warnings, err := TranslateResponseBetween(ir.Anthropic, tc.source, []byte(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				if got := anthropicWireStopReason(t, string(out)); got != tc.want {
					t.Fatalf("stop_reason = %q, want %q: %s", got, tc.want, out)
				}
				if got := warningField(warnings, "stop_reason"); got != tc.wantWarning {
					t.Fatalf("stop_reason warning = %v, want %v (%+v)", got, tc.wantWarning, warnings)
				}
			})
		}
	})

	t.Run("stream", func(t *testing.T) {
		chat := func(delta, finish string) string {
			return `data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,"choices":[{"index":0,"delta":{` + delta + `}}]}` + "\n\n" +
				`data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}` + "\n\n" +
				"data: [DONE]\n\n"
		}
		for _, tc := range []struct {
			name   string
			source ir.Protocol
			stream string
			want   string
		}{
			{
				name:   "chat refusal",
				source: ir.Chat,
				stream: chat(`"role":"assistant","refusal":"no"`, "stop"),
				want:   "refusal",
			},
			{
				name:   "chat stop plus tool calls",
				source: ir.Chat,
				stream: chat(`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]`, "stop"),
				want:   "tool_use",
			},
			{
				name:   "tool_calls control",
				source: ir.Chat,
				stream: chat(`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]`, "tool_calls"),
				want:   "tool_use",
			},
			{
				name:   "length control",
				source: ir.Chat,
				stream: chat(`"content":"partial"`, "length"),
				want:   "max_tokens",
			},
			{
				name:   "responses refusal",
				source: ir.Responses,
				stream: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}` + "\n\n" +
					`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"msg_1","part":{"type":"refusal","refusal":""}}` + "\n\n" +
					`data: {"type":"response.refusal.delta","output_index":0,"content_index":0,"item_id":"msg_1","delta":"no"}` + "\n\n" +
					`data: {"type":"response.completed","response":{"id":"r","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}}` + "\n\n",
				want: "refusal",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, err := translateStreamSource(t, tc.source, ir.Anthropic, tc.stream)
				if err != nil {
					t.Fatal(err)
				}
				if got := anthropicStreamStopReason(t, out); got != tc.want {
					t.Fatalf("stream stop_reason = %q, want %q: %s", got, tc.want, strings.TrimSpace(out))
				}
			})
		}
	})
}
