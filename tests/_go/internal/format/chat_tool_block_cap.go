package format

// Round 7 C-U(c) regression: the Chat stream decoder tracked every distinct
// tool-call index without a cap, unlike the Anthropic and Responses decoders.
// A source that opens unique indices without closing them could grow the block
// and argument maps until the terminal; the shared toolBlocksMax bound now
// fails the stream instead.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatToolBlockStream(count int) string {
	var b strings.Builder
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call_%d","type":"function","function":{"name":"f%d","arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n", i, i, i)
	}
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestChatStreamToolBlockCountIsBounded(t *testing.T) {
	t.Run("1024 blocks pass", func(t *testing.T) {
		var buf bytes.Buffer
		if err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader(chatToolBlockStream(toolBlocksMax)), nil, nil, StreamOptions{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "data: [DONE]") {
			t.Fatalf("accepted stream has no terminal sentinel")
		}
	})

	t.Run("1025 blocks fail", func(t *testing.T) {
		var buf bytes.Buffer
		err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader(chatToolBlockStream(toolBlocksMax+1)), nil, nil, StreamOptions{})
		if err == nil || !strings.Contains(err.Error(), "concurrent tool calls") {
			t.Fatalf("error = %v, want the tool-block cap", err)
		}
		if strings.Contains(buf.String(), "data: [DONE]") {
			t.Fatalf("over-cap stream reached a clean terminal")
		}
	})

	t.Run("sequential text and reasoning unchanged", func(t *testing.T) {
		source := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"why"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
		var buf bytes.Buffer
		if err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.Contains(out, `"reasoning_content":"why"`) || !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, `"id":"call_1"`) {
			t.Fatalf("sequential content changed: %s", out)
		}
	})
}
