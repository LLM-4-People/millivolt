package format

// H7 regression: Chat stream created accepts the schema's integral spellings
// and carries the value into translated targets.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatStreamCreatedIntegralSpelling(t *testing.T) {
	source := `data: {"id":"c1","object":"chat.completion.chunk","created":1.0,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1.0,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var responses bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Chat, &responses, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(responses.String(), `"created_at":1`) {
		t.Fatalf("Responses stream did not carry created 1: %s", responses.String())
	}
	var chat bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Chat, &chat, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat.String(), `"created":1`) {
		t.Fatalf("Chat stream did not carry created 1: %s", chat.String())
	}
}
