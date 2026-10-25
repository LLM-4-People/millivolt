package format

// Round 5 H5 regression: the Anthropic allowed_callers fail-closed path has the
// same coverage as the Responses decoder. A programmatic-only, empty, scalar or
// malformed list must be rejected; a list containing direct is accepted with
// the provider-only knob reported as dropped.

import (
	"strings"
	"testing"
)

func anthropicToolAllowedCallers(raw string) (warningsText string, err error) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"lookup","input_schema":{"type":"object"},"allowed_callers":` + raw + `}]}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		return "", err
	}
	return req.Warnings.Header(), nil
}

func TestAnthropicAllowedCallersFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`["programmatic"]`,
		`[]`,
		`"direct"`,
		`[1]`,
		`[null]`,
	} {
		if _, err := anthropicToolAllowedCallers(raw); err == nil || !strings.Contains(err.Error(), "allowed_callers") {
			t.Fatalf("allowed_callers %s error = %v, want a rejection naming the field", raw, err)
		}
	}
}

func TestAnthropicAllowedCallersDirectIsAcceptedAndReported(t *testing.T) {
	for _, raw := range []string{`["direct"]`, `["direct","programmatic"]`} {
		warnings, err := anthropicToolAllowedCallers(raw)
		if err != nil {
			t.Fatalf("allowed_callers %s: %v", raw, err)
		}
		if !strings.Contains(warnings, "tool.allowed_callers") {
			t.Fatalf("allowed_callers %s was not reported as dropped: %q", raw, warnings)
		}
	}
}
