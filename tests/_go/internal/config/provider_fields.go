package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestApplyProviderAliases pins the Settings-sheet path for provider_aliases:
// the JSON map coerces into the Go field, a malformed pair fails the Apply,
// and Map() round-trips the map so the editor cannot silently drop it.
func TestApplyProviderAliases(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{"old.example": "new.example"}}); err != nil {
		t.Fatalf("Apply(valid aliases) = %v, want nil", err)
	}
	if c.ProviderAliases["old.example"] != "new.example" {
		t.Errorf("ProviderAliases = %v, want old.example → new.example", c.ProviderAliases)
	}
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{"old.example": ""}}); err == nil {
		t.Error("Apply(empty target) = nil error, want rejection")
	}
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{"old.example": 5}}); err == nil {
		t.Error("Apply(non-string target) = nil error, want rejection")
	}
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{}}); err != nil {
		t.Errorf("Apply(empty map) = %v, want nil", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	m := c.Map()
	aa, ok := m["provider_aliases"].(map[string]string)
	if !ok {
		t.Fatalf("Map()[provider_aliases] = %T, want map[string]string", m["provider_aliases"])
	}
	if _, present := aa["old.example"]; present {
		t.Error("Map() resurrected a removed alias; settings saves would never be able to delete one")
	}
}

// Clone must deep-copy EVERY nested provider map. Regression: a broken
// indentation made the ProviderAliases clone run inside `if c.Providers !=
// nil`, so a config with providers unset (Default()'s shape) but aliases set
// handed the NEW snapshot the OLD snapshot's alias map - two live snapshots
// sharing one mutable map.
func TestCloneProvidersAndAliasesIndependent(t *testing.T) {
	c := Default()
	c.Providers = nil // Default() ships Providers nil - the exact broken case
	c.ProviderAliases = map[string]string{"old.example": "new.example"}

	clone := c.Clone()
	clone.ProviderAliases["old.example"] = "mutated"
	if c.ProviderAliases["old.example"] != "new.example" {
		t.Fatalf("clone shares the ProviderAliases map with the original")
	}

	c2 := Default()
	c2.Providers = map[string]ProviderOverride{
		"prov.example": {
			CostKeys:   []string{"usage.cost"},
			UsageKeys:  map[string]string{"input_tokens": "p"},
			ModelsPath: "/meta",
			ModelsKeys: map[string]string{"input_modalities": "modalities"},
		},
	}
	clone2 := c2.Clone()
	clone2.Providers["prov.example"].UsageKeys["input_tokens"] = "mutated"
	clone2.Providers["prov.example"].ModelsKeys["input_modalities"] = "mutated"
	clone2.Providers["prov.example"].CostKeys[0] = "mutated"
	if c2.Providers["prov.example"].UsageKeys["input_tokens"] != "p" ||
		c2.Providers["prov.example"].ModelsKeys["input_modalities"] != "modalities" ||
		c2.Providers["prov.example"].CostKeys[0] != "usage.cost" {
		t.Fatalf("clone shares provider override maps with the original: %+v", c2.Providers["prov.example"])
	}
}

// The models enrichment override loads, validates, and round-trips: models_path
// must be a clean path, models_keys values must be dotted JSON paths - a bad
// value fails the load (deny by default), never clamps.
func TestProviderModelsOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := `providers:
  prov.example:
    models_path: "/language-models"
    models_keys:
      input_modalities: "input_modalities"
      max_output_tokens: "limits.max_output"
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ov := c.Providers["prov.example"]
	if ov.ModelsPath != "/language-models" {
		t.Errorf("models_path = %q", ov.ModelsPath)
	}
	if ov.ModelsKeys["max_output_tokens"] != "limits.max_output" {
		t.Errorf("models_keys = %v", ov.ModelsKeys)
	}

	bad := []struct {
		name string
		yaml string
	}{
		{"relative path", "providers:\n  p:\n    models_path: language-models\n"},
		{"query in path", "providers:\n  p:\n    models_path: \"/m?x=1\"\n"},
		{"space in path", "providers:\n  p:\n    models_path: \"/mo dels\"\n"},
		{"non-dotted key path", "providers:\n  p:\n    models_keys:\n      input_modalities: \"a b\"\n"},
		{"empty field name", "providers:\n  p:\n    models_keys:\n      \" \": \"x\"\n"},
	}
	for _, tc := range bad {
		bp := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(bp, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadFileOK(t, tc.yaml)
		if len(c.Providers) != 0 {
			t.Errorf("%s: invalid models override applied: %+v", tc.name, c.Providers)
		}
	}
}

// Provider headers load, validate, and survive a WriteFile round trip: names
// must be RFC 7230 tokens, values single-line printable header values - a bad
// value fails the load (deny by default), never clamps.
func TestProviderHeaders(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := `providers:
  prov.example:
    headers:
      User-Agent: "my-shell/1.2.3 ({{platform}})"
      x-req-id: "{{uuid4}}"
      x-static: "v1"
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Providers["prov.example"].Headers
	if h["User-Agent"] != "my-shell/1.2.3 ({{platform}})" || h["x-req-id"] != "{{uuid4}}" || h["x-static"] != "v1" {
		t.Fatalf("headers = %v", h)
	}

	// Round trip: the written file parses back to the same map.
	out := filepath.Join(dir, "out.yaml")
	if err := WriteFile(out, c); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := c2.Providers["prov.example"].Headers
	if len(got) != 3 || got["x-req-id"] != "{{uuid4}}" || got["User-Agent"] != "my-shell/1.2.3 ({{platform}})" {
		t.Fatalf("round-tripped headers = %v", got)
	}

	bad := []struct{ name, yaml string }{
		{"bad name (space)", "providers:\n  p:\n    headers:\n      \"bad name\": \"v\"\n"},
		{"empty name", "providers:\n  p:\n    headers:\n      \"\": \"v\"\n"},
		{"empty value", "providers:\n  p:\n    headers:\n      x-a: \"\"\n"},
		{"newline in value", "providers:\n  p:\n    headers:\n      x-a: \"one\\ntwo\"\n"},
		{"control char in value", "providers:\n  p:\n    headers:\n      x-a: \"one\\x01two\"\n"},
	}
	for _, tc := range bad {
		bp := filepath.Join(dir, "badh.yaml")
		if err := os.WriteFile(bp, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadFileOK(t, tc.yaml)
		if len(c.Providers) != 0 {
			t.Errorf("%s: invalid provider header applied: %+v", tc.name, c.Providers)
		}
	}
}

// ValidHeaderValue owns the RFC 7230 field-value grammar alone: the empty
// string passes the grammar (an X-Proxy-Auth-Prefix may legitimately be "";
// the provider-header mapping rejects empty as its own policy, pinned by
// the "empty value" row above), and control bytes never pass.
func TestValidHeaderValueGrammar(t *testing.T) {
	if !ValidHeaderValue("") {
		t.Error("empty string must pass the grammar: emptiness is caller policy")
	}
	for _, v := range []string{"v", "Bearer ", "tab\tvalue", "caf\xc3\xa9 unicode", "obs-text \xff"} {
		if !ValidHeaderValue(v) {
			t.Errorf("ValidHeaderValue(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"one\ntwo", "one\rtwo", "nul\x00byte", "ctl\x01byte", "del\x7fbyte"} {
		if ValidHeaderValue(v) {
			t.Errorf("ValidHeaderValue(%q) = true, want false", v)
		}
	}
}

// cost_keys entries are dotted JSON paths resolved by metrics.DigJSON; a
// malformed path is a cost key that can never resolve, so it is rejected at
// the load boundary with a clear error (deny by default), never silently
// applied. Before this validation existed, garbage paths loaded cleanly.
func TestProviderCostKeysPathValidated(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"space in path", "providers:\n  gw:\n    cost_keys: [\"usage cost\"]\n",
			`providers.gw.cost_keys: "usage cost" is not a dotted JSON path`},
		{"double dot", "providers:\n  gw:\n    cost_keys: [\"usage..cost\"]\n",
			`providers.gw.cost_keys: "usage..cost" is not a dotted JSON path`},
		{"empty path", "providers:\n  gw:\n    cost_keys: [\"\"]\n",
			`providers.gw.cost_keys: "" is not a dotted JSON path`},
		{"bare dot", "providers:\n  gw:\n    cost_keys: [\".\"]\n",
			`providers.gw.cost_keys: "." is not a dotted JSON path`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c := loadFileOK(t, tc.yaml)
			if len(c.Providers) != 0 {
				t.Fatalf("LoadFile applied a bad cost_keys path:\n%s", tc.yaml)
			}
		})
	}
}

// ensure_tools entries relay into the upstream tools array verbatim; an
// empty name would inject a nameless stub and a duplicate would inject the
// same stub twice, so both are rejected at the load boundary with the entry
// dropped (deny by default), never silently applied.
func TestProviderEnsureToolsValidated(t *testing.T) {
	cases := []struct{ name, yaml string }{
		{"empty name", "providers:\n  gw:\n    ensure_tools: [\"\"]\n"},
		{"whitespace name", "providers:\n  gw:\n    ensure_tools: [\"  \"]\n"},
		{"duplicate name", "providers:\n  gw:\n    ensure_tools: [\"bash\", \"bash\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadFileOK(t, tc.yaml)
			if len(c.Providers) != 0 {
				t.Fatalf("LoadFile applied a bad ensure_tools entry:\n%s", tc.yaml)
			}
		})
	}
	valid := "providers:\n  gw:\n    ensure_tools: [\"bash\", \"read\"]\n"
	c := loadFileOK(t, valid)
	if len(c.Providers) != 1 || len(c.Providers["gw"].EnsureTools) != 2 {
		t.Fatalf("LoadFile dropped a valid ensure_tools entry:\n%s", valid)
	}
}

// usage_keys: the VALUE must be a dotted JSON path (metrics.DigJSON resolves
// it), and the KEY must be a canonical usage field - one of
// metrics.CanonicalUsageFields, the single owner the Settings dropdown offers
// and ParseUsage consumes. A typo on either side is a silent runtime no-op,
// so both are rejected at load with a clear error. Before this validation
// existed, both garbage sides loaded cleanly.
func TestProviderUsageKeysValidated(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"bad value path", "providers:\n  gw:\n    usage_keys:\n      input_tokens: \"usage cost\"\n",
			`providers.gw.usage_keys: input_tokens: "usage cost" is not a dotted JSON path`},
		{"double-dot value path", "providers:\n  gw:\n    usage_keys:\n      output_tokens: \"toks..out\"\n",
			`providers.gw.usage_keys: output_tokens: "toks..out" is not a dotted JSON path`},
		{"typo'd canonical key", "providers:\n  gw:\n    usage_keys:\n      input_token: \"prompt_toks\"\n",
			`providers.gw.usage_keys: "input_token" is not a canonical usage field`},
		{"empty canonical key", "providers:\n  gw:\n    usage_keys:\n      \"\": \"prompt_toks\"\n",
			`providers.gw.usage_keys: "" is not a canonical usage field`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c := loadFileOK(t, tc.yaml)
			if len(c.Providers) != 0 {
				t.Fatalf("LoadFile applied a bad usage_keys entry:\n%s", tc.yaml)
			}
		})
	}

	// Every canonical field, mapped to a valid path, loads cleanly - the
	// validation must accept exactly the set metrics.ParseUsage consumes.
	allCanonical := "providers:\n  gw:\n    usage_keys:\n"
	for _, f := range metrics.CanonicalUsageFields {
		allCanonical += "      " + f + ": \"custom." + f + "\"\n"
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(allCanonical), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p); err != nil {
		t.Errorf("LoadFile(all canonical usage_keys) = %v, want nil. YAML:\n%s", err, allCanonical)
	}
}

// A provider override keyed by an empty or whitespace-only label can never
// apply - the runtime label is derived from the base URL host and is never
// empty - so the whole section would be a silent no-op. Rejected at load
// (the YAML path), mirroring the Settings POST path's rule in values.go
// (asProviders); the quoted empty key is legal YAML, so it must not slip
// through LoadFile.
func TestProviderLabelValidated(t *testing.T) {
	for _, yaml := range []string{
		"providers:\n  \"\":\n    cost_keys: [\"a.b\"]\n",
		"providers:\n  \"  \":\n    headers:\n      x-a: \"v\"\n",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadFileOK(t, yaml)
		if len(c.Providers) != 0 {
			t.Errorf("LoadFile applied an empty/whitespace provider label:\n%s", yaml)
		}
	}

	// A properly labeled override still loads.
	p := filepath.Join(t.TempDir(), "ok.yaml")
	if err := os.WriteFile(p, []byte("providers:\n  gw.example:\n    cost_keys: [\"a.b\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p); err != nil {
		t.Errorf("LoadFile(labeled provider) = %v, want nil", err)
	}
}
