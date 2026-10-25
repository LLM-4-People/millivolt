package format

// Round 7 A2 regression: Anthropic requires the first message to use the user
// role. A source history that begins with an assistant turn, or whose leading
// user turn encodes to nothing, must drop the leading assistant turns with one
// warning instead of emitting an invalid conversation.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicTargetDropsLeadingAssistantTurn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		messages   string
		wantRoles  []string
		wantWarned bool
	}{
		{
			name:       "assistant first",
			messages:   `[{"role":"assistant","content":"hi"},{"role":"user","content":"hello"}]`,
			wantRoles:  []string{"user"},
			wantWarned: true,
		},
		{
			name:       "empty leading user",
			messages:   `[{"role":"user","content":""},{"role":"assistant","content":"hi"},{"role":"user","content":"hello"}]`,
			wantRoles:  []string{"user"},
			wantWarned: true,
		},
		{
			name:       "multiple leading assistants",
			messages:   `[{"role":"assistant","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"hello"}]`,
			wantRoles:  []string{"user"},
			wantWarned: true,
		},
		{
			name:      "user first unchanged",
			messages:  `[{"role":"user","content":"hello"},{"role":"assistant","content":"ok"}]`,
			wantRoles: []string{"user", "assistant"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":16,"messages":` + tc.messages + `}`
			out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Messages []struct {
					Role string `json:"role"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			roles := make([]string, 0, len(doc.Messages))
			for _, m := range doc.Messages {
				roles = append(roles, m.Role)
			}
			if strings.Join(roles, ",") != strings.Join(tc.wantRoles, ",") {
				t.Fatalf("roles = %v, want %v (body %s)", roles, tc.wantRoles, out)
			}
			if got := warningCount(warnings, ir.WarningDropped, "messages.leading_assistant"); (got == 1) != tc.wantWarned {
				t.Fatalf("leading-assistant warnings = %d, want warned=%v (%+v)", got, tc.wantWarned, warnings)
			}
		})
	}

	// An assistant-only history has no representable Anthropic turn; the
	// existing empty-result error stays.
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"assistant","content":"only"}]}`
	if _, _, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, ""); err == nil ||
		!strings.Contains(err.Error(), "requires at least one non-empty message") {
		t.Fatalf("assistant-only error = %v, want the empty-result rejection", err)
	}
}
