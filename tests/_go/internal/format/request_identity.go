package format

// Round 2 TC1-4a regressions: verbosity, safety_identifier and
// prompt_cache_key cross Chat and Responses unchanged, and only an Anthropic
// target reports them as dropped.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestRequestIdentityFieldsCrossWire(t *testing.T) {
	chatBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"verbosity":"high","safety_identifier":"s1","prompt_cache_key":"k1"}`

	t.Run("chat to responses", func(t *testing.T) {
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Responses, []byte(chatBody), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		root := decodeJSONMap(t, out)
		if root["safety_identifier"] != "s1" || root["prompt_cache_key"] != "k1" {
			t.Fatalf("identity fields = %v/%v, want s1/k1: %s",
				root["safety_identifier"], root["prompt_cache_key"], out)
		}
		text, _ := root["text"].(map[string]any)
		if text["verbosity"] != "high" {
			t.Fatalf("text.verbosity = %v, want high: %s", text["verbosity"], out)
		}
		for _, warning := range warnings {
			switch warning.Field {
			case "verbosity", "text.verbosity", "safety_identifier", "prompt_cache_key":
				t.Fatalf("unexpected identity warning: %+v", warning)
			}
		}
	})

	t.Run("responses to chat", func(t *testing.T) {
		body := `{"model":"m","input":"hi","text":{"verbosity":"low"},` +
			`"safety_identifier":"s2","prompt_cache_key":"k2"}`
		out, warnings, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		root := decodeJSONMap(t, out)
		if root["verbosity"] != "low" || root["safety_identifier"] != "s2" || root["prompt_cache_key"] != "k2" {
			t.Fatalf("identity fields = %v/%v/%v, want low/s2/k2: %s",
				root["verbosity"], root["safety_identifier"], root["prompt_cache_key"], out)
		}
		for _, warning := range warnings {
			switch warning.Field {
			case "verbosity", "text.verbosity", "safety_identifier", "prompt_cache_key":
				t.Fatalf("unexpected identity warning: %+v", warning)
			}
		}
	})

	t.Run("anthropic warns", func(t *testing.T) {
		out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(chatBody), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, warning := range warnings {
			found[warning.Field] = true
		}
		for _, field := range []string{"verbosity", "safety_identifier", "prompt_cache_key"} {
			if !found[field] {
				t.Fatalf("missing %q warning: %+v", field, warnings)
			}
		}
		root := decodeJSONMap(t, out)
		if _, ok := root["verbosity"]; ok {
			t.Fatalf("anthropic body carried verbosity: %s", out)
		}
		if _, ok := root["safety_identifier"]; ok {
			t.Fatalf("anthropic body carried safety_identifier: %s", out)
		}
	})
}
