package config

import (
	"reflect"
	"testing"
)

// TestRetryableErrorClassesConfig pins the retryable_error_classes setting:
// the default is empty (the built-in vocabulary stands alone), valid
// extension lists round-trip through YAML and Settings, and the trust
// boundary rejects empty/whitespace entries, duplicates, and durable
// quota/billing classes (waiting can never restore credits, so no operator
// list may make one retryable). Invalid YAML values follow the loader's
// keepYAMLKeys contract: the key is skipped and the default stands, exactly
// like storm_status_codes.
func TestRetryableErrorClassesConfig(t *testing.T) {
	if got := Default().RetryableErrorClasses; len(got) != 0 {
		t.Fatalf("default retryable_error_classes = %q, want none (built-ins only)", got)
	}

	// Valid extensions round-trip via the YAML file loader.
	for _, raw := range []string{
		"retryable_error_classes: []",
		`retryable_error_classes: ["model_warming_up"]`,
		`retryable_error_classes: ["model_warming_up", "gateway_hiccup"]`,
	} {
		c, err := loadStormConfig(t, raw) // the storm suite's generic YAML loader
		if err != nil {
			t.Errorf("valid YAML %s: %v", raw, err)
		} else if err := c.Validate(); err != nil {
			t.Errorf("valid YAML %s rejected by Validate: %v", raw, err)
		}
	}
	// Invalid YAML values are skipped by the loader, never coerced: the
	// field keeps the built-in-default (empty) value.
	for _, invalid := range []string{"null", "[500]", "[true]", "[null]", "500", `"api_error"`} {
		c, err := loadStormConfig(t, "retryable_error_classes: "+invalid)
		if err != nil {
			t.Errorf("invalid YAML %s: %v (the loader skips the key, not the file)", invalid, err)
		} else if len(c.RetryableErrorClasses) != 0 {
			t.Errorf("invalid YAML %s applied %q, want the default kept", invalid, c.RetryableErrorClasses)
		}
	}
	// Settings Apply accepts a valid list; Validate is the value gate.
	c := Default()
	if err := c.Apply(map[string]any{"retryable_error_classes": []string{"model_warming_up"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.RetryableErrorClasses, []string{"model_warming_up"}) {
		t.Fatalf("applied list = %q, want [model_warming_up]", c.RetryableErrorClasses)
	}
	// The value gate: every invalid list must fail Validate.
	for name, invalid := range map[string][]string{
		"duplicate (case-insensitive)": {"model_warming_up", "MODEL_WARMING_UP"},
		"empty entry":                  {"model_warming_up", ""},
		"whitespace entry":             {"  "},
		"quota class":                  {"insufficient_quota"},
		"quota code":                   {"exceeded_current_quota_error"},
		"billing class":                {"billing_not_active"},
	} {
		c := Default()
		c.RetryableErrorClasses = invalid
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted %s list %q", name, invalid)
		}
	}
	// The clone copies the slice (no aliasing of the source).
	src := Default()
	src.RetryableErrorClasses = []string{"model_warming_up"}
	clone := src.Clone()
	clone.RetryableErrorClasses[0] = "mutated"
	if src.RetryableErrorClasses[0] != "model_warming_up" {
		t.Fatal("Clone aliases the retryable_error_classes slice")
	}
}
