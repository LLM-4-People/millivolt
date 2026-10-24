package format

// Round 7 C3 regression: the Chat stream decoder must synthesize a bounded id
// when a modern tool call omits one, commit it on the first fragment, and keep
// a mixed legacy call distinct when its name collides with a committed id.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatLegacyFunctionCallNameCollisionKeepsBothCalls(t *testing.T) {
	source := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"lookup","type":"function","function":{"name":"modern","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"name":"lookup","arguments":"{\"b\":2}"}},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Count(got, `"type":"tool_use"`) != 2 {
		t.Fatalf("colliding calls were merged: %s", got)
	}
	if !strings.Contains(got, `"id":"lookup"`) {
		t.Fatalf("modern call id was rewritten: %s", got)
	}
	if !strings.Contains(got, `"id":"call_proxy_`) {
		t.Fatalf("legacy call kept the colliding id: %s", got)
	}
	if !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Fatalf("mixed stream did not finish as a tool turn: %s", got)
	}
}

func TestChatStreamMissingToolIDIsSynthesized(t *testing.T) {
	source := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"late_id","function":{}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	for _, target := range []ir.Protocol{ir.Responses, ir.Anthropic} {
		var buf bytes.Buffer
		if err := TranslateStreamBetween(target, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		got := buf.String()
		if strings.Contains(got, `"call_id":""`) || strings.Contains(got, `"id":""`) || strings.Contains(got, "late_id") {
			t.Fatalf("%s: missing or late id was not synthesized on the first fragment: %s", target, got)
		}
		if !strings.Contains(got, `"call_proxy_0"`) {
			t.Fatalf("%s: synthesized id missing: %s", target, got)
		}
		if strings.Contains(got, `"type":"error"`) {
			t.Fatalf("%s: synthesized id did not translate cleanly: %s", target, got)
		}
	}
}
