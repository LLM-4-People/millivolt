package format

// Round 7 verifier NEW-2 regression: dropping a leading assistant turn must
// also drop the tool results that answered its tool calls. Anthropic requires
// every tool_result to reference a preceding assistant tool_use, so keeping the
// result produced an orphan after the A2 fix. Sibling user text is preserved and
// the loss is reported once.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicWireMessages(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	raw, _ := decodeJSONMap(t, out)["messages"].([]any)
	messages := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		message, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("message is not an object: %v", item)
		}
		messages = append(messages, message)
	}
	return messages
}

func anthropicToolUse(arguments string, ids ...string) ir.Block {
	return ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
		ID: ids[0], Name: "lookup", Arguments: json.RawMessage(arguments),
	}}
}

func anthropicToolResult(callID, text string) ir.Block {
	return ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
		CallID: callID, Content: []ir.Block{ir.TextBlock(text)},
	}}
}

func TestAnthropicTargetDropsOrphanToolResults(t *testing.T) {
	t.Run("chat leading tool pair", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"result"},` +
			`{"role":"user","content":"next"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"tool_use_id":"call_1"`) {
			t.Fatalf("orphan tool_result reached the wire: %s", out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 1 || messages[0]["role"] != "user" || messages[0]["content"] != "next" {
			t.Fatalf("messages = %v, want only the next user turn", messages)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.orphan_tool_result"); got != 1 {
			t.Fatalf("orphan tool_result warnings = %d, want 1 (%+v)", got, warnings)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.leading_assistant"); got != 1 {
			t.Fatalf("leading assistant warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("sibling text survives and warns once", func(t *testing.T) {
		req := &ir.Request{
			Model: "m", MaxOutputTokens: intPtr(16),
			Messages: []ir.Message{
				{Role: ir.RoleAssistant, Content: []ir.Block{
					anthropicToolUse(`{"a":1}`, "call_1"),
					anthropicToolUse(`{"b":2}`, "call_2"),
				}},
				{Role: ir.RoleUser, Content: []ir.Block{
					anthropicToolResult("call_1", "one"),
					anthropicToolResult("call_2", "two"),
					ir.TextBlock("keep me"),
				}},
			},
		}
		out, err := EncodeAnthropicRequest(req, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"tool_use_id"`) {
			t.Fatalf("orphan tool results reached the wire: %s", out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 1 || messages[0]["role"] != "user" || messages[0]["content"] != "keep me" {
			t.Fatalf("messages = %v, want the surviving user text", messages)
		}
		if got := warningCount(req.Warnings, ir.WarningDropped, "messages.orphan_tool_result"); got != 1 {
			t.Fatalf("orphan warnings = %d, want 1 (%+v)", got, req.Warnings)
		}
	})

	t.Run("kept tool pair is unchanged", func(t *testing.T) {
		req := &ir.Request{
			Model: "m", MaxOutputTokens: intPtr(16),
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("start")}},
				{Role: ir.RoleAssistant, Content: []ir.Block{anthropicToolUse(`{"a":1}`, "call_1")}},
				{Role: ir.RoleUser, Content: []ir.Block{
					anthropicToolResult("call_1", "one"),
					ir.TextBlock("done"),
				}},
			},
		}
		out, err := EncodeAnthropicRequest(req, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"tool_use_id":"call_1"`) {
			t.Fatalf("kept tool result was dropped: %s", out)
		}
		if warningField(req.Warnings, "messages.orphan_tool_result") {
			t.Fatalf("kept tool pair warned as orphan: %+v", req.Warnings)
		}
		if got := warningField(req.Warnings, "messages.leading_assistant"); got {
			t.Fatalf("kept leading user turn warned: %+v", req.Warnings)
		}
	})
}
