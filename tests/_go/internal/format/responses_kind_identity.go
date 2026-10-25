package format

// Round 2 A1/A5 regression: an item id is scoped by item kind. A message and a
// reasoning item may share the same provider id, and an id-less function call
// may share its item id with reasoning. Neither collision may synthesize a
// duplicate block or route reasoning bytes onto a tool block.

import (
	"bytes"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicBlockStarts(t *testing.T, out string) map[string]int {
	t.Helper()
	starts := map[string]int{}
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "content_block_start" {
			continue
		}
		block, _ := payload["content_block"].(map[string]any)
		typ, _ := block["type"].(string)
		starts[typ]++
	}
	return starts
}

func anthropicThinkingDeltaIndexes(t *testing.T, out string) []int {
	t.Helper()
	var indexes []int
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "content_block_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta["type"] != "thinking_delta" {
			continue
		}
		index, _ := payload["index"].(float64)
		indexes = append(indexes, int(index))
	}
	return indexes
}

func anthropicBlockKinds(t *testing.T, out string) map[int]string {
	t.Helper()
	kinds := map[int]string{}
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "content_block_start" {
			continue
		}
		index, _ := payload["index"].(float64)
		block, _ := payload["content_block"].(map[string]any)
		typ, _ := block["type"].(string)
		kinds[int(index)] = typ
	}
	return kinds
}

func TestResponsesKindQualifiedItemIdentity(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"shared","role":"assistant","content":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"shared","summary":[]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"shared","summary":[{"type":"summary_text","text":"why"}]},{"type":"message","id":"shared","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, bytes.NewBufferString(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("TranslateStreamBetween: %v", err)
	}
	if starts := anthropicBlockStarts(t, buf.String()); starts["text"] != 1 || starts["thinking"] != 1 || len(starts) != 2 {
		t.Fatalf("block starts = %v, want one text and one thinking: %s", starts, buf.String())
	}
	if got := anthropicThinkingDeltaIndexes(t, buf.String()); len(got) != 1 {
		t.Fatalf("thinking deltas = %v, want exactly one: %s", got, buf.String())
	}
}

func TestResponsesReasoningDoesNotReuseToolIdentity(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"shared","name":"lookup","arguments":"{}"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"shared","summary":[]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"shared","summary":[{"type":"summary_text","text":"why"}]},{"type":"function_call","id":"shared","name":"lookup","arguments":"{}"}]}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, bytes.NewBufferString(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("TranslateStreamBetween: %v", err)
	}
	if starts := anthropicBlockStarts(t, buf.String()); starts["tool_use"] != 1 || starts["thinking"] != 1 || len(starts) != 2 {
		t.Fatalf("block starts = %v, want one tool_use and one thinking: %s", starts, buf.String())
	}
	kinds := anthropicBlockKinds(t, buf.String())
	deltas := anthropicThinkingDeltaIndexes(t, buf.String())
	if len(deltas) != 1 {
		t.Fatalf("thinking deltas = %v, want exactly one: %s", deltas, buf.String())
	}
	if kinds[deltas[0]] != "thinking" {
		t.Fatalf("thinking delta index %d is a %q block, want thinking: %s", deltas[0], kinds[deltas[0]], buf.String())
	}
}
