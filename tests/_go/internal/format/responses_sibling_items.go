package format

// TC3-A: a provider may repeat a call id or item id across distinct items at
// distinct output positions. Announcement identity is the position-qualified
// item, never the shared id alone; a duplicated target tool id still fails
// closed. The non-stream decoder keeps both siblings and must stay unchanged.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const responsesSiblingSharedCallIDStream = terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"dup","name":"first","arguments":""}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"dup","name":"second","arguments":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"dup","name":"first","arguments":"{\"a\":1}"},{"type":"function_call","id":"fc_2","call_id":"dup","name":"second","arguments":"{\"b\":2}"}]}}

`

// TestResponsesStreamSharedCallIDFailsClosed pins the duplicate target id
// boundary: the two items are distinct positions and both are announced, so
// the second reservation must reject the shared target id instead of the
// announcement path silently skipping the sibling.
func TestResponsesStreamSharedCallIDFailsClosed(t *testing.T) {
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Responses, ir.Responses, &buf, strings.NewReader(responsesSiblingSharedCallIDStream), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("stream error = %v, want a duplicate tool call id rejection; output: %s", err, buf.String())
	}
}

const responsesSiblingSharedItemIDStream = terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_shared","call_id":"call_a","name":"first","arguments":""}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_shared","call_id":"call_b","name":"second","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"b\":2}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"function_call","id":"fc_shared","call_id":"call_b","name":"second","arguments":"{\"b\":2}"},{"type":"function_call","id":"fc_shared","call_id":"call_a","name":"first","arguments":"{\"a\":1}"}]}}

`

// TestResponsesStreamFunctionCallSiblingsShareItemID keeps both distinct calls
// when only their item id is shared. The terminal document renumbers them, so
// the call-id aliases must still route each terminal entry to its announcement.
func TestResponsesStreamFunctionCallSiblingsShareItemID(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(responsesSiblingSharedItemIDStream), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("TranslateStreamBetween: %v", err)
	}
	calls := streamedToolCalls(t, ir.Chat, buf.String())
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want two: %s", len(calls), buf.String())
	}
	got := map[string]string{}
	for _, call := range calls {
		got[call.id] = call.name + "/" + strings.TrimSpace(call.args.String())
	}
	want := map[string]string{
		"call_a": "first/{\"a\":1}",
		"call_b": "second/{\"b\":2}",
	}
	for id, expected := range want {
		if got[id] != expected {
			t.Fatalf("call %s = %q, want %q: %v\n%s", id, got[id], expected, got, buf.String())
		}
	}
}

const responsesSiblingSharedMessageIDStream = terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_shared","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"alpha"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_shared","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"beta"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_shared","role":"assistant","content":[{"type":"output_text","text":"alpha"}]},{"type":"message","id":"msg_shared","role":"assistant","content":[{"type":"output_text","text":"beta"}]}]}}

`

// TestResponsesStreamMessageSiblingsShareItemID pins the message sibling case:
// both announced content parts must reach the target once, at their own
// positions, even though the item id repeats.
func TestResponsesStreamMessageSiblingsShareItemID(t *testing.T) {
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(responsesSiblingSharedMessageIDStream), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := strings.Join(textEmissionOrder(t, target, buf.String()), ""); got != "alphabeta" {
				t.Fatalf("text = %q, want %q: %s", got, "alphabeta", buf.String())
			}
			for _, value := range []string{"alpha", "beta"} {
				if got := textDeltaCount(t, target, buf.String(), value); got != 1 {
					t.Fatalf("%q emitted %d times, want once: %s", value, got, buf.String())
				}
			}
		})
	}
}

// TestResponsesSiblingItemsNonStreamKeepsBoth proves the streaming fix does
// not change the non-stream decoder, which already keeps every sibling.
func TestResponsesSiblingItemsNonStreamKeepsBoth(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"m","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"dup","name":"first","arguments":"{}"},` +
		`{"type":"function_call","id":"fc_2","call_id":"dup","name":"second","arguments":"{}"},` +
		`{"type":"message","id":"msg_shared","role":"assistant","content":[{"type":"output_text","text":"alpha"}]},` +
		`{"type":"message","id":"msg_shared","role":"assistant","content":[{"type":"output_text","text":"beta"}]}]}`)
	resp, err := DecodeResponsesResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponsesResponse: %v", err)
	}
	toolNames := []string{}
	texts := []string{}
	for _, block := range resp.Blocks {
		switch block.Type {
		case ir.BlockToolCall:
			toolNames = append(toolNames, block.ToolCall.Name)
		case ir.BlockText:
			texts = append(texts, block.Text)
		}
	}
	if got := strings.Join(toolNames, ","); got != "first,second" {
		t.Fatalf("tool names = %q, want first,second", got)
	}
	if got := strings.Join(texts, ","); got != "alpha,beta" {
		t.Fatalf("text blocks = %q, want alpha,beta", got)
	}
}
