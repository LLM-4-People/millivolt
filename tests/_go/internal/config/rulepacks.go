package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePack(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func enabledBase(dir string) *Config {
	c := Default()
	c.PluginsEnabled = true
	c.PluginDir = dir
	return c
}

func TestRulePacksMergeAllowedKeysInFileOrder(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "b-rules.yaml", "model_rules:\n  - mode: lower\nrequest_overrides:\n  - client: pack-client\n    headers:\n      X-Pack: yes\n")
	writePack(t, dir, "a-aliases.yml", "provider_aliases:\n  old.example: new.example\n")
	// Non-YAML neighbours are ignored, not treated as invalid packs.
	writePack(t, dir, "README.txt", "not a pack")

	base := enabledBase(dir)
	merged, origins, err := ApplyRulePacks(base)
	if err != nil {
		t.Fatalf("ApplyRulePacks: %v", err)
	}
	if len(merged.ModelRules) != len(base.ModelRules)+1 {
		t.Fatalf("model_rules: got %d, want %d", len(merged.ModelRules), len(base.ModelRules)+1)
	}
	if len(merged.RequestOverrides) != 1 || merged.RequestOverrides[0].Client != "pack-client" {
		t.Fatalf("request_overrides: got %+v", merged.RequestOverrides)
	}
	if merged.ProviderAliases["old.example"] != "new.example" {
		t.Fatalf("provider_aliases: got %+v", merged.ProviderAliases)
	}
	if len(origins) != 2 || origins[0].File != "a-aliases.yml" || origins[1].File != "b-rules.yaml" {
		t.Fatalf("origins out of order: %+v", origins)
	}
	if len(origins[0].Keys) != 1 || origins[0].Keys[0] != "provider_aliases" {
		t.Fatalf("origin keys: %+v", origins[0])
	}
	if base.PluginsEnabled != true || base.PluginDir != dir || len(base.ModelRules) != len(merged.ModelRules)-1 {
		t.Fatal("ApplyRulePacks mutated its input")
	}
}

func TestRulePacksRejectUnknownKeyWithoutPartialMerge(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "bad.yaml", "model_rules:\n  - mode: lower\nlisten: \":9999\"\n")
	cfg, origins, err := ApplyRulePacks(enabledBase(dir))
	if err == nil {
		t.Fatalf("ApplyRulePacks = %+v, %+v, want an error", cfg, origins)
	}
	if cfg != nil || origins != nil {
		t.Fatalf("failed merge returned data: %+v %+v", cfg, origins)
	}
	if !strings.Contains(err.Error(), `key "listen" is not allowed`) {
		t.Fatalf("error %q must name the disallowed key", err)
	}
}

func TestRulePacksRejectDuplicateProviderLabel(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "dup.yaml", "providers:\n  neutral.example:\n    usage_keys: {}\n")
	base := enabledBase(dir)
	base.Providers = map[string]ProviderOverride{"neutral.example": {}}
	_, _, err := ApplyRulePacks(base)
	if err == nil || !strings.Contains(err.Error(), "duplicates an existing key") {
		t.Fatalf("ApplyRulePacks error = %v, want a duplicate-label failure", err)
	}
}

func TestRulePacksEnforcePerPackEntryCap(t *testing.T) {
	dir := t.TempDir()
	var body strings.Builder
	body.WriteString("request_overrides:\n")
	for i := 0; i < RequestOverridesMax+1; i++ {
		fmt.Fprintf(&body, "  - client: c%d\n    headers:\n      X-Test: yes\n", i)
	}
	writePack(t, dir, "many.yaml", body.String())
	_, _, err := ApplyRulePacks(enabledBase(dir))
	if err == nil || (!strings.Contains(err.Error(), "per-pack cap") && !strings.Contains(err.Error(), "exceeds the cap of 64")) {
		t.Fatalf("ApplyRulePacks error = %v, want a cap failure", err)
	}
}

func TestRulePacksRejectOversizedFile(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "huge.yaml", strings.Repeat("x", RulePackMaxBytes+1))
	_, _, err := ApplyRulePacks(enabledBase(dir))
	if err == nil || !strings.Contains(err.Error(), "byte cap") {
		t.Fatalf("ApplyRulePacks error = %v, want the byte cap", err)
	}
}

func TestRulePacksDisabledIgnoresDirectory(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "bad.yaml", "this: is not a valid pack key\n")
	base := Default()
	base.PluginDir = dir
	cfg, origins, err := ApplyRulePacks(base)
	if err != nil {
		t.Fatalf("ApplyRulePacks with plugins disabled: %v", err)
	}
	if origins != nil || len(cfg.RulePackOrigins) != 0 {
		t.Fatalf("disabled packs produced origins: %+v", origins)
	}
}

func TestAdminConfigDocReportsRulePackOrigins(t *testing.T) {
	h := &Handler{
		Path:      filepath.Join(t.TempDir(), "config.yaml"),
		Startup:   Default(),
		Effective: Default,
		RulePacks: func() any {
			return []RulePackOrigin{{File: "a.yaml", Keys: []string{"model_rules"}}}
		},
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/config status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RulePacks []RulePackOrigin `json:"rule_packs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.RulePacks) != 1 || body.RulePacks[0].File != "a.yaml" {
		t.Fatalf("rule_packs in GET doc = %+v", body.RulePacks)
	}
}
