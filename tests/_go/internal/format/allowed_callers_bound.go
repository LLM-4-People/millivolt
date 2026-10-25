package format

// A2 regression: one tool's allowed_callers array is bounded lexically before
// the typed []string decode, for both the Responses and Anthropic decoders.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func allowedCallersArray(n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		if i == 0 {
			b.WriteString(`"direct"`)
		} else {
			b.WriteString(`""`)
		}
	}
	b.WriteString("]")
	return b.String()
}

func TestToolAllowedCallersBound(t *testing.T) {
	cases := []struct {
		name string
		wire string
	}{
		{
			name: "responses",
			wire: `{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{},"allowed_callers":%s}]}`,
		},
		{
			name: "anthropic",
			wire: `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{},"allowed_callers":%s}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(body string) (*ir.Request, error) {
				if tc.name == "responses" {
					return DecodeResponsesRequest([]byte(body))
				}
				return DecodeAnthropicRequest([]byte(body))
			}
			atLimit := strings.Replace(tc.wire, "%s", allowedCallersArray(10_000), 1)
			req, err := decode(atLimit)
			if err != nil {
				t.Fatalf("10,000-caller array was rejected: %v", err)
			}
			if !warningField(req.Warnings, "tool.allowed_callers") {
				t.Fatalf("10,000-caller array recorded no loss warning: %+v", req.Warnings)
			}

			over := strings.Replace(tc.wire, "%s", allowedCallersArray(10_001), 1)
			if _, err := decode(over); err == nil {
				t.Fatal("10,001-caller array was accepted")
			} else if !strings.Contains(err.Error(), "allowed_callers exceeds 10000 items") {
				t.Fatalf("error = %v, want the allowed_callers array bound", err)
			}
		})
	}
}
