package format

// Round 7 verifier D1: the request decoders must accept every JSON spelling of
// an integral token cap, not only a plain integer literal. encoding/json
// rejects 1e6 and 1000000.0 for an int field even though the value is integral,
// so the shared jsonnum owner now decides the value and each decoder writes the
// canonical integer into the IR.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func tokenCapBody(source ir.Protocol, field, literal string) string {
	switch source {
	case ir.Chat:
		return `{"model":"m","messages":[{"role":"user","content":"hi"}],"` + field + `":` + literal + `}`
	case ir.Responses:
		return `{"model":"m","input":"hi","` + field + `":` + literal + `}`
	default:
		return `{"model":"m","` + field + `":` + literal + `,"messages":[{"role":"user","content":"hi"}]}`
	}
}

func decodeTokenCap(t *testing.T, source ir.Protocol, body string) (*ir.Request, error) {
	t.Helper()
	switch source {
	case ir.Chat:
		return DecodeChatRequest([]byte(body))
	case ir.Responses:
		return DecodeResponsesRequest([]byte(body))
	default:
		return DecodeAnthropicRequest([]byte(body))
	}
}

func TestTokenCapIntegralSpellingsDecode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source ir.Protocol
		field  string
	}{
		{name: "chat max_tokens", source: ir.Chat, field: "max_tokens"},
		{name: "chat max_completion_tokens", source: ir.Chat, field: "max_completion_tokens"},
		{name: "responses max_output_tokens", source: ir.Responses, field: "max_output_tokens"},
		{name: "anthropic max_tokens", source: ir.Anthropic, field: "max_tokens"},
	} {
		for _, literal := range []string{"1000000", "1e6", "1000000.0", "1E+6", "1000000.000"} {
			t.Run(tc.name+"/"+literal, func(t *testing.T) {
				req, err := decodeTokenCap(t, tc.source, tokenCapBody(tc.source, tc.field, literal))
				if err != nil {
					t.Fatalf("integral spelling %s was rejected: %v", literal, err)
				}
				if req.MaxOutputTokens == nil || *req.MaxOutputTokens != 1000000 {
					t.Fatalf("decoded cap = %v, want 1000000", req.MaxOutputTokens)
				}
			})
		}
		for _, literal := range []string{"1.5", "0.5", `"1000000"`, "true", "99999999999999999999", "1e99"} {
			t.Run(tc.name+" rejects "+literal, func(t *testing.T) {
				req, err := decodeTokenCap(t, tc.source, tokenCapBody(tc.source, tc.field, literal))
				if err == nil {
					t.Fatalf("non-integral spelling %s was accepted with cap %v", literal, req.MaxOutputTokens)
				}
				if !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("error = %v, want the field name %s", err, tc.field)
				}
				if !strings.Contains(err.Error(), "integer") && !strings.Contains(err.Error(), "outside the supported integer range") {
					t.Fatalf("error = %v, want the integral-value reason", err)
				}
			})
		}
	}
}

// A malformed max_completion_tokens must not fall back to a valid max_tokens:
// the body carries a cap the wire schema rejects, so the request fails closed.
func TestTokenCapMalformedPreferredFieldDoesNotFallBack(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1.5,"max_tokens":100}`
	if _, err := DecodeChatRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "max_completion_tokens") {
		t.Fatalf("error = %v, want the malformed preferred field", err)
	}
}

func TestTokenCapIntegralSpellingsEncodeCanonicalInteger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source ir.Protocol
		target ir.Protocol
		field  string
	}{
		{name: "chat to anthropic", source: ir.Chat, target: ir.Anthropic, field: "max_tokens"},
		{name: "responses to anthropic", source: ir.Responses, target: ir.Anthropic, field: "max_output_tokens"},
		{name: "anthropic to chat", source: ir.Anthropic, target: ir.Chat, field: "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tokenCapBody(tc.source, tc.field, "1e6")
			out, _, err := TranslateRequestBetween(tc.source, tc.target, []byte(body), 16, "")
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			wantField := "max_tokens"
			if tc.target == ir.Responses {
				wantField = "max_output_tokens"
			}
			if got, ok := doc[wantField].(float64); !ok || got != 1000000 {
				t.Fatalf("translated %s = %v, want the canonical integer 1000000 (%s)", wantField, doc[wantField], out)
			}
		})
	}
}
