package format

// Round 7 verifier NEW-4 and NEW-6 regressions: a Chat assistant tool call that
// omits its id gets a synthesized id, and a tool reply may identify its call by
// id or by function name. The Chat decoder is the pairing owner, so it keeps the
// reference kind, tracks every call (explicit and generated), and reconciles the
// reply before any target encoder runs. Keeping a name made Anthropic and
// Responses emit a dangling reference, while treating every reference as a
// generated name let one call's function name steal another call's explicit id.

import (
	"slices"
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

func chatToolCall(id, name string) string {
	if id == "" {
		return `{"type":"function","function":{"name":"` + name + `","arguments":"{}"}}`
	}
	return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":"{}"}}`
}

func chatAssistantCalls(calls ...string) string {
	return `{"role":"assistant","content":"","tool_calls":[` + strings.Join(calls, ",") + `]}`
}

func chatToolResult(id, content string) string {
	return `{"role":"tool","tool_call_id":"` + id + `","content":"` + content + `"}`
}

func anthropicCallIDs(t *testing.T, out []byte) (uses, results []string) {
	t.Helper()
	for _, message := range anthropicWireMessages(t, out) {
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "tool_use":
				id, _ := block["id"].(string)
				uses = append(uses, id)
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				results = append(results, id)
			}
		}
	}
	return uses, results
}

func anthropicToolUseID(t *testing.T, out []byte) string {
	t.Helper()
	uses, _ := anthropicCallIDs(t, out)
	if len(uses) == 0 {
		return ""
	}
	return uses[0]
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
	uses, results := anthropicCallIDs(t, out)
	if want := []string{"call_1_0", "call_3_0"}; !slices.Equal(uses, want) {
		t.Fatalf("tool_use ids = %v, want %v: %s", uses, want, out)
	}
	if want := []string{"call_1_0", "call_3_0"}; !slices.Equal(results, want) {
		t.Fatalf("tool_result ids = %v, want %v: %s", results, want, out)
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
			!strings.Contains(err.Error(), "does not identify a unique generated tool call") {
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
				!strings.Contains(err.Error(), "matches more than one unresolved tool call") {
				t.Fatalf("error = %v, want the ambiguous-name rejection", err)
			}
		})
	}
}

// TestChatExplicitIDWinsOverGeneratedName is the NEW-6 regression: an explicit
// call id must answer its own result even when another call's function name is
// the same string, in either result order and with either call first. The
// generated call keeps its synthesized id on the other result.
func TestChatExplicitIDWinsOverGeneratedName(t *testing.T) {
	generatedFirst := chatAssistantCalls(chatToolCall("", "foo"), chatToolCall("foo", "bar"))
	explicitFirst := chatAssistantCalls(chatToolCall("foo", "bar"), chatToolCall("", "foo"))
	for _, tc := range []struct {
		name        string
		calls       string
		results     []string
		wantCalls   []string
		wantResults []string
	}{
		{
			name:  "generated first, explicit result first",
			calls: generatedFirst,
			results: []string{
				chatToolResult("foo", "explicit result"),
				chatToolResult("call_1_0", "generated result"),
			},
			wantCalls:   []string{"call_1_0", "foo"},
			wantResults: []string{"foo", "call_1_0"},
		},
		{
			name:  "generated first, generated result first",
			calls: generatedFirst,
			results: []string{
				chatToolResult("call_1_0", "generated result"),
				chatToolResult("foo", "explicit result"),
			},
			wantCalls:   []string{"call_1_0", "foo"},
			wantResults: []string{"call_1_0", "foo"},
		},
		{
			name:  "explicit first, explicit result first",
			calls: explicitFirst,
			results: []string{
				chatToolResult("foo", "explicit result"),
				chatToolResult("call_1_1", "generated result"),
			},
			wantCalls:   []string{"foo", "call_1_1"},
			wantResults: []string{"foo", "call_1_1"},
		},
		{
			name:        "only the explicit id is answered",
			calls:       generatedFirst,
			results:     []string{chatToolResult("foo", "explicit result")},
			wantCalls:   []string{"call_1_0", "foo"},
			wantResults: []string{"foo"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := append([]string{chatUserGo, tc.calls}, tc.results...)
			body := chatGeneratedIDBody(append(messages, chatUserNext)...)
			for _, target := range []ir.Protocol{ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					out, _, err := TranslateRequestBetween(ir.Chat, target, body, 16, "")
					if err != nil {
						t.Fatal(err)
					}
					var calls, results []string
					if target == ir.Anthropic {
						calls, results = anthropicCallIDs(t, out)
					} else {
						ids := responsesCallIDs(t, out)
						calls, results = ids["function_call"], ids["function_call_output"]
					}
					if !slices.Equal(calls, tc.wantCalls) {
						t.Fatalf("call ids = %v, want %v: %s", calls, tc.wantCalls, out)
					}
					if !slices.Equal(results, tc.wantResults) {
						t.Fatalf("result ids = %v, want %v: %s", results, tc.wantResults, out)
					}
				})
			}
		})
	}

	t.Run("answered explicit id is not stolen by the generated name", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			generatedFirst,
			chatToolResult("foo", "first explicit result"),
			chatToolResult("foo", "duplicate explicit result"),
			chatUserNext,
		)
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		uses, results := anthropicCallIDs(t, out)
		if !slices.Equal(uses, []string{"call_1_0", "foo"}) {
			t.Fatalf("call ids = %v, want [call_1_0 foo]: %s", uses, out)
		}
		if !slices.Equal(results, []string{"foo", "foo"}) {
			t.Fatalf("result ids = %v, want [foo foo]: %s", results, out)
		}
	})
}

// TestChatEmptyReferenceAmbiguity pins the empty-reference rule: it may answer
// only the one remaining generated call, and any other unresolved call makes it
// ambiguous even when that other call is explicit.
func TestChatEmptyReferenceAmbiguity(t *testing.T) {
	const emptyResult = `{"role":"tool","content":"result"}`
	t.Run("one generated and one explicit fail closed", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			chatAssistantCalls(chatToolCall("c1", "lookup"), chatToolCall("", "other")),
			emptyResult,
		)
		if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, ""); err == nil ||
			!strings.Contains(err.Error(), "does not identify a unique generated tool call") {
			t.Fatalf("error = %v, want the ambiguous empty-reference rejection", err)
		}
	})

	t.Run("explicit only fails closed", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			chatAssistantCalls(chatToolCall("c1", "lookup")),
			emptyResult,
		)
		if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, ""); err == nil ||
			!strings.Contains(err.Error(), "does not identify a unique generated tool call") {
			t.Fatalf("error = %v, want the explicit-only empty-reference rejection", err)
		}
	})

	t.Run("answered explicit leaves the generated call unique", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			chatAssistantCalls(chatToolCall("c1", "lookup"), chatToolCall("", "other")),
			chatToolResult("c1", "explicit result"),
			emptyResult,
			chatUserNext,
		)
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, results := anthropicCallIDs(t, out); !slices.Equal(results, []string{"c1", "call_1_1"}) {
			t.Fatalf("result ids = %v, want [c1 call_1_1]: %s", results, out)
		}
	})
}

// TestChatNameReferencePairing pins the message-name rule: a name can answer a
// generated call, it fails closed when unresolved calls share the name, and an
// explicit call is answered only through its id.
func TestChatNameReferencePairing(t *testing.T) {
	const nameResult = `{"role":"tool","name":"lookup","content":"result"}`
	for _, tc := range []struct {
		name  string
		calls string
	}{
		{
			name:  "generated and explicit share the name",
			calls: chatAssistantCalls(chatToolCall("c1", "lookup"), chatToolCall("", "lookup")),
		},
		{
			name: "two generated share the name",
			calls: chatAssistantCalls(
				chatToolCall("", "lookup"),
				chatToolCall("", "lookup"),
			),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := chatGeneratedIDBody(chatUserGo, tc.calls, nameResult)
			if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, ""); err == nil ||
				!strings.Contains(err.Error(), "matches more than one unresolved tool call") {
				t.Fatalf("error = %v, want the ambiguous-name rejection", err)
			}
		})
	}

	t.Run("explicit only stays unchanged", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			chatAssistantCalls(chatToolCall("c1", "lookup")),
			nameResult,
			chatUserNext,
		)
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		uses, results := anthropicCallIDs(t, out)
		if !slices.Equal(uses, []string{"c1"}) || !slices.Equal(results, []string{"lookup"}) {
			t.Fatalf("call ids = %v results = %v, want [c1] and [lookup]: %s", uses, results, out)
		}
	})

	t.Run("explicit tool_call_id name stays unchanged", func(t *testing.T) {
		body := chatGeneratedIDBody(
			chatUserGo,
			chatAssistantCalls(chatToolCall("c1", "lookup")),
			chatToolResult("lookup", "result"),
			chatUserNext,
		)
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, body, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		uses, results := anthropicCallIDs(t, out)
		if !slices.Equal(uses, []string{"c1"}) || !slices.Equal(results, []string{"lookup"}) {
			t.Fatalf("call ids = %v results = %v, want [c1] and [lookup]: %s", uses, results, out)
		}
	})
}
