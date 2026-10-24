package format

// Round 6 A4 regression: the Responses request-input call_id is 1..64
// characters. A Chat history tool call with a longer id was emitted verbatim in
// both function_call and function_call_output, producing a wire the target
// rejects.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesToolHistoryRequest(id string) *ir.Request {
	return &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: id, Name: "lookup", Arguments: []byte(`{}`)}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{CallID: id, Content: []ir.Block{ir.TextBlock("ok")}}},
			}},
		},
	}
}

func TestResponsesTargetRejectsOverlongToolCallID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "one byte", id: "a"},
		{name: "64 bytes", id: strings.Repeat("a", 64)},
		{name: "65 bytes", id: strings.Repeat("a", 65), wantErr: true},
		{name: "513 bytes", id: strings.Repeat("a", 513), wantErr: true},
		{name: "empty", id: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := EncodeResponsesRequest(responsesToolHistoryRequest(tc.id))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("over-bound call id was accepted: %s", body)
				}
				// The empty id is also rejected by ir.ValidateRequest, which
				// runs first; any fail-closed error satisfies the contract.
				if tc.id != "" && !strings.Contains(err.Error(), "1..64 bytes") {
					t.Fatalf("error = %v, want the Responses call-id bound", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"call_id":"`+tc.id+`"`) {
				t.Fatalf("accepted id is missing from %s", body)
			}
		})
	}
}
