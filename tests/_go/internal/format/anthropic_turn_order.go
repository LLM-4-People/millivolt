package format

// Round 13 A-U regression: merging two same-role Anthropic turns must keep
// tool_result blocks first in the assembled user turn. A Chat sequence of
// assistant tool call, user note, tool result otherwise reaches Anthropic with
// the note before its matching tool_result.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicLastUserContent(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("translated request does not decode: %v\n%s", err, out)
	}
	for i := len(doc.Messages) - 1; i >= 0; i-- {
		if doc.Messages[i].Role != "user" {
			continue
		}
		var blocks []map[string]any
		if err := json.Unmarshal(doc.Messages[i].Content, &blocks); err != nil {
			t.Fatalf("last user content is not a block array: %v (%s)", err, doc.Messages[i].Content)
		}
		return blocks
	}
	t.Fatalf("no user message in translated request: %s", out)
	return nil
}

func TestMergedAnthropicUserTurnKeepsToolResultsFirst(t *testing.T) {
	const assistantCall = `{"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
	t.Run("text then tool result", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"},` + assistantCall + `,` +
			`{"role":"user","content":"note"},` +
			`{"role":"tool","tool_call_id":"call_1","content":"result"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		blocks := anthropicLastUserContent(t, out)
		if len(blocks) != 2 || blocks[0]["type"] != "tool_result" || blocks[1]["type"] != "text" {
			t.Fatalf("merged user blocks = %v, want tool_result then text: %s", blocks, out)
		}
		if got := warningCount(warnings, ir.WarningApproximated, "messages.tool_result"); got != 1 {
			t.Fatalf("approximated messages.tool_result warnings = %d, want 1: %+v", got, warnings)
		}
	})

	t.Run("already results first", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"},` + assistantCall + `,` +
			`{"role":"tool","tool_call_id":"call_1","content":"result"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		blocks := anthropicLastUserContent(t, out)
		if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
			t.Fatalf("control user blocks = %v, want one tool_result: %s", blocks, out)
		}
		if got := warningCount(warnings, ir.WarningApproximated, "messages.tool_result"); got != 0 {
			t.Fatalf("already-ordered turn warned: %+v", warnings)
		}
	})

	t.Run("text-only merge", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		blocks := anthropicLastUserContent(t, out)
		if len(blocks) != 2 || blocks[0]["text"] != "a" || blocks[1]["text"] != "b" {
			t.Fatalf("text merge order = %v, want a then b: %s", blocks, out)
		}
		if got := warningCount(warnings, ir.WarningApproximated, "messages.tool_result"); got != 0 {
			t.Fatalf("text-only merge warned: %+v", warnings)
		}
	})
}
