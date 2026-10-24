package format

// A5 regression: request integer fields accept every integral JSON spelling
// and reject fractional values at the decoder that owns each protocol.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestRequestIntegralNumbers(t *testing.T) {
	chat := func(fields string) string {
		return `{"model":"m","messages":[{"role":"user","content":"hi"}],` + fields + `}`
	}
	responses := func(fields string) string {
		return `{"model":"m","input":"hi",` + fields + `}`
	}
	anthropic := func(fields string) string {
		return `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],` + fields + `}`
	}
	cases := []struct {
		name   string
		body   string
		decode func([]byte) (*ir.Request, error)
		check  func(*testing.T, *ir.Request)
	}{
		{
			name:   "chat seed",
			body:   chat(`"seed":1.0`),
			decode: DecodeChatRequest,
			check: func(t *testing.T, req *ir.Request) {
				if req.Seed == nil || *req.Seed != 1 {
					t.Fatalf("seed = %v, want 1", req.Seed)
				}
			},
		},
		{
			name:   "chat exponent seed",
			body:   chat(`"seed":1e1`),
			decode: DecodeChatRequest,
			check: func(t *testing.T, req *ir.Request) {
				if req.Seed == nil || *req.Seed != 10 {
					t.Fatalf("seed = %v, want 10", req.Seed)
				}
			},
		},
		{
			name:   "chat n",
			body:   chat(`"n":1.0`),
			decode: DecodeChatRequest,
			check:  func(*testing.T, *ir.Request) {},
		},
		{
			name:   "chat top_logprobs",
			body:   chat(`"top_logprobs":1e1`),
			decode: DecodeChatRequest,
			check: func(t *testing.T, req *ir.Request) {
				if req.TopLogprobs == nil || *req.TopLogprobs != 10 {
					t.Fatalf("top_logprobs = %v, want 10", req.TopLogprobs)
				}
			},
		},
		{
			name:   "responses max_tool_calls",
			body:   responses(`"max_tool_calls":1.0`),
			decode: DecodeResponsesRequest,
			check: func(t *testing.T, req *ir.Request) {
				if !hasWarningField(req, "max_tool_calls") {
					t.Fatalf("max_tool_calls warning missing: %+v", req.Warnings)
				}
			},
		},
		{
			name:   "responses top_logprobs",
			body:   responses(`"top_logprobs":1e1`),
			decode: DecodeResponsesRequest,
			check: func(t *testing.T, req *ir.Request) {
				if req.TopLogprobs == nil || *req.TopLogprobs != 10 {
					t.Fatalf("top_logprobs = %v, want 10", req.TopLogprobs)
				}
			},
		},
		{
			name:   "anthropic top_k",
			body:   anthropic(`"top_k":5.0`),
			decode: DecodeAnthropicRequest,
			check: func(t *testing.T, req *ir.Request) {
				if req.TopK == nil || *req.TopK != 5 {
					t.Fatalf("top_k = %v, want 5", req.TopK)
				}
			},
		},
		{
			name:   "anthropic thinking budget",
			body:   anthropic(`"thinking":{"type":"enabled","budget_tokens":2048.0}`),
			decode: DecodeAnthropicRequest,
			check: func(t *testing.T, req *ir.Request) {
				if req.Reasoning == nil || req.Reasoning.BudgetTokens == nil || *req.Reasoning.BudgetTokens != 2048 {
					t.Fatalf("budget_tokens = %+v, want 2048", req.Reasoning)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.decode([]byte(tc.body))
			if err != nil {
				t.Fatalf("integral spelling rejected: %v", err)
			}
			tc.check(t, req)

			fractional := strings.Replace(tc.body, `1.0`, `1.5`, 1)
			fractional = strings.Replace(fractional, `1e1`, `1.5`, 1)
			fractional = strings.Replace(fractional, `5.0`, `5.5`, 1)
			fractional = strings.Replace(fractional, `2048.0`, `2048.5`, 1)
			if fractional == tc.body {
				t.Fatal("fractional fixture did not change the body")
			}
			if _, err := tc.decode([]byte(fractional)); err == nil {
				t.Fatal("fractional spelling was accepted")
			}
		})
	}
}
