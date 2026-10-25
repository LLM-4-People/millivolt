package format

// Round 3 batch 13 regressions (B3): the Responses wire's raw error member is
// read through the shared envelope parser, so a string error or a numeric code
// no longer fails the typed decode and degrades to a generic transform_error.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesNonStreamStringErrorEnvelope(t *testing.T) {
	resp, err := DecodeResponsesResponse([]byte(`{"id":"r1","status":"completed","error":"boom","output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatal("string error envelope was not classified")
	}
	if resp.Error.Type != "provider_error" || resp.Error.Message != "boom" {
		t.Fatalf("error = %+v, want provider_error/boom", resp.Error)
	}
}

func TestResponsesNonStreamNumericErrorCode(t *testing.T) {
	resp, err := DecodeResponsesResponse([]byte(`{"id":"r2","status":"failed","error":{"code":500,"type":"server_error","message":"boom"},"output":[]}`))
	if err != nil {
		t.Fatalf("numeric error code rejected by the typed decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "500" || resp.Error.Type != "server_error" || resp.Error.Message != "boom" {
		t.Fatalf("error = %+v, want server_error/500/boom", resp.Error)
	}
	if resp.Finish != ir.FinishError {
		t.Fatalf("finish = %q, want error", resp.Finish)
	}
}

func TestResponsesStreamFailedNumericError(t *testing.T) {
	source := `event: response.created
data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}

event: response.failed
data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":500,"type":"server_error","message":"boom"}}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"code":"500"`) || !strings.Contains(out, `"boom"`) {
		t.Fatalf("stream failure lost the provider error: %s", out)
	}
}
