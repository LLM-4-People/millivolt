package proxy

// Regression test for the translated-stream retained-initial budget: a
// committed-200 Anthropic stream that opens tool blocks carrying large inline
// input JSON must close in-band with the stable translation_error chunk, not
// retain the bytes or surface a fabricated success/truncation.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/sse"
)

// translatedToolStartLine renders one Anthropic content_block_start line with
// the given input JSON value.
func translatedToolStartLine(index int, input string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"t%d","name":"n","input":%s}}`, index, index, input)
}

// TestTranslatedStreamRetainedToolInputClosesInBand sends five open tool
// blocks whose inline inputs total more than the retained-initial budget. The
// translator aborts before the fifth block is retained, and the committed
// response closes with exactly one translation_error frame plus [DONE].
func TestTranslatedStreamRetainedToolInputClosesInBand(t *testing.T) {
	const blocks = 5
	input := strings.Repeat("x", 900*1024)
	var native strings.Builder
	native.Grow(blocks * (len(input) + 160))
	native.WriteString(`data: {"type":"message_start","message":{"id":"msg-input","model":"claude-test","usage":{"input_tokens":4}}}` + "\n\n")
	for i := 0; i < blocks; i++ {
		native.WriteString("data: " + translatedToolStartLine(i, `"`+input+`"`) + "\n\n")
	}
	upstream, srv, buf := translatedSSEFixture(t, native.String())

	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200 (body %s)", status, body)
	}
	payloads := translatedSSEPayloads(t, body)
	if got := translatedSSEErrorFrames(payloads); got != 1 {
		t.Fatalf("in-band error frames = %d, want exactly 1 (body %s)", got, body)
	}
	if payloads[len(payloads)-1] != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE] (body %s)", payloads[len(payloads)-1], body)
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payloads[len(payloads)-2]), &envelope); err != nil {
		t.Fatalf("penultimate frame is not the error chunk: %v (%q)", err, payloads[len(payloads)-2])
	}
	if envelope.Error.Type != sse.TypeUpstreamError || envelope.Error.Code != translationErrorCode {
		t.Fatalf("error chunk = %+v, want %s/%s", envelope.Error, sse.TypeUpstreamError, translationErrorCode)
	}
	if strings.Contains(body, "retained tool input") {
		t.Fatalf("translator internals leaked to the client: %s", body)
	}

	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want 1", len(recs))
	}
	if !recs[0].IsError() || recs[0].FinishReason != "" || recs[0].HadAnswerContent {
		t.Fatalf("failed translation classified as success: %+v", recs[0])
	}
}
