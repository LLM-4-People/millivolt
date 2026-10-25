package format

// Round 2 B4 regression: a non-streaming response must not encode two target
// tool calls with the same id. The stream decoder already fails closed; the
// shared non-stream encode boundary must enforce the same contract.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const duplicateNonStreamToolCalls = `{"id":"resp_1","model":"m","status":"completed","output":[` +
	`{"type":"function_call","id":"fc_1","call_id":"dup","name":"first","arguments":"{}"},` +
	`{"type":"function_call","id":"fc_2","call_id":"dup","name":"second","arguments":"{}"}]}`

func TestResponsesNonStreamDuplicateTargetToolIDsFailClosed(t *testing.T) {
	out, _, err := TranslateResponseBetween(ir.Chat, ir.Responses, []byte(duplicateNonStreamToolCalls))
	if err == nil {
		t.Fatalf("non-stream duplicate target ids encoded successfully with %d duplicate ids: %s",
			strings.Count(string(out), `"id":"dup"`), out)
	}
	if !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("non-stream duplicate error = %v, want a duplicate tool call id rejection", err)
	}
}

func TestEncodeResponseRejectsDuplicateToolIDs(t *testing.T) {
	resp := &ir.Response{
		ID:     "resp_1",
		Model:  "m",
		Status: "completed",
		Blocks: []ir.Block{
			{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: "dup", Name: "first", Arguments: []byte(`{}`)}},
			{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: "dup", Name: "second", Arguments: []byte(`{}`)}},
		},
	}
	if _, err := EncodeResponse(ir.Chat, resp); err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("EncodeResponse duplicate ids = %v, want a fail-closed rejection", err)
	}
}
