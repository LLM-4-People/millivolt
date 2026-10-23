package format

// Batch 9 regressions: a client's tool restriction is never broadened, and a
// malformed tool schema or name fails closed before any upstream send.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatAllowedToolsRejected(t *testing.T) {
	for _, choice := range []string{
		`{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"function","function":{"name":"f"}}]}}`,
		`{"type":"bogus"}`,
		`{}`,
	} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":` + choice + `}`
		out, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 0, "")
		if err == nil {
			t.Fatalf("tool_choice %s was translated: %s", choice, out)
		}
		if !strings.Contains(err.Error(), "tool_choice") && !strings.Contains(err.Error(), "broadening") {
			t.Fatalf("tool_choice %s error = %v, want a tool-restriction rejection", choice, err)
		}
	}
}

func TestToolChoiceNameMustBeString(t *testing.T) {
	for _, name := range []string{`null`, `5`, `""`, `{}`} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"tool_choice":{"type":"function","function":{"name":` + name + `}}}`
		if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 0, ""); err == nil {
			t.Fatalf("tool_choice.function.name %s was accepted", name)
		}
	}
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tool_choice":{"type":"function","function":{"name":"lookup"}}}`
	if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 0, ""); err != nil {
		t.Fatalf("valid named tool_choice rejected: %v", err)
	}
}

func TestToolParametersMustBeObject(t *testing.T) {
	for _, params := range []string{`[]`, `"text"`, `5`, `null`} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","function":{"name":"f","parameters":` + params + `}}]}`
		_, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 0, "")
		if params == `null` {
			if err != nil {
				t.Fatalf("null parameters rejected: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "parameters") {
			t.Fatalf("parameters %s error = %v, want an object requirement", params, err)
		}
	}
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`
	if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 0, ""); err != nil {
		t.Fatalf("object parameters rejected: %v", err)
	}
}
