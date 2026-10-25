package format

// A6 regression: Anthropic enabled thinking requires a budget of at least 1024
// and strictly below max_tokens.

import (
	"fmt"
	"strings"
	"testing"
)

func TestAnthropicThinkingBudgetRequired(t *testing.T) {
	body := func(maxTokens int, thinking string) string {
		return `{"model":"m","max_tokens":` + fmt.Sprintf("%d", maxTokens) + `,"messages":[{"role":"user","content":"hi"}],"thinking":` + thinking + `}`
	}
	invalid := []struct {
		name     string
		max      int
		thinking string
		want     string
	}{
		{"missing budget", 2048, `{"type":"enabled"}`, "budget_tokens is required"},
		{"null budget", 2048, `{"type":"enabled","budget_tokens":null}`, "budget_tokens is required"},
		{"below the floor", 2048, `{"type":"enabled","budget_tokens":100}`, "at least 1024"},
		{"equal to max_tokens", 1024, `{"type":"enabled","budget_tokens":1024}`, "less than max_tokens"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeAnthropicRequest([]byte(body(tc.max, tc.thinking))); err == nil {
				t.Fatal("invalid thinking budget was accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		thinking string
	}{
		{"floor below a larger max", `{"type":"enabled","budget_tokens":1024}`},
		{"integral spelling", `{"type":"enabled","budget_tokens":1024.0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := DecodeAnthropicRequest([]byte(body(2048, tc.thinking)))
			if err != nil {
				t.Fatalf("valid thinking budget rejected: %v", err)
			}
			if req.Reasoning == nil || req.Reasoning.BudgetTokens == nil || *req.Reasoning.BudgetTokens != 1024 {
				t.Fatalf("reasoning = %+v, want budget 1024", req.Reasoning)
			}
		})
	}
}
