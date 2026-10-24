package format

// Round 8 A3 regression: a Responses input_file can carry raw base64 in
// file_data with no media-type declaration. The decoder used to leave the type
// empty, so the Anthropic and Chat encoders dropped the bytes. One owner now
// infers the type from the data URL, the filename or the base64 PDF signature.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesFileRequest(parts string) string {
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` + parts + `]}]}`
}

func responsesFilePart(fields string) string {
	return `{"type":"input_file",` + fields + `}`
}

func anthropicDocument(t *testing.T, out []byte) map[string]any {
	t.Helper()
	for _, raw := range anthropicWireMessages(t, out) {
		content, _ := raw["content"].([]any)
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block["type"] == "document" {
				return block
			}
		}
	}
	t.Fatalf("no Anthropic document block: %s", out)
	return nil
}

func chatFilePart(t *testing.T, out []byte) map[string]any {
	t.Helper()
	content, _ := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	for _, item := range content {
		part, _ := item.(map[string]any)
		if part["type"] == "file" {
			return part["file"].(map[string]any)
		}
	}
	return nil
}

func TestResponsesRawBase64FileDataInfersMediaType(t *testing.T) {
	t.Run("pdf filename", func(t *testing.T) {
		body := responsesFileRequest(`{"type":"input_text","text":"look"},` +
			responsesFilePart(`"filename":"a.pdf","file_data":"JVBERi0="`))
		chat, _, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		file := chatFilePart(t, chat)
		if file == nil || file["file_data"] != "data:application/pdf;base64,JVBERi0=" {
			t.Fatalf("Chat file part = %v, want the PDF data URL", file)
		}

		anthropic, _, err := TranslateRequestBetween(ir.Responses, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		source := anthropicDocument(t, anthropic)["source"].(map[string]any)
		if source["type"] != "base64" || source["media_type"] != "application/pdf" ||
			source["data"] != "JVBERi0=" {
			t.Fatalf("Anthropic source = %v, want base64 PDF", source)
		}
	})

	t.Run("text filename", func(t *testing.T) {
		body := responsesFileRequest(`{"type":"input_text","text":"look"},` +
			responsesFilePart(`"filename":"notes.txt","file_data":"bm90ZXM="`))
		anthropic, warnings, err := TranslateRequestBetween(ir.Responses, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		source := anthropicDocument(t, anthropic)["source"].(map[string]any)
		if source["type"] != "text" || source["media_type"] != "text/plain" || source["data"] != "notes" {
			t.Fatalf("Anthropic source = %v, want plain text", source)
		}
		if warningField(warnings, "file.media_type") {
			t.Fatalf("plain text document warned: %+v", warnings)
		}

		chat, warnings, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if file := chatFilePart(t, chat); file != nil {
			t.Fatalf("Chat kept a non-PDF file: %v", file)
		}
		message := decodeJSONMap(t, chat)["messages"].([]any)[0].(map[string]any)
		if content, ok := message["content"].(string); !ok || content != "look" {
			t.Fatalf("Chat content = %#v, want the text part", message["content"])
		}
		if !warningField(warnings, "file.media_type") {
			t.Fatalf("Chat non-PDF drop produced no warning: %+v", warnings)
		}
	})

	t.Run("pdf signature without a filename", func(t *testing.T) {
		body := responsesFileRequest(`{"type":"input_text","text":"look"},` +
			responsesFilePart(`"file_data":"JVBERi0="`))
		anthropic, _, err := TranslateRequestBetween(ir.Responses, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		source := anthropicDocument(t, anthropic)["source"].(map[string]any)
		if source["type"] != "base64" || source["media_type"] != "application/pdf" {
			t.Fatalf("Anthropic source = %v, want the PDF signature inference", source)
		}
	})

	t.Run("unknown bytes drop only the inline data", func(t *testing.T) {
		body := responsesFileRequest(`{"type":"input_text","text":"look"},` +
			responsesFilePart(`"filename":"data.bin","file_data":"AAAA"`))
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
			out, warnings, err := TranslateRequestBetween(ir.Responses, target, []byte(body), 16, "")
			if err != nil {
				t.Fatalf("%s: unknown inline bytes failed the whole request: %v", target, err)
			}
			if !strings.Contains(string(out), "look") {
				t.Fatalf("%s: text part was lost: %s", target, out)
			}
			if got := warningCount(warnings, ir.WarningDropped, "content.file_data"); got != 1 {
				t.Fatalf("%s: content.file_data warnings = %d, want 1 (%+v)", target, got, warnings)
			}
		}
	})

	t.Run("unknown bytes keep a file id", func(t *testing.T) {
		body := responsesFileRequest(`{"type":"input_text","text":"look"},` +
			responsesFilePart(`"file_id":"file_1","file_data":"AAAA"`))
		out, warnings, err := TranslateRequestBetween(ir.Responses, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		source := anthropicDocument(t, out)["source"].(map[string]any)
		if source["type"] != "file" || source["file_id"] != "file_1" {
			t.Fatalf("Anthropic source = %v, want the surviving file id", source)
		}
		if got := warningCount(warnings, ir.WarningDropped, "content.file_data"); got != 1 {
			t.Fatalf("content.file_data warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("data url unchanged", func(t *testing.T) {
		body := responsesFileRequest(`{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0="}`)
		chat, _, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		file := chatFilePart(t, chat)
		if file == nil || file["file_data"] != "data:application/pdf;base64,JVBERi0=" {
			t.Fatalf("Chat file part = %v, want the data URL unchanged", file)
		}
	})
}
