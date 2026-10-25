package format

// Round 12 C2 regression: Responses reasoning events route on kind-agnostic
// position caches (itemBlock/keyByOutput). A reasoning summary event at a tool
// or message output index therefore landed on that item's target block and the
// Anthropic encoder emitted thinking_delta inside it.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func reasoningKindSource(item, delta string) string {
	return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":" + item + "}\n\n" +
		delta +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[" + terminalReasoningKindItem(item) + "]}}\n\n"
}

// terminalReasoningKindItem repeats the announced item in the terminal document
// so the stream stays internally consistent; reasoning kind is what matters.
func terminalReasoningKindItem(item string) string {
	if strings.Contains(item, `"function_call"`) {
		return `{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"a\":1}"}`
	}
	return `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`
}

func TestResponsesReasoningRequiresReasoningItem(t *testing.T) {
	reasoningDelta := `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"think"}

`
	for _, tc := range []struct {
		name string
		item string
	}{
		{
			name: "tool item",
			item: `{"type":"function_call","call_id":"call_1","name":"f","arguments":""}`,
		},
		{
			name: "message item",
			item: `{"type":"message","id":"msg_1","role":"assistant","content":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := reasoningKindSource(tc.item, reasoningDelta)
			out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, source)
			if err != nil {
				t.Fatalf("stream failed: %v", err)
			}
			if strings.Contains(out, `"thinking_delta"`) {
				t.Fatalf("reasoning event landed on the %s block: %s", tc.name, out)
			}
			if !strings.Contains(out, "message_start") {
				t.Fatalf("no Anthropic frames: %s", out)
			}
		})
	}

	t.Run("valid reasoning item still streams", func(t *testing.T) {
		source := reasoningKindSource(
			`{"type":"reasoning","id":"rs_1","summary":[]}`,
			reasoningDelta,
		)
		out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, source)
		if err != nil {
			t.Fatalf("valid reasoning failed: %v", err)
		}
		if !strings.Contains(out, `"thinking_delta"`) || !strings.Contains(out, `"thinking":"think"`) {
			t.Fatalf("valid reasoning lost its thinking delta: %s", out)
		}
	})

	t.Run("terminal-only reasoning keeps its thinking content", func(t *testing.T) {
		source := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"think\"}]}]}}\n\n"
		out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, source)
		if err != nil {
			t.Fatalf("terminal-only reasoning failed: %v", err)
		}
		if !strings.Contains(out, `"thinking_delta"`) {
			t.Fatalf("terminal-only reasoning lost its thinking content: %s", out)
		}
	})
}
