package format

// Round 2 correction NEW-5 regressions: a source response tool call that does
// not match the Anthropic target vocabulary must fail closed on both the
// non-streaming and streaming encoders instead of emitting an invalid
// tool_use block.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatResponseToolCallBody(id, name string) []byte {
	return []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}]}`)
}

func chatStreamToolCallSource(id, name string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[` +
		`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + id + `","type":"function",` +
		`"function":{"name":"` + name + `","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[` +
		`{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[` +
		`{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
}

func TestAnthropicResponseToolVocabularyRejected(t *testing.T) {
	cases := []struct {
		name string
		id   string
		tool string
		want string
	}{
		{name: "invalid id", id: "call:1", tool: "lookup", want: "tool_use.id"},
		{name: "invalid name", id: "call_1", tool: "lookup:1", want: "tool name"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/non-stream", func(t *testing.T) {
			_, _, err := TranslateResponseBetween(ir.Anthropic, ir.Chat, chatResponseToolCallBody(tc.id, tc.tool))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("non-stream error = %v, want %s rejection", err, tc.want)
			}
		})
		t.Run(tc.name+"/stream", func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader(
				chatStreamToolCallSource(tc.id, tc.tool)), nil, nil, StreamOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("stream error = %v, want %s rejection (output %s)", err, tc.want, buf.String())
			}
			if strings.Contains(buf.String(), tc.id) || strings.Contains(buf.String(), tc.tool) {
				t.Fatalf("invalid tool identity reached the wire: %s", buf.String())
			}
			if strings.Contains(buf.String(), `"type":"tool_use"`) {
				t.Fatalf("invalid tool call opened an Anthropic tool_use block: %s", buf.String())
			}
		})
	}
}

func TestAnthropicResponseToolVocabularyAccepted(t *testing.T) {
	body := chatResponseToolCallBody("call_1", "lookup")
	out, _, err := TranslateResponseBetween(ir.Anthropic, ir.Chat, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"tool_use"`) || !strings.Contains(string(out), `"id":"call_1"`) {
		t.Fatalf("valid tool call did not translate: %s", out)
	}

	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader(
		chatStreamToolCallSource("call_1", "lookup")), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"type":"tool_use"`) || !strings.Contains(buf.String(), `"id":"call_1"`) {
		t.Fatalf("valid streamed tool call did not translate: %s", buf.String())
	}
}
