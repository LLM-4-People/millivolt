package format

// Round 12 C4 regression: Responses stream positions are non-negative. The
// decoder narrowed output_index/content_index/summary_index without a sign
// check, while the part-coverage owner rejects a negative trailing index, so
// streamed bytes were never recorded as covered and the terminal copy repeated
// them.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const negativeIndexPrefix = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"

func TestResponsesNegativeStreamIndexes(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{
			name: "output index",
			source: negativeIndexPrefix +
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":-1,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n",
		},
		{
			name: "content index",
			source: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
				"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":-1,\"delta\":\"hello\"}\n\n" +
				"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":-1,\"text\":\"hello\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n",
		},
		{
			name: "summary index",
			source: negativeIndexPrefix +
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[]}}\n\n" +
				"event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"summary_index\":-1,\"delta\":\"think\"}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := translateStreamSource(t, ir.Responses, ir.Chat, tc.source)
			if err == nil {
				t.Fatalf("negative index was accepted: %s", out)
			}
			if !strings.Contains(err.Error(), "negative") {
				t.Fatalf("error = %v, want the negative-index rejection", err)
			}
			if strings.Count(out, `"hello"`) > 1 {
				t.Fatalf("rejected index duplicated visible content: %s", out)
			}
		})
	}

	t.Run("zero index control", func(t *testing.T) {
		source := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
			"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n" +
			"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":\"hello\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n"
		out, err := translateStreamSource(t, ir.Responses, ir.Chat, source)
		if err != nil {
			t.Fatalf("zero-index control failed: %v", err)
		}
		if got := strings.Count(out, "hello"); got != 1 {
			t.Fatalf("zero-index control emitted hello %d times, want 1: %s", got, out)
		}
	})
}
