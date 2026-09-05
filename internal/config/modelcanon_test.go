package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apply is the test entry into the real pipeline path (compile once, apply
// per name) - the same functions the web fold uses.
func apply(rules []ModelRule, name string) string {
	return ApplyModelRules(CompileModelRules(rules), name)
}

// TestCanonicalModel is the Go half of the canonicalization contract - the
// SAME case table is mirrored in ui_check.js (the JS mirror canonicalModel
// in explorer.js); keep both in lockstep.
func TestCanonicalModel(t *testing.T) {
	def := DefaultModelRules()
	cases := []struct {
		name  string
		rules []ModelRule
		in    string
		want  string
	}{
		{"digit dots merge", def, "glm-5.3", "glm-5-3"},
		{"digit dots idempotent", def, "glm-5-3", "glm-5-3"},
		{"case folds", def, "GLM-5.3", "glm-5-3"},
		{"vendor strip", def, "moonshotai/kimi-k3", "kimi-k3"},
		{"vendor+tag strip", def, "moonshotai/kimi-k3:nube", "kimi-k3"},
		{"vendor+tag+dots", def, "z-ai/glm-5.3-flash:crofai", "glm-5-3-flash"},
		{"dated snapshot stays distinct", def, "deepseek-v4-pro-0813", "deepseek-v4-pro-0813"},
		{"mid-name dots", def, "gpt-4.1-mini", "gpt-4-1-mini"},
		{"trim", def, "  glm-5.3 ", "glm-5-3"},
		{"all-stripped falls back to raw", def, "z-ai/", "z-ai/"},
		{"fp8 quant merges with the base", def, "glm-5.3-fp8", "glm-5-3"},
		{"fp4 quant merges with the base", def, "glm-5.3-fp4", "glm-5-3"},
		{"nvfp4 architecture merges", def, "glm-5.3-nvfp4", "glm-5-3"},
		{"two-letter prefixed quant (nxfp4) merges", def, "GLM-5.3-NXFP4", "glm-5-3"},
		{"bf16 merges", def, "qwen3-32b-bf16", "qwen3-32b"},
		{"int8 merges", def, "llama-3.3-70b-int8", "llama-3-3-70b"},
		{"nf4 merges", def, "mistral-small-nf4", "mistral-small"},
		{"gguf quant (q4_k_m) merges", def, "qwen2.5-72b-q4_k_m", "qwen2-5-72b"},
		{"gguf i-quant (iq4_xs) merges", def, "qwen2.5-72b-iq4_xs", "qwen2-5-72b"},
		{"parameter-count suffix is NOT a quant", def, "gemma-3-27b", "gemma-3-27b"},
		{"quant mid-name is untouched", def, "fp4-lab-model", "fp4-lab-model"},
		{"exact rule merges the remainder", append([]ModelRule{{Mode: ModelRuleExact, From: "deepseek-v4-pro-0813", To: "deepseek-v4-pro"}}, def...), "deepseek-v4-pro-0813", "deepseek-v4-pro"},
		{"exact rule still rule-normalized after", append([]ModelRule{{Mode: ModelRuleExact, From: "x", To: "GLM-5.3"}}, def...), "x", "glm-5-3"},
		{"exact after normalization (pipeline order matters)", append(append([]ModelRule{}, def...), ModelRule{Mode: ModelRuleExact, From: "glm-5-3", To: "glm5"}), "glm-5.3", "glm5"},
		{"pattern rule is case-sensitive", []ModelRule{{Mode: ModelRulePattern, From: `^(.*)-flash$`, To: "$1"}, {Mode: ModelRuleLower}}, "GLM-5.3-FLASH", "glm-5.3-flash"},
		{"pattern rule with (?i) flag", []ModelRule{{Mode: ModelRulePattern, From: `(?i)^(.*)-flash$`, To: "$1"}, {Mode: ModelRuleLower}}, "GLM-5.3-FLASH", "glm-5.3"},
		{"empty pipeline = identity", nil, "GLM-5.3", "GLM-5.3"},
		{"empty pipeline trims only", nil, " glm-5.3 ", "glm-5.3"},
	}
	for _, c := range cases {
		if got := apply(c.rules, c.in); got != c.want {
			t.Errorf("%s: apply(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestModelCanonBuilder(t *testing.T) {
	c := Default()
	mc := c.ModelCanon()
	if len(mc.Rules) != 5 || mc.Rules[0].Mode != ModelRuleLower || mc.Rules[4].To != "$1-$2" || mc.Rules[3].To != "" {
		t.Fatalf("default rules = %+v, want the shipped five-step pipeline", mc.Rules)
	}
	// The payload carries a copy - mutating it must not leak into the config.
	mc.Rules[0].Mode = ModelRuleExact
	if c.ModelRules[0].Mode != ModelRuleLower {
		t.Fatal("ModelCanon must copy the rule slice")
	}
}

func TestModelRulesValidated(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unknown mode", "model_rules:\n  - {mode: fuzzy}\n", "mode must be one of exact, pattern, lower"},
		{"invalid pattern", "model_rules:\n  - {mode: pattern, from: \"a(\"}\n", "invalid pattern"},
		{"pattern missing from", "model_rules:\n  - {mode: pattern, to: x}\n", "needs a from pattern"},
		{"exact missing from", "model_rules:\n  - {mode: exact, to: x}\n", "needs a from spelling"},
		{"lower with from", "model_rules:\n  - {mode: lower, from: x}\n", "takes no from/to"},
		{"ok exact", "model_rules:\n  - {mode: exact, from: \"a-b\", to: ab}\n", ""},
		{"ok pattern", "model_rules:\n  - {mode: pattern, from: \"^x/\", to: \"\"}\n", ""},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "proxy.yaml")
		if err := os.WriteFile(p, []byte(c.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadFile(p)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: LoadFile: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: LoadFile accepted an invalid rule list", c.name)
		} else if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error = %q, want substring %q", c.name, err.Error(), c.wantErr)
		}
	}
}

// TestModelRulesCap pins the pipeline-length guardrail (an unbounded chain
// cannot be memoized meaningfully; the cap is an internal bound, not a
// tunable).
func TestModelRulesCap(t *testing.T) {
	rules := make([]ModelRule, ModelRulesMax+1)
	for i := range rules {
		rules[i] = ModelRule{Mode: ModelRuleLower}
	}
	if err := ValidateModelRules(rules); err == nil || !strings.Contains(err.Error(), "exceeds the cap") {
		t.Fatalf("cap error = %v, want exceeds-the-cap", err)
	}
}

// TestModelRulesPresenceMerge pins the slice-merge semantics: a written
// empty rule list really means "no rules" (exact grouping) - the zero-guard
// never resurrects the default pipeline - and a written default round-trips.
func TestModelRulesPresenceMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proxy.yaml")
	if err := os.WriteFile(p, []byte("model_rules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ModelRules) != 0 {
		t.Fatalf("explicit empty model_rules = %+v, want zero rules (exact grouping)", cfg.ModelRules)
	}
}

// TestModelRuleDisabledSkipped: a parked rule must cost nothing at fold
// time - CompileModelRules skips it, so the pipeline behaves exactly as if
// it were absent (the Grafana-eye / Stripe-disable A/B affordance).
func TestModelRuleDisabledSkipped(t *testing.T) {
	rules := []ModelRule{
		{Mode: ModelRuleLower},
		{Mode: ModelRuleExact, From: "glm-5-3", To: "glm-5.3", Disabled: true},
		{Mode: ModelRulePattern, From: `(\d)\.(\d)`, To: "$1-$2"},
	}
	if err := ValidateModelRules(rules); err != nil {
		t.Fatalf("disabled pipeline rejected: %v", err)
	}
	got := ApplyModelRules(CompileModelRules(rules), "GLM-5.3")
	if got != "glm-5-3" {
		t.Fatalf("disabled exact rule still fired: %q", got)
	}
}

// TestModelRuleDisabledSkipsBodyValidation: the point of parking a rule is
// keeping a broken or unfinished pattern without failing the load - the
// mode stays structural (a garbage mode is still rejected), the body does
// not.
func TestModelRuleDisabledSkipsBodyValidation(t *testing.T) {
	bad := []ModelRule{{Mode: ModelRulePattern, From: "a(", Disabled: true}, {Mode: ModelRuleExact, Disabled: true}}
	if err := ValidateModelRules(bad); err != nil {
		t.Fatalf("disabled draft rules rejected: %v", err)
	}
	if err := ValidateModelRules([]ModelRule{{Mode: "banana", Disabled: true}}); err == nil {
		t.Fatalf("disabled rule with an invalid mode accepted")
	}
	if err := ValidateModelRules([]ModelRule{{Mode: ModelRulePattern, From: "a("}}); err == nil {
		t.Fatalf("enabled broken pattern accepted")
	}
}

// TestModelRuleExactNoPadding: exact compares the whole string against the
// TRIMMED stored spelling - a padded from is a dead rule, rejected at the
// boundary (the UI mirrors the same error live).
func TestModelRuleExactNoPadding(t *testing.T) {
	bad := []ModelRule{{Mode: ModelRuleExact, From: " glm-5.3 "}}
	if err := ValidateModelRules(bad); err == nil {
		t.Fatalf("padded exact from accepted (would never fire)")
	}
	ok := []ModelRule{{Mode: ModelRulePattern, From: `^\s+`}} // spaces in PATTERNS are meaningful
	if err := ValidateModelRules(ok); err != nil {
		t.Fatalf("pattern with spaces rejected: %v", err)
	}
	// A parked rule skips body checks - padding stays allowed while parked.
	if err := ValidateModelRules([]ModelRule{{Mode: ModelRuleExact, From: " x ", Disabled: true}}); err != nil {
		t.Fatalf("padded parked exact rule rejected: %v", err)
	}
}
