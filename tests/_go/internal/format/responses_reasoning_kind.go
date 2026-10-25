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

	t.Run("reasoning text at a tool index", func(t *testing.T) {
		source := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"\"}}\n\n" +
			"event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"think\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"{}\",\"status\":\"completed\"}]}}\n\n"
		out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, source)
		if err != nil {
			t.Fatalf("stream failed: %v", err)
		}
		if strings.Contains(out, `"thinking_delta"`) {
			t.Fatalf("reasoning_text landed on an item that is not reasoning: %s", out)
		}
	})

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

// TestResponsesReasoningGuardsDropAtNonReasoningIndexes pins every
// summary/content terminal handler, not only the delta handlers: an event at
// a tool or message output index is dropped, while a real reasoning item
// still emits its thinking text once and closes once.
func TestResponsesReasoningGuardsDropAtNonReasoningIndexes(t *testing.T) {
	handlers := []struct {
		name  string
		event string
	}{
		{
			name: "summary part added",
			event: `event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"think"}}

`,
		},
		{
			name: "summary text done",
			event: `event: response.reasoning_summary_text.done
data: {"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"think"}

`,
		},
		{
			name: "reasoning text done",
			event: `event: response.reasoning_text.done
data: {"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"think"}

`,
		},
		{
			name: "summary part done",
			event: `event: response.reasoning_summary_part.done
data: {"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"think"},"text":"think"}

`,
		},
	}
	items := []struct {
		name string
		item string
	}{
		{"tool", `{"type":"function_call","call_id":"call_1","name":"f","arguments":""}`},
		{"message", `{"type":"message","id":"msg_1","role":"assistant","content":[]}`},
		{"reasoning", `{"type":"reasoning","id":"rs_1","summary":[]}`},
	}
	for _, handler := range handlers {
		for _, item := range items {
			t.Run(handler.name+"/"+item.name, func(t *testing.T) {
				source := reasoningKindSource(item.item, handler.event)
				out, err := translateStreamSource(t, ir.Responses, ir.Anthropic, source)
				if err != nil {
					t.Fatalf("stream failed: %v", err)
				}
				if n := strings.Count(out, "event: message_stop"); n != 1 {
					t.Fatalf("message_stop count = %d, want 1: %s", n, out)
				}
				hasThinking := strings.Contains(out, `"thinking_delta"`)
				if item.name != "reasoning" {
					if hasThinking {
						t.Fatalf("%s event landed on a %s block: %s", handler.name, item.name, out)
					}
					return
				}
				if !hasThinking || !strings.Contains(out, `"thinking":"think"`) {
					t.Fatalf("%s lost the valid reasoning text: %s", handler.name, out)
				}
			})
		}
	}
}
