package format

// A3/H6 regressions: system placement is judged on emitted turns. An empty
// assistant turn must not keep a system run between two emitted user turns,
// while a valid tool_result -> system -> assistant position and an emitted
// encrypted reasoning assistant stay in place.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicSystemAfterEmptyAssistantIsLifted(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"user","content":"u1"},` +
		`{"role":"system","content":"SYS"},` +
		`{"role":"assistant","content":""},` +
		`{"role":"user","content":"u2"}]}`
	out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONMap(t, out)
	if root["system"] != "SYS" {
		t.Fatalf("top-level system = %v, want SYS: %s", root["system"], out)
	}
	messages := anthropicWireMessages(t, out)
	for _, message := range messages {
		if message["role"] == "system" {
			t.Fatalf("an empty assistant turn left a system role in messages: %s", out)
		}
	}
	if len(messages) != 2 || messages[0]["role"] != "user" || messages[1]["role"] != "user" {
		t.Fatalf("messages = %v, want the two user turns: %s", messages, out)
	}
	if !strings.Contains(string(out), "u1") || !strings.Contains(string(out), "u2") {
		t.Fatalf("system lift lost a user turn: %s", out)
	}
	if got := warningCount(warnings, ir.WarningApproximated, "system"); got != 1 {
		t.Fatalf("system placement warnings = %d, want 1 (%+v)", got, warnings)
	}
}

func TestAnthropicSystemAfterToolResultStays(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"result"},` +
		`{"role":"system","content":"SYS"},` +
		`{"role":"assistant","content":"answer"}]}`
	out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeJSONMap(t, out)["system"]; ok {
		t.Fatalf("valid system after tool_result was lifted: %s", out)
	}
	messages := anthropicWireMessages(t, out)
	if len(messages) != 5 || messages[0]["role"] != "user" || messages[1]["role"] != "assistant" ||
		messages[2]["role"] != "user" || messages[3]["role"] != "system" || messages[4]["role"] != "assistant" {
		t.Fatalf("messages = %v, want user/assistant/tool_result/system/assistant: %s", messages, out)
	}
	if !strings.Contains(string(out), `"tool_use_id":"call_1"`) {
		t.Fatalf("tool result lost its call id: %s", out)
	}
	if messages[2]["role"] != "user" {
		t.Fatalf("tool result is not immediately after its tool_use: %s", out)
	}
	if warningField(warnings, "system") {
		t.Fatalf("valid tool_result placement was reported as moved: %+v", warnings)
	}
}

// TestAnthropicSystemBeforeEncryptedAssistantStays is the emitted-turn control
// for an assistant whose only output is opaque reasoning: it still emits a
// reasoning block, so the system run before it remains valid.
func TestAnthropicSystemBeforeEncryptedAssistantStays(t *testing.T) {
	instructions := ""
	req := &ir.Request{
		Model: "m", MaxOutputTokens: intPtr(16), Instructions: &instructions,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("u1")}},
			{Role: ir.RoleSystem, Content: []ir.Block{ir.TextBlock("SYS")}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{
				Type: ir.BlockRedactedReasoning,
				Reasoning: &ir.Reasoning{
					Encrypted: "opaque",
					Provider:  ir.Anthropic,
				},
			}}},
		},
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeJSONMap(t, out)["system"]; ok {
		t.Fatalf("system before an emitted assistant was lifted: %s", out)
	}
	messages := anthropicWireMessages(t, out)
	if len(messages) != 3 || messages[1]["role"] != "system" || messages[2]["role"] != "assistant" {
		t.Fatalf("messages = %v, want user/system/assistant: %s", messages, out)
	}
}
