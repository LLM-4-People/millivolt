package format

// B-U regression: an Anthropic response Message.content never carries an
// assistant image or document block. The request encoder keeps its existing
// block-and-approximation behavior because request content does accept media.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicResponseDropsAssistantMedia(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{
			ir.TextBlock("visible"),
			{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
			{Type: ir.BlockFile, File: &ir.File{FileID: "file_1"}},
		},
	}
	out, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"type":"image"`) || strings.Contains(string(out), `"type":"document"`) {
		t.Fatalf("assistant media reached Message.content: %s", out)
	}
	if !strings.Contains(string(out), `"text":"visible"`) {
		t.Fatalf("visible assistant text was lost: %s", out)
	}
	for _, field := range []string{"response.assistant.image", "response.assistant.document"} {
		if !warningField(resp.Warnings, field) {
			t.Fatalf("missing %s drop warning: %+v", field, resp.Warnings)
		}
	}
}

func TestAnthropicRequestKeepsAssistantMedia(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.Block{ir.TextBlock("start")},
		}, {
			Role: ir.RoleAssistant,
			Content: []ir.Block{
				{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
				{Type: ir.BlockFile, File: &ir.File{FileID: "file_1"}},
			},
		}, {
			Role:    ir.RoleUser,
			Content: []ir.Block{ir.TextBlock("hi")},
		}},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"image"`) || !strings.Contains(string(out), `"type":"document"`) {
		t.Fatalf("request assistant media was dropped: %s", out)
	}
	for _, field := range []string{"assistant.image", "assistant.document"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("missing %s approximation warning: %+v", field, req.Warnings)
		}
	}
}
