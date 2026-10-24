package format

// Round 7 verifier NEW-4 regression: a Chat assistant tool call that omits its
// id gets a synthesized id, but a tool reply may name the call by function name
// (tool_call_id or the message's name). The Chat decoder is the id owner, so it
// reconciles that reference with the synthesized id before any target encoder
// runs. Keeping the name made Anthropic and Responses emit a dangling
// tool_result/function_call_output, and it hid the leading-assistant orphan from
// the Anthropic filter because that filter tracks the generated id.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const (
	chatUserGo      = `{"role":"user","content":"go"}`
	chatUserNext    = `{"role":"user","content":"next"}`
	chatToolNamed   = `{"role":"tool","tool_call_id":"lookup","content":"result"}`
	chatToolByField = `{"role":"tool","name":"lookup","content":"result"}`
)

func chatGeneratedIDBody(messages ...string) []byte {
	return []byte(`{"model":"m","max_tokens":16,"messages":[` + strings.Join(messages, ",") + `]}`)
}

func chatMissingIDCall(name string) string {
	return `{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"` + name + `","arguments":"{\"q\":1}"}}]}`
}

func anthropicToolUseID(t *testing.T, out []byte) string {
	t.Helper()
	messages := anthropicWireMessages(t, out)
	for _, message := range messages {
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block["type"] == "tool_use" {
				id, _ := block["id"].(string)
				return id
			}
		}
	}
	return ""
}

func TestChatGeneratedToolIDReconcilesNamedResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		target   ir.Protocol
		messages []string
	}{
		{
			name:     "anthropic tool_call_id names the function",
			target:   ir.Anthropic,
			messages: []string{chatUserGo, chatMissingIDCall("lookup"), chatToolNamed, chatUserNext},
		},
		{
			name:     "anthropic message name identifies the call",
			target:   ir.Anthropic,
			messages: []string{chatUserGo, chatMissingIDCall("lookup"), chatToolByField, chatUserNext},
		},
		{
			name:     "responses tool_call_id names the function",
			target:   ir.Responses,
			messages: []string{chatUserGo, chatMissingIDCall("lookup"), chatToolNamed, chatUserNext},
		},
		{
			name:     "responses message name identifies the call",
			target:   ir.Responses,
			messages: []string{chatUserGo, chatMissingIDCall("lookup"), chatToolByField, chatUserNext},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, warnings, err := TranslateRequestBetween(ir.Chat, tc.target, chatGeneratedIDBody(tc.messages...), 16, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := warningCount(warnings, ir.WarningApproximated, "tool_calls.id"); got != 1 {
				t.Fatalf("generated-id warnings = %d, want 1 (%+v)", got, warnings)
			}
			if tc.target == ir.Anthropic {
				if got := anthropicToolUseID(t, out); got != "call_1_0" {
					t.Fatalf("tool_use id = %q, want the generated call_1_0: %s", got, out)
				}
				if !strings.Contains(string(out), `"tool_use_id":"call_1_0"`) {
					t.Fatalf("tool_result does not reference the generated id: %s", out)
				}
				if strings.Contains(string(out), `"tool_use_id":"lookup"`) {
					t.Fatalf("tool_result kept the dangling function name: %s", out)
				}
				return
			}
			ids := responsesCallIDs(t, out)
			if len(ids["function_call"]) != 1 || ids["function_call"][0] != "call_1_0" {
				t.Fatalf("function_call ids = %v, want [call_1_0]: %s", ids["function_call"], out)
			}
			if len(ids["function_call_output"]) != 1 || ids["function_call_output"][0] != "call_1_0" {
				t.Fatalf("function_call_output ids = %v, want [call_1_0]: %s", ids["function_call_output"], out)
			}
		})
	}
}

func responsesCallIDs(t *testing.T, out []byte) map[string][]string {
	t.Helper()
	ids := map[string][]string{}
	input, _ := decodeJSONMap(t, out)["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		typ, _ := item["type"].(string)
		if id, ok := item["call_id"].(string); ok {
			ids[typ] = append(ids[typ], id)
		}
	}
	return ids
}

func TestChatGeneratedToolIDReconcilesLeadingResult(t *testing.T) {
	messages := []string{chatMissingIDCall("lookup"), chatToolNamed, chatUserNext}

	t.Run("anthropic drops the reconciled orphan", func(t *testing.T) {
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, chatGeneratedIDBody(messages...), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"tool_use_id"`) {
			t.Fatalf("orphan tool_result reached the wire: %s", out)
		}
		wire := anthropicWireMessages(t, out)
		if len(wire) != 1 || wire[0]["role"] != "user" || wire[0]["content"] != "next" {
			t.Fatalf("messages = %v, want only the next user turn", wire)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.leading_assistant"); got != 1 {
			t.Fatalf("leading-assistant warnings = %d, want 1 (%+v)", got, warnings)
		}
		if got := warningCount(warnings, ir.WarningDropped, "messages.orphan_tool_result"); got != 1 {
			t.Fatalf("orphan tool_result warnings = %d, want 1 (%+v)", got, warnings)
		}
	})

	t.Run("responses keeps both sides on the generated id", func(t *testing.T) {
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Responses, chatGeneratedIDBody(messages...), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		// The leading assistant turn is message index 0, so its synthesized
		// id is call_0_0. Responses keeps both sides on that same id.
		ids := responsesCallIDs(t, out)
		if len(ids["function_call"]) != 1 || ids["function_call"][0] != "call_0_0" {
			t.Fatalf("function_call ids = %v, want [call_0_0]: %s", ids["function_call"], out)
		}
		if len(ids["function_call_output"]) != 1 || ids["function_call_output"][0] != "call_0_0" {
			t.Fatalf("function_call_output ids = %v, want [call_0_0]: %s", ids["function_call_output"], out)
		}
	})
}

func TestChatExplicitToolIDHistoryUnchanged(t *testing.T) {
	const explicitCall = `{"role":"assistant","content":"","tool_calls":[{"id":"call_explicit","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]}`
	const explicitResult = `{"role":"tool","tool_call_id":"call_explicit","content":"result"}`
	messages := []string{chatUserGo, explicitCall, explicitResult, chatUserNext}

	for _, target := range []ir.Protocol{ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			out, warnings, err := TranslateRequestBetween(ir.Chat, target, chatGeneratedIDBody(messages...), 16, "")
			if err != nil {
				t.Fatal(err)
			}
			if warningField(warnings, "tool_calls.id") {
				t.Fatalf("explicit id warned as approximated: %+v", warnings)
			}
			if target == ir.Anthropic {
				if got := anthropicToolUseID(t, out); got != "call_explicit" {
					t.Fatalf("tool_use id = %q, want call_explicit: %s", got, out)
				}
				if !strings.Contains(string(out), `"tool_use_id":"call_explicit"`) {
					t.Fatalf("tool_result id changed: %s", out)
				}
				return
			}
			ids := responsesCallIDs(t, out)
			if len(ids["function_call"]) != 1 || ids["function_call"][0] != "call_explicit" {
				t.Fatalf("function_call ids = %v, want [call_explicit]: %s", ids["function_call"], out)
			}
			if len(ids["function_call_output"]) != 1 || ids["function_call_output"][0] != "call_explicit" {
				t.Fatalf("function_call_output ids = %v, want [call_explicit]: %s", ids["function_call_output"], out)
			}
		})
	}

	t.Run("legacy function_call keeps name pairing", func(t *testing.T) {
		legacy := []string{
			chatUserGo,
			`{"role":"assistant","content":"","function_call":{"name":"lookup","arguments":"{\"q\":1}"}}`,
			`{"role":"function","name":"lookup","content":"result"}`,
			chatUserNext,
		}
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, chatGeneratedIDBody(legacy...), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if warningField(warnings, "tool_calls.id") {
			t.Fatalf("legacy function_call warned as approximated: %+v", warnings)
		}
		if !strings.Contains(string(out), `"id":"lookup"`) || !strings.Contains(string(out), `"tool_use_id":"lookup"`) {
			t.Fatalf("legacy name pairing changed: %s", out)
		}
	})
}

func TestChatGeneratedToolIDConsumesInOrder(t *testing.T) {
	body := chatGeneratedIDBody(
		chatUserGo,
		chatMissingIDCall("lookup"),
		chatToolNamed,
		chatMissingIDCall("lookup"),
		chatToolNamed,
		chatUserNext,
	)
	out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, message := range anthropicWireMessages(t, out) {
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "tool_use":
				ids = append(ids, block["id"].(string))
			case "tool_result":
				ids = append(ids, block["tool_use_id"].(string))
			}
		}
	}
	want := []string{"call_1_0", "call_1_0", "call_3_0", "call_3_0"}
	if len(ids) != len(want) {
		t.Fatalf("call ids = %v, want %v: %s", ids, want, out)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("call ids = %v, want %v: %s", ids, want, out)
		}
	}
}

func TestChatGeneratedToolIDEmptyReference(t *testing.T) {
	t.Run("single generated call resolves", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			chatMissingIDCall("lookup"),
			`{"role":"tool","content":"result"}`,
			chatUserNext,
		)
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"tool_use_id":"call_1_0"`) {
			t.Fatalf("empty reference did not resolve to the generated id: %s", out)
		}
	})

	t.Run("multiple generated calls fail closed", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			`{"role":"assistant","content":"","tool_calls":[`+
				`{"type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}},`+
				`{"type":"function","function":{"name":"other","arguments":"{}"}}]}`,
			`{"role":"tool","content":"result"}`,
		)
		if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, ""); err == nil ||
			!strings.Contains(err.Error(), "more than one generated tool call is unresolved") {
			t.Fatalf("error = %v, want the ambiguous empty-reference rejection", err)
		}
	})
}

func TestChatGeneratedToolIDAmbiguousNameFailsClosed(t *testing.T) {
	body := chatGeneratedIDBody(
		chatUserGo,
		`{"role":"assistant","content":"","tool_calls":[`+
			`{"type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}},`+
			`{"type":"function","function":{"name":"lookup","arguments":"{\"q\":2}"}}]}`,
		chatToolNamed,
	)
	for _, target := range []ir.Protocol{ir.Anthropic, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			if _, _, err := TranslateRequestBetween(ir.Chat, target, body, 16, ""); err == nil ||
				!strings.Contains(err.Error(), "matches more than one generated tool call") {
				t.Fatalf("error = %v, want the ambiguous-name rejection", err)
			}
		})
	}
}
