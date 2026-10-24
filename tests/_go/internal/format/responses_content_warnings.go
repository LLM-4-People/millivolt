package format

// Round 7 A3 regression: a user or system turn can carry IR blocks the
// Responses content-part union cannot represent. The encoder used to drop a
// tool call or reasoning block silently; it must report the loss with a
// role-aware content.<type> warning. The assistant branch already handles its
// representable blocks explicitly and must not gain a redundant warning.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTargetWarnsOnUserToolCall(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hi"},` +
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}},` +
		`{"type":"thinking","thinking":"why","signature":"sig"}]}]}`
	out, warnings, err := TranslateRequestBetween(ir.Anthropic, ir.Responses, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := warningCount(warnings, ir.WarningDropped, "content.tool_call"); got != 1 {
		t.Fatalf("content.tool_call warnings = %d, want 1 (%+v)", got, warnings)
	}
	if got := warningCount(warnings, ir.WarningDropped, "content.reasoning"); got != 1 {
		t.Fatalf("content.reasoning warnings = %d, want 1 (%+v)", got, warnings)
	}
	if strings.Contains(string(out), "toolu_1") || strings.Contains(string(out), `"why"`) {
		t.Fatalf("unrepresentable user blocks leaked into the Responses body: %s", out)
	}
	if !strings.Contains(string(out), "hi") {
		t.Fatalf("representable user text was dropped: %s", out)
	}

	assistant := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"why","signature":"sig"},` +
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}},` +
		`{"type":"text","text":"calling"}]}]}`
	out, warnings, err = TranslateRequestBetween(ir.Anthropic, ir.Responses, []byte(assistant), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if warningField(warnings, "content.tool_call") || warningField(warnings, "content.reasoning") {
		t.Fatalf("assistant tool/reasoning blocks gained a content drop warning: %+v", warnings)
	}
	if !strings.Contains(string(out), "toolu_1") || !strings.Contains(string(out), "why") {
		t.Fatalf("assistant tool/reasoning blocks were not encoded: %s", out)
	}
}
