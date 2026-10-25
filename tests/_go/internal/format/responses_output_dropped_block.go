package format

// Round 8 B-U regression: a canonical block with no Responses output part was
// silently dropped. An Anthropic document carried into a Responses response
// vanished with no warning, so a client could not tell the answer had lost an
// attachment. The default encoder case now reports the dropped type once.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicDocumentResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[` +
	`{"type":"text","text":"look"},` +
	`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func TestResponsesOutputWarnsOnDroppedDocument(t *testing.T) {
	resp, err := DecodeAnthropicResponse([]byte(anthropicDocumentResponse))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"type":"file"`) || strings.Contains(string(out), `"type":"document"`) {
		t.Fatalf("document reached the Responses output: %s", out)
	}
	if !strings.Contains(string(out), `"text":"look"`) {
		t.Fatalf("text block was lost: %s", out)
	}
	if got := warningCount(resp.Warnings, ir.WarningDropped, "output.file"); got != 1 {
		t.Fatalf("output.file warnings = %d, want 1 (%+v)", got, resp.Warnings)
	}

	chatResp, err := DecodeAnthropicResponse([]byte(anthropicDocumentResponse))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := EncodeChatResponse(chatResp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chat), `"type":"file"`) {
		t.Fatalf("document reached the Chat output: %s", chat)
	}
	if !warningField(chatResp.Warnings, "content.file") {
		t.Fatalf("existing Chat file warning disappeared: %+v", chatResp.Warnings)
	}
}
