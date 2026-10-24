package format

// Round 8 A5 regression: the Anthropic DocumentBlockParam title is limited to
// 500 characters. The encoder copied any source filename into document.title,
// so a longer Chat or Responses filename produced a schema-invalid upstream
// request. A bound now drops the title with one warning and keeps the document.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func encodeAnthropicTitleDocument(t *testing.T, filename string) ([]byte, ir.Warnings) {
	t.Helper()
	req := &ir.Request{
		Model: "m", MaxOutputTokens: intPtr(16),
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{
			Type: ir.BlockFile,
			File: &ir.File{
				FileData: "JVBERi0=", MediaType: "application/pdf", Filename: filename,
			},
		}}}},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	return out, req.Warnings
}

func TestAnthropicDocumentTitleBound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		wantKeep bool
		wantWarn bool
	}{
		{name: "500 ascii kept", filename: strings.Repeat("a", 500), wantKeep: true},
		{name: "501 ascii dropped", filename: strings.Repeat("a", 501), wantWarn: true},
		{name: "500 multibyte kept", filename: strings.Repeat("é", 500), wantKeep: true},
		{name: "501 multibyte dropped", filename: strings.Repeat("é", 501), wantWarn: true},
		{name: "no title unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.filename != "" && utf8.RuneCountInString(tc.filename) != 500 &&
				utf8.RuneCountInString(tc.filename) != 501 {
				t.Fatalf("fixture has %d runes", utf8.RuneCountInString(tc.filename))
			}
			out, warnings := encodeAnthropicTitleDocument(t, tc.filename)
			block := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			if block["type"] != "document" {
				t.Fatalf("block = %v, want a document", block)
			}
			title, present := block["title"]
			if present != tc.wantKeep {
				t.Fatalf("title present = %v, want %v (%v)", present, tc.wantKeep, title)
			}
			if tc.wantKeep && title != tc.filename {
				t.Fatalf("title = %v, want the filename kept", title)
			}
			if got := warningCount(warnings, ir.WarningDropped, "document.title"); (got == 1) != tc.wantWarn {
				t.Fatalf("document.title warnings = %d, want warned=%v (%+v)", got, tc.wantWarn, warnings)
			}
		})
	}
}
