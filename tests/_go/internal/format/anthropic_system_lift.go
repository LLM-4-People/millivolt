package format

// Round 8 A1/A-U regression: the top-level system field must be derived from
// the emitted conversation turns, not from the raw input roles. A leading
// assistant turn or an empty leading user turn can leave the conversation with
// no emitted turn, and the system turn that follows then belongs at the top
// level. Treating the dropped turn as the conversation start either lost the
// system text or emitted a system message first, which Anthropic rejects.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicSystemAfterDroppedLeadingTurnIsLifted(t *testing.T) {
	t.Run("system after a dropped leading assistant", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"assistant","content":"hi"},` +
			`{"role":"system","content":"SYS"},` +
			`{"role":"user","content":"hello"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		root := decodeJSONMap(t, out)
		if root["system"] != "SYS" {
			t.Fatalf("top-level system = %v, want SYS: %s", root["system"], out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 1 || messages[0]["role"] != "user" || messages[0]["content"] != "hello" {
			t.Fatalf("messages = %v, want only the user turn: %s", messages, out)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.leading_assistant"); got != 1 {
			t.Fatalf("leading-assistant warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("system after an empty leading user turn", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":""},` +
			`{"role":"system","content":"SYS"},` +
			`{"role":"user","content":"hello"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		root := decodeJSONMap(t, out)
		if root["system"] != "SYS" {
			t.Fatalf("top-level system = %v, want SYS: %s", root["system"], out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 1 || messages[0]["role"] != "user" || messages[0]["content"] != "hello" {
			t.Fatalf("messages = %v, want the first emitted role user: %s", messages, out)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.leading_assistant"); got != 0 {
			t.Fatalf("empty user turn warned as a leading assistant: %+v", warnings)
		}
	})

	t.Run("system after a dropped assistant and its orphan result", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"result"},` +
			`{"role":"system","content":"SYS"},` +
			`{"role":"user","content":"next"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		root := decodeJSONMap(t, out)
		if root["system"] != "SYS" {
			t.Fatalf("top-level system = %v, want SYS: %s", root["system"], out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 1 || messages[0]["role"] != "user" || messages[0]["content"] != "next" {
			t.Fatalf("messages = %v, want only the next user turn: %s", messages, out)
		}
		if strings.Contains(string(out), `"tool_use_id"`) {
			t.Fatalf("orphan tool result reached the wire: %s", out)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.leading_assistant"); got != 1 {
			t.Fatalf("leading-assistant warnings = %d, want 1 (%+v)", got, warnings)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.orphan_tool_result"); got != 1 {
			t.Fatalf("orphan warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("system after an emitted user turn before an assistant stays in messages", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":"u1"},` +
			`{"role":"system","content":"mid"},` +
			`{"role":"assistant","content":"a1"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := decodeJSONMap(t, out)["system"]; ok {
			t.Fatalf("valid mid-conversation system was lifted: %s", out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 3 || messages[0]["role"] != "user" || messages[1]["role"] != "system" ||
			messages[1]["content"] != "mid" || messages[2]["role"] != "assistant" {
			t.Fatalf("messages = %v, want user/system/assistant in place: %s", messages, out)
		}
		if warningField(warnings, "system") {
			t.Fatalf("in-place system turn was reported as moved: %+v", warnings)
		}
	})

	t.Run("system between user turns is lifted", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":"u1"},` +
			`{"role":"system","content":"mid"},` +
			`{"role":"user","content":"u2"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeJSONMap(t, out)["system"]; got != "mid" {
			t.Fatalf("top-level system = %v, want mid: %s", got, out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 1 || messages[0]["role"] != "user" {
			t.Fatalf("messages = %v, want the two user turns merged: %s", messages, out)
		}
		if !strings.Contains(string(out), "u1") || !strings.Contains(string(out), "u2") {
			t.Fatalf("merged user turns lost text: %s", out)
		}
		if got := warningCount(warnings, ir.WarningApproximated, "system"); got != 1 {
			t.Fatalf("system placement warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("system between a tool call and its result is lifted", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]},` +
			`{"role":"system","content":"SYS"},` +
			`{"role":"tool","tool_call_id":"call_1","content":"result"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeJSONMap(t, out)["system"]; got != "SYS" {
			t.Fatalf("top-level system = %v, want SYS: %s", got, out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 3 || messages[0]["role"] != "user" || messages[1]["role"] != "assistant" ||
			messages[2]["role"] != "user" {
			t.Fatalf("messages = %v, want user/assistant/tool-result: %s", messages, out)
		}
		if !strings.Contains(string(out), `"tool_use_id":"call_1"`) {
			t.Fatalf("tool result lost its call id: %s", out)
		}
		if got := warningCount(warnings, ir.WarningApproximated, "system"); got != 1 {
			t.Fatalf("system placement warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("consecutive system turns are one in-place run", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":"u1"},` +
			`{"role":"system","content":"S1"},` +
			`{"role":"system","content":"S2"},` +
			`{"role":"assistant","content":"a1"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := decodeJSONMap(t, out)["system"]; ok {
			t.Fatalf("valid system run was lifted: %s", out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 3 || messages[1]["role"] != "system" || messages[1]["content"] != "S1\n\nS2" {
			t.Fatalf("messages = %v, want one merged system run: %s", messages, out)
		}
		if warningField(warnings, "system") {
			t.Fatalf("in-place system run was reported as moved: %+v", warnings)
		}
	})

	t.Run("system at the end after a user turn stays", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":"u1"},` +
			`{"role":"system","content":"last"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := decodeJSONMap(t, out)["system"]; ok {
			t.Fatalf("trailing valid system was lifted: %s", out)
		}
		messages := anthropicWireMessages(t, out)
		if len(messages) != 2 || messages[1]["role"] != "system" || messages[1]["content"] != "last" {
			t.Fatalf("messages = %v, want user/system: %s", messages, out)
		}
		if warningField(warnings, "system") {
			t.Fatalf("trailing system was reported as moved: %+v", warnings)
		}
	})

	t.Run("instructions precede the lifted system turn", func(t *testing.T) {
		instructions := "INST"
		req := &ir.Request{
			Model: "m", MaxOutputTokens: intPtr(16), Instructions: &instructions,
			Messages: []ir.Message{
				{Role: ir.RoleAssistant, Content: []ir.Block{ir.TextBlock("hi")}},
				{Role: ir.RoleSystem, Content: []ir.Block{ir.TextBlock("SYS")}},
				{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hello")}},
			},
		}
		out, err := EncodeAnthropicRequest(req, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeJSONMap(t, out)["system"]; got != "INST\n\nSYS" {
			t.Fatalf("top-level system = %v, want instructions followed by the lifted turn: %s", got, out)
		}
	})

	t.Run("a system-only history still has no conversation turn", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[{"role":"system","content":"SYS"}]}`
		if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, ""); err == nil ||
			!strings.Contains(err.Error(), "requires at least one non-empty message") {
			t.Fatalf("system-only error = %v, want the empty-result rejection", err)
		}
	})
}
