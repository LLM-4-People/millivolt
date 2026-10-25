package format

// Round 2 TC8-5 pin: an announced empty reasoning item that closes before its
// terminal summary parts leaves one empty Anthropic thinking block. This is a
// cosmetic residual; suppressing it would risk ordering and signature loss.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func emptyAnthropicThinkingBlocks(t *testing.T, out string) int {
	t.Helper()
	starts := map[int]bool{}
	deltas := map[int]bool{}
	for _, payload := range mustAnthropicPayloads(t, out) {
		typ, _ := payload["type"].(string)
		switch typ {
		case "content_block_start":
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] != "thinking" {
				continue
			}
			idx, _ := payload["index"].(float64)
			starts[int(idx)] = true
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] != "thinking_delta" {
				continue
			}
			if text, _ := delta["thinking"].(string); text != "" {
				idx, _ := payload["index"].(float64)
				deltas[int(idx)] = true
			}
		}
	}
	empty := 0
	for idx := range starts {
		if !deltas[idx] {
			empty++
		}
	}
	return empty
}

func TestResponsesEmptyAnnouncedReasoningPinsThinkingBlock(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"},{"type":"summary_text","text":"beta"}]}]}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reasoningEmissionOrder(t, ir.Anthropic, buf.String()), ""); got != "alphabeta" {
		t.Fatalf("reasoning = %q, want alphabeta: %s", got, buf.String())
	}
	if empty := emptyAnthropicThinkingBlocks(t, buf.String()); empty != 1 {
		t.Fatalf("empty thinking blocks = %d, want the pinned 1: %s", empty, buf.String())
	}
}
