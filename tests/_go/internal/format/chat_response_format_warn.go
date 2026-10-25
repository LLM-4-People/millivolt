package format

// Round 5 A1 regression: an unsupported Chat response_format type is a dropped
// field. decodeChatResponseFormat returns a nil format plus a warning, so the
// warning must be appended outside the non-nil assignment guard; otherwise the
// field disappears silently and reject mode accepts it.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatResponseFormatUnknownTypeWarns(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"grammar"}}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.ResponseFormat != nil {
		t.Fatalf("unsupported format was installed: %+v", req.ResponseFormat)
	}
	if !warningField(req.Warnings, "response_format") {
		t.Fatalf("unsupported response_format recorded no warning: %+v", req.Warnings)
	}
	for _, target := range []ir.Protocol{ir.Responses, ir.Anthropic} {
		_, out, warnings, err := TranslateRequestBetweenIR(ir.Chat, target, []byte(body), 0, "")
		if err != nil {
			t.Fatalf("%s translation: %v", target, err)
		}
		if !warningField(warnings, "response_format") {
			t.Fatalf("%s translation lost the response_format warning: %+v", target, warnings)
		}
		if len(out) == 0 {
			t.Fatalf("%s translation produced no body", target)
		}
	}
}

func TestChatResponseFormatKnownTypesStayUnchanged(t *testing.T) {
	for _, format := range []string{
		`{"type":"text"}`,
		`{"type":"json_object"}`,
		`{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`,
	} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":` + format + `}`
		req, err := DecodeChatRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if req.ResponseFormat == nil {
			t.Fatalf("supported format %s was dropped", format)
		}
		if warningField(req.Warnings, "response_format") {
			t.Fatalf("supported format %s warned: %+v", format, req.Warnings)
		}
	}
}
