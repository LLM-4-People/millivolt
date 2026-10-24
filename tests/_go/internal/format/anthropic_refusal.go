package format

// Round 3 batch 11 regression (A4/B4): Anthropic text blocks require
// minLength 1, so an empty refusal must not become an invalid empty text
// block.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicEmptyRefusalDropped(t *testing.T) {
	resp := &ir.Response{
		ID:    "msg_1",
		Model: "m",
		Blocks: []ir.Block{
			{Type: ir.BlockRefusal, Refusal: ""},
			{Type: ir.BlockText, Text: "visible"},
		},
		Finish: ir.FinishStop,
	}
	body, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, body)
	content, _ := doc["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want only the visible text block", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "visible" {
		t.Fatalf("content block = %v, want the visible text", block)
	}
	if !warningField(resp.Warnings, "content.refusal") {
		t.Fatalf("empty refusal drop warning missing: %+v", resp.Warnings)
	}

	// A non-empty refusal still becomes text and keeps its cache breakpoint.
	cache := &ir.CacheControl{Type: "ephemeral"}
	resp = &ir.Response{
		ID:    "msg_2",
		Model: "m",
		Blocks: []ir.Block{
			{Type: ir.BlockRefusal, Refusal: "no", CacheControl: cache},
		},
		Finish: ir.FinishStop,
	}
	body, err = EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	content, _ = decodeJSONMap(t, body)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("non-empty refusal content = %v, want one text block", content)
	}
	block = content[0].(map[string]any)
	if block["text"] != "no" {
		t.Fatalf("non-empty refusal = %v, want text no", block)
	}
	control, _ := block["cache_control"].(map[string]any)
	if control["type"] != "ephemeral" {
		t.Fatalf("non-empty refusal lost its cache control: %v", block)
	}
}
