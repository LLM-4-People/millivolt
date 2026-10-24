package format

// Round 3 batch 16 regressions (H2): the Chat target's tool-name vocabulary is
// enforced on history, non-stream responses and stream tool-call starts.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func longToolName(n int) string { return strings.Repeat("a", n) }

func TestChatRejectsOverlongHistoryToolName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		wantErr bool
	}{
		{name: "64 accepted", tool: longToolName(64)},
		{name: "65 rejected", tool: longToolName(65), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model: "m",
				Messages: []ir.Message{
					{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
					{Role: ir.RoleAssistant, Content: []ir.Block{{
						Type: ir.BlockToolCall,
						ToolCall: &ir.ToolCall{
							ID: "call_1", Name: tc.tool, Arguments: []byte(`{}`),
						},
					}}},
				},
			}
			_, err := EncodeChatRequest(req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestChatRejectsOverlongResponseToolName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		wantErr bool
	}{
		{name: "64 accepted", tool: longToolName(64)},
		{name: "65 rejected", tool: longToolName(65), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &ir.Response{
				ID: "c1", Model: "m", Finish: ir.FinishToolCalls,
				Blocks: []ir.Block{{
					Type: ir.BlockToolCall,
					ToolCall: &ir.ToolCall{
						ID: "call_1", Name: tc.tool, Arguments: []byte(`{}`),
					},
				}},
			}
			_, err := EncodeChatResponse(resp)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestChatStreamRejectsOverlongToolName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		wantErr bool
	}{
		{name: "64 accepted", tool: longToolName(64)},
		{name: "65 rejected", tool: longToolName(65), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			enc := NewChatStreamEncoder(&buf, nil, false)
			err := enc.Write(&ir.Event{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{
				Type: ir.BlockToolCall,
				ToolCall: &ir.ToolCall{
					ID: "call_1", Name: tc.tool, Arguments: []byte(`{}`),
				},
			}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && buf.Len() != 0 {
				t.Fatalf("rejected tool name emitted %q", buf.String())
			}
		})
	}
}
