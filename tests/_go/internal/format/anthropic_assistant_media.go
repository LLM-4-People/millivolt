package format

// Round 6 A-U regression: a Responses assistant message carrying an image or
// file was emitted inside an Anthropic assistant message with the role
// parameter discarded and no warning. The block is schema-valid (assistant
// content shares MessageContentParam), so keep it and report the unverified
// runtime acceptance. A user image stays warning-free.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicMediaRequest(role ir.Role, block ir.Block) *ir.Request {
	return &ir.Request{
		Model: "m", MaxOutputTokens: intPtr(16),
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
			{Role: role, Content: []ir.Block{block}},
		},
	}
}

func TestAnthropicAssistantMediaWarns(t *testing.T) {
	image := ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "AAAA"}}
	file := ir.Block{Type: ir.BlockFile, File: &ir.File{MediaType: "application/pdf", FileData: "JVBERi0="}}

	for _, tc := range []struct {
		name  string
		role  ir.Role
		block ir.Block
		field string
		want  bool
	}{
		{name: "assistant image", role: ir.RoleAssistant, block: image, field: "assistant.image", want: true},
		{name: "assistant file", role: ir.RoleAssistant, block: file, field: "assistant.document", want: true},
		{name: "user image", role: ir.RoleUser, block: image},
		{name: "user file", role: ir.RoleUser, block: file},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := anthropicMediaRequest(tc.role, tc.block)
			out, err := EncodeAnthropicRequest(req, 16, "")
			if err != nil {
				t.Fatal(err)
			}
			warned := false
			for _, w := range req.Warnings {
				if w.Field == tc.field {
					warned = true
					if w.Kind != ir.WarningApproximated {
						t.Fatalf("warning kind = %q, want approximated", w.Kind)
					}
				}
			}
			if warned != tc.want {
				t.Fatalf("warning %s present = %v, want %v (%+v)", tc.field, warned, tc.want, req.Warnings)
			}
			kind := "image"
			if tc.block.Type == ir.BlockFile {
				kind = "document"
			}
			if !strings.Contains(string(out), `"type":"`+kind+`"`) {
				t.Fatalf("%s block was not kept: %s", kind, out)
			}
		})
	}
}
