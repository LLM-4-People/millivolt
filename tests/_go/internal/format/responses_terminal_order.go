package format

// R4-2 regressions: terminal items and parts are collected in terminal
// document order. An announced-but-empty later part is deferred, so an earlier
// terminal-only part is not forced behind it. Once a later announced part has
// streamed bytes its target index is already fixed; the terminal fill can then
// only append, never reorder visible content.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalOrderForDeferredParts(t *testing.T) {
	// Part 1 is announced empty; terminal order is part 0 alpha, part 1 beta.
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"alpha"},{"type":"output_text","text":"beta"}]}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := textEmissionOrder(t, target.inbound, buf.String()); strings.Join(got, " ") != "alpha beta" {
				t.Fatalf("order = %v, want [alpha beta]: %s", got, buf.String())
			}
		})
	}
}

func TestResponsesTerminalOrderAfterStreamedLaterPart(t *testing.T) {
	// Part 1 already streamed beta before the terminal document. Its target
	// block index is fixed, so beta cannot move behind the terminal-only
	// alpha; both values still arrive exactly once.
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"beta"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"alpha"},{"type":"output_text","text":"beta"}]}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			order := textEmissionOrder(t, target.inbound, buf.String())
			if strings.Join(order, " ") != "beta alpha" {
				t.Fatalf("order = %v, want already-streamed [beta alpha]: %s", order, buf.String())
			}
			for _, value := range []string{"alpha", "beta"} {
				if got := textDeltaCount(t, target.inbound, buf.String(), value); got != 1 {
					t.Fatalf("%q emitted %d times, want once: %s", value, got, buf.String())
				}
			}
		})
	}
}

// textEmissionOrder returns the non-empty answer-text units in emission order.
func textEmissionOrder(t *testing.T, protocol ir.Protocol, out string) []string {
	t.Helper()
	var order []string
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] != "response.output_text.delta" {
				continue
			}
			if text, _ := payload["delta"].(string); text != "" {
				order = append(order, text)
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "text_delta" {
				if text, _ := delta["text"].(string); text != "" {
					order = append(order, text)
				}
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			if text, ok := chatDeltaContent(payload); ok && text != "" {
				order = append(order, text)
			}
		}
	}
	return order
}

// TestResponsesEmptyDeltaKeepsDeferredPart pins F5-4: an empty delta carries
// no bytes and must not start a deferred part. If it did, its target position
// would be fixed and an earlier terminal-only part would be forced behind it.
func TestResponsesEmptyDeltaKeepsDeferredPart(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":""}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"alpha"},{"type":"output_text","text":"beta"}]}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := textEmissionOrder(t, target.inbound, buf.String()); strings.Join(got, " ") != "alpha beta" {
				t.Fatalf("order = %v, want [alpha beta]: %s", got, buf.String())
			}
			for _, value := range []string{"alpha", "beta"} {
				if got := textDeltaCount(t, target.inbound, buf.String(), value); got != 1 {
					t.Fatalf("%q emitted %d times, want once: %s", value, got, buf.String())
				}
			}
		})
	}
}

// TestResponsesEmptyContentPartDoneOpensNoBlock pins F5-5: an empty done for a
// deferred part must keep it deferred. Starting it just to close it would open
// an empty target block (visible as an empty Anthropic text block) and fix its
// position before terminal content exists.
func TestResponsesEmptyContentPartDoneOpensNoBlock(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"alpha"},{"type":"output_text","text":"beta"}]}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := textEmissionOrder(t, target.inbound, buf.String()); strings.Join(got, " ") != "alpha beta" {
				t.Fatalf("order = %v, want [alpha beta]: %s", got, buf.String())
			}
			if target.inbound == ir.Anthropic {
				if got := emptyAnthropicTextBlocks(t, buf.String()); got != 0 {
					t.Fatalf("empty Anthropic text blocks = %d, want 0: %s", got, buf.String())
				}
			}
		})
	}
}

// emptyAnthropicTextBlocks counts Anthropic text blocks that started but never
// received a non-empty text delta.
func emptyAnthropicTextBlocks(t *testing.T, out string) int {
	t.Helper()
	starts := map[int]bool{}
	deltas := map[int]bool{}
	for _, payload := range mustAnthropicPayloads(t, out) {
		switch payload["type"] {
		case "content_block_start":
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] != "text" {
				continue
			}
			idx, _ := payload["index"].(float64)
			starts[int(idx)] = true
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] != "text_delta" {
				continue
			}
			if text, _ := delta["text"].(string); text == "" {
				continue
			}
			idx, _ := payload["index"].(float64)
			deltas[int(idx)] = true
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
