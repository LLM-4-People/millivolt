package format

// Round 7 C2/C3 regression: the Chat stream decoder must decode the deprecated
// delta.function_call object (still in the official schema) using its function
// name as the call id, keep every call distinct when a stream mixes shapes and
// ids collide, and synthesize a bounded id when a modern tool call omits one.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatLegacyStream(fragments ...string) string {
	stream := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

`
	for _, fragment := range fragments {
		stream += `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":` + fragment + `},"finish_reason":null}]}

`
	}
	return stream + `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}

data: [DONE]

`
}

func TestChatLegacyFunctionCallStreamTranslates(t *testing.T) {
	source := chatLegacyStream(
		`{"name":"lookup","arguments":"{\"q\":"}`,
		`{"arguments":"1}"}`,
	)

	var responses bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Chat, &responses, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	got := responses.String()
	if !strings.Contains(got, `"type":"function_call"`) ||
		!strings.Contains(got, `"call_id":"lookup"`) ||
		!strings.Contains(got, `"name":"lookup"`) {
		t.Fatalf("legacy call missing from Responses output: %s", got)
	}
	if !strings.Contains(got, `"arguments":"{\"q\":1}"`) {
		t.Fatalf("legacy arguments missing from Responses output: %s", got)
	}
	if !strings.Contains(got, `"type":"response.completed"`) || strings.Contains(got, `"type":"error"`) {
		t.Fatalf("legacy call did not complete cleanly: %s", got)
	}

	var anthropic bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &anthropic, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	got = anthropic.String()
	if !strings.Contains(got, `"type":"tool_use"`) || !strings.Contains(got, `"id":"lookup"`) || !strings.Contains(got, `"name":"lookup"`) {
		t.Fatalf("legacy call missing from Anthropic output: %s", got)
	}
	if !strings.Contains(got, `"partial_json":"{\"q\":"`) || !strings.Contains(got, `"partial_json":"1}"`) {
		t.Fatalf("legacy arguments missing from Anthropic output: %s", got)
	}
	if !strings.Contains(got, `"stop_reason":"tool_use"`) || strings.Contains(got, `"type":"error"`) {
		t.Fatalf("legacy call did not finish as a tool turn: %s", got)
	}
}

func TestChatLegacyFunctionCallPrefersToolCalls(t *testing.T) {
	source := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_modern","type":"function","function":{"name":"modern","arguments":"{\"a\":1}"}}],"function_call":{"name":"legacy","arguments":"{\"b\":2}"}},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `"id":"call_modern"`) || !strings.Contains(got, `"name":"modern"`) {
		t.Fatalf("tool_calls did not win the delta: %s", got)
	}
	if strings.Contains(got, `"name":"legacy"`) || strings.Contains(got, `"id":"legacy"`) {
		t.Fatalf("legacy twin was also emitted: %s", got)
	}
	if strings.Count(got, `"type":"tool_use"`) != 1 {
		t.Fatalf("want exactly one tool block: %s", got)
	}
}

func TestChatLegacyFunctionCallRequiresName(t *testing.T) {
	source := chatLegacyStream(`{"arguments":"{\"q\":1}"}`)
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Responses, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "function_call needs a name") {
		t.Fatalf("error = %v, want the missing-name rejection", err)
	}
	if strings.Contains(buf.String(), `"type":"response.completed"`) {
		t.Fatalf("nameless legacy call reached a clean terminal: %s", buf.String())
	}
}
