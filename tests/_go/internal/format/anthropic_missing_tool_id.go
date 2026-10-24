package format

// Round 2 TC2-4 regressions: Anthropic requires a non-empty tool_use id, so a
// missing id is synthesized (and reported on the non-stream path) instead of
// reaching a target wire empty.

import (
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicMissingToolIDGenerated(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"tool_use","name":"lookup","input":{"q":1}}],` +
			`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
		resp, err := DecodeAnthropicResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Blocks) != 1 || resp.Blocks[0].ToolCall == nil || resp.Blocks[0].ToolCall.ID == "" {
			t.Fatalf("blocks = %+v, want a non-empty tool id", resp.Blocks)
		}
		found := false
		for _, warning := range resp.Warnings {
			if warning.Field == "tool_use.id" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing tool id recorded no warning: %+v", resp.Warnings)
		}

		for _, target := range []ir.Protocol{ir.Chat, ir.Responses} {
			out, _, err := TranslateResponseBetween(target, ir.Anthropic, []byte(body))
			if err != nil {
				t.Fatalf("%s: %v", target, err)
			}
			if !strings.Contains(string(out), `"toolu_proxy_0"`) {
				t.Fatalf("%s body has no synthesized tool id: %s", target, out)
			}
		}
	})

	t.Run("request history", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"assistant","content":[{"type":"tool_use","name":"lookup","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_proxy_0","content":"ok"}]}]}`
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 2 || req.Messages[0].Content[0].ToolCall == nil ||
			req.Messages[0].Content[0].ToolCall.ID == "" {
			t.Fatalf("request blocks = %+v, want a non-empty tool id", req.Messages)
		}
		found := false
		for _, warning := range req.Warnings {
			if warning.Field == "tool_use.id" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing request tool id recorded no warning: %+v", req.Warnings)
		}
	})

	t.Run("stream", func(t *testing.T) {
		source := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"lookup","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
		dec := NewAnthropicStreamDecoder(strings.NewReader(source), nil)
		sawTool := false
		for {
			ev, err := dec.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type == ir.EventBlockStart && ev.Block != nil && ev.Block.Type == ir.BlockToolCall {
				sawTool = true
				if ev.Block.ToolCall == nil || ev.Block.ToolCall.ID == "" {
					t.Fatalf("streamed tool call has an empty id: %+v", ev.Block)
				}
			}
		}
		if !sawTool {
			t.Fatal("streamed tool block never started")
		}
	})
}
