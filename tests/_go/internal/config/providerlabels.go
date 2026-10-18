package config

import (
	"slices"
	"strings"
	"testing"
)

// TestProviderAliasesCanonicalizedAtValidate pins the one provider_aliases
// policy: both sides are trimmed and lowercased before the self/chain checks,
// so a padded chain cannot pass validation and then split history.
func TestProviderAliasesCanonicalizedAtValidate(t *testing.T) {
	t.Run("padded chain is rejected after normalization", func(t *testing.T) {
		c := Default()
		c.ProviderAliases = map[string]string{"a": " b ", "b": "c"}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "chains are not resolved") {
			t.Fatalf("Validate() = %v, want the normalized chain rejection", err)
		}
	})
	t.Run("padded pair normalizes", func(t *testing.T) {
		c := Default()
		c.ProviderAliases = map[string]string{" Old.Example ": " New.Example "}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if len(c.ProviderAliases) != 1 || c.ProviderAliases["old.example"] != "new.example" {
			t.Fatalf("ProviderAliases = %#v, want the canonical single pair", c.ProviderAliases)
		}
	})
	t.Run("valid aliases are unchanged", func(t *testing.T) {
		c := Default()
		c.ProviderAliases = map[string]string{"old.example": "new.example", "legacy.example": "new.example"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if len(c.ProviderAliases) != 2 ||
			c.ProviderAliases["old.example"] != "new.example" ||
			c.ProviderAliases["legacy.example"] != "new.example" {
			t.Fatalf("ProviderAliases = %#v, want both pairs unchanged", c.ProviderAliases)
		}
	})
	t.Run("normalized key collision is rejected", func(t *testing.T) {
		c := Default()
		c.ProviderAliases = map[string]string{"Old.Example": "a.example", "old.example": "b.example"}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "collide") {
			t.Fatalf("Validate() = %v, want the normalized-key collision rejection", err)
		}
	})
}

// TestProviderLabelsRequireCanonicalForm pins the one provider-label policy:
// only the lowercase, whitespace-free label the request path derives can ever
// match, so a padded or mixed-case section is rejected instead of silently
// doing nothing.
func TestProviderLabelsRequireCanonicalForm(t *testing.T) {
	for _, label := range []string{"openai.com", "cursor.sh", "gw", "localhost", "192.168.1.1:8080", "[::1]:8080"} {
		t.Run("accepts "+label, func(t *testing.T) {
			c := Default()
			c.Providers = map[string]ProviderOverride{label: {}}
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want the canonical label accepted", err)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		label string
		want  string
	}{
		{"padded", " OpenAI.com ", "whitespace"},
		{"mixed case", "OpenAI.com", "lowercase"},
		{"trailing space", "openai.com ", "whitespace"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			c := Default()
			c.Providers = map[string]ProviderOverride{tc.label: {}}
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), "providers") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want a providers label error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestProviderIdentityYAMLLoad pins the YAML boundary too: a padded alias chain
// or a padded/mixed-case provider label is dropped with the key named in the
// skipped list, never applied and never silently kept.
func TestProviderIdentityYAMLLoad(t *testing.T) {
	t.Run("padded alias chain", func(t *testing.T) {
		c, skipped, err := LoadBytes([]byte("provider_aliases:\n  a: \" b \"\n  b: c\n"))
		if err != nil {
			t.Fatalf("LoadBytes() = %v, want the invalid key skipped", err)
		}
		if len(c.ProviderAliases) != 0 {
			t.Fatalf("ProviderAliases = %#v, want none", c.ProviderAliases)
		}
		if !slices.Contains(skipped, "provider_aliases") {
			t.Fatalf("skipped = %v, want provider_aliases named", skipped)
		}
	})
	for _, label := range []string{" OpenAI.com ", "OpenAI.com"} {
		t.Run("provider label "+label, func(t *testing.T) {
			body := "providers:\n  \"" + label + "\":\n    models_path: /v1/models\n"
			c, skipped, err := LoadBytes([]byte(body))
			if err != nil {
				t.Fatalf("LoadBytes() = %v, want the invalid key skipped", err)
			}
			if _, present := c.Providers[label]; present {
				t.Fatalf("Providers kept the non-canonical label %q", label)
			}
			if !slices.Contains(skipped, "providers") {
				t.Fatalf("skipped = %v, want providers named", skipped)
			}
		})
	}
}

// TestApplyProviderAliasesNormalizes pins the Settings POST path: Apply uses
// the same normalizer, so the map is canonical before Validate runs.
func TestApplyProviderAliasesNormalizes(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{" Old.Example ": " New.Example "}}); err != nil {
		t.Fatalf("Apply() = %v, want nil", err)
	}
	if len(c.ProviderAliases) != 1 || c.ProviderAliases["old.example"] != "new.example" {
		t.Fatalf("ProviderAliases = %#v, want the canonical pair", c.ProviderAliases)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
