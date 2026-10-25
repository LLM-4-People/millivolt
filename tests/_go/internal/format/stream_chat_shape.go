package format

// Round 11 C4 regression: the Chat stream decoder accepts only chunk
// documents. A full completion (or a choice carrying message instead of
// delta) used to yield a successful empty answer because the decoder never
// reads message.content.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatStreamRejectsNonChunkBody(t *testing.T) {
	sources := []struct {
		name   string
		source string
	}{
		{
			name: "full completion document",
			source: "data: {\"id\":\"c\",\"object\":\"chat.completion\",\"model\":\"m\",\"created\":1," +
				"\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"answering\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "message instead of delta without object",
			source: "data: {\"id\":\"c\",\"model\":\"m\",\"created\":1," +
				"\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"answering\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, tc := range sources {
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
			t.Run(tc.name+"/"+string(target), func(t *testing.T) {
				out, err := translateStreamSource(t, ir.Chat, target, tc.source)
				if err == nil {
					t.Fatalf("non-chunk body accepted for target %s: %s", target, out)
				}
				if strings.Contains(out, "answering") {
					t.Fatalf("complete answer reached the wire without an error: %s", out)
				}
			})
		}
	}
}

func TestChatStreamChunkControl(t *testing.T) {
	source := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"created\":1," +
		"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"created\":1," +
		"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			out, err := translateStreamSource(t, ir.Chat, target, source)
			if err != nil {
				t.Fatalf("chunk control rejected: %v", err)
			}
			if !strings.Contains(out, "hello") {
				t.Fatalf("chunk control lost its content: %s", out)
			}
		})
	}
}
