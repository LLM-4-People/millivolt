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

// TestErrorClassExtensionListsConfig pins the two vocabulary-extension
// settings added alongside the classifier (rate_limit_error_classes,
// quota_error_classes) plus the rules all three extension lists share: empty
// defaults, YAML round-trip, case-insensitive uniqueness within a list, no
// spelling in two lists, and no entry that names a built-in spelling of a
// different class (an extension may only add, never move a built-in).
func TestErrorClassExtensionListsConfig(t *testing.T) {
	def := Default()
	for key, got := range map[string][]string{
		"rate_limit_error_classes": def.RateLimitErrorClasses,
		"quota_error_classes":      def.QuotaErrorClasses,
	} {
		if len(got) != 0 {
			t.Fatalf("default %s = %q, want none (built-ins only)", key, got)
		}
	}
	for _, raw := range []string{
		`rate_limit_error_classes: ["gateway_throttle"]`,
		`quota_error_classes: ["account_depleted", "plan_exhausted"]`,
	} {
		c, err := loadStormConfig(t, raw)
		if err != nil {
			t.Errorf("valid YAML %s: %v", raw, err)
		} else if err := c.Validate(); err != nil {
			t.Errorf("valid YAML %s rejected by Validate: %v", raw, err)
		}
	}
	// Settings Apply accepts the lists; Validate is the value gate.
	c := Default()
	if err := c.Apply(map[string]any{
		"rate_limit_error_classes": []string{"gateway_throttle"},
		"quota_error_classes":      []string{"account_depleted"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.RateLimitErrorClasses, []string{"gateway_throttle"}) ||
		!reflect.DeepEqual(c.QuotaErrorClasses, []string{"account_depleted"}) {
		t.Fatalf("applied lists = %q / %q, want the submitted values", c.RateLimitErrorClasses, c.QuotaErrorClasses)
	}
	// Every invalid shape must fail Validate with the owning list named.
	for name, mutate := range map[string]func(*Config){
		"blank rate-limit entry": func(c *Config) { c.RateLimitErrorClasses = []string{"  "} },
		"duplicate quota entry":  func(c *Config) { c.QuotaErrorClasses = []string{"a_b", "A_B"} },
		"cross-list duplicate": func(c *Config) {
			c.RateLimitErrorClasses = []string{"warming_up"}
			c.RetryableErrorClasses = []string{"warming_up"}
		},
		"built-in quota in limits": func(c *Config) { c.RateLimitErrorClasses = []string{"insufficient_quota"} },
		"built-in retryable in quota": func(c *Config) {
			c.QuotaErrorClasses = []string{"server_error"}
		},
		"built-in rate limit in quota": func(c *Config) {
			c.QuotaErrorClasses = []string{"tier_limit_exceeded"}
		},
	} {
		c := Default()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted %s", name)
		}
	}
	// A built-in of the SAME class is a harmless no-op, not a remap.
	c = Default()
	c.RateLimitErrorClasses = []string{"tier_limit_exceeded"}
	c.QuotaErrorClasses = []string{"insufficient_quota"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate rejected same-class built-in spellings: %v", err)
	}
	// The clone copies each new slice.
	src := Default()
	src.RateLimitErrorClasses = []string{"gateway_throttle"}
	src.QuotaErrorClasses = []string{"account_depleted"}
	clone := src.Clone()
	clone.RateLimitErrorClasses[0] = "mutated"
	clone.QuotaErrorClasses[0] = "mutated"
	if src.RateLimitErrorClasses[0] != "gateway_throttle" || src.QuotaErrorClasses[0] != "account_depleted" {
		t.Fatal("Clone aliases an error-class extension slice")
	}
}

// TestClientErrorClassConfig pins the client_error_class enum: off by
// default, the exact lowercase members accepted, anything else denied at load
// (deny by default), and the clone/value plumbing intact.
func TestClientErrorClassConfig(t *testing.T) {
	if got := Default().ClientErrorClass; got != ClientErrorClassOff {
		t.Fatalf("default client_error_class = %q, want %q", got, ClientErrorClassOff)
	}
	for _, raw := range []string{`client_error_class: "off"`, `client_error_class: "header"`} {
		c, err := loadStormConfig(t, raw)
		if err != nil {
			t.Errorf("valid YAML %s: %v", raw, err)
		} else if err := c.Validate(); err != nil {
			t.Errorf("valid YAML %s rejected by Validate: %v", raw, err)
		}
	}
	for _, invalid := range []string{`client_error_class: "HEADER"`, `client_error_class: "body"`, `client_error_class: ""`} {
		c, err := loadStormConfig(t, invalid)
		if err != nil {
			t.Errorf("invalid YAML %s: %v (the loader skips the key, not the file)", invalid, err)
			continue
		}
		if c.ClientErrorClass != ClientErrorClassOff {
			t.Errorf("invalid YAML %s applied %q, want the default kept", invalid, c.ClientErrorClass)
		}
	}
	// The value gate: an invalid enum must fail Validate, not fall back.
	c := Default()
	c.ClientErrorClass = "Body"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown client_error_class")
	}
	if err := c.Apply(map[string]any{"client_error_class": ClientErrorClassHeader}); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Clone().ClientErrorClass != ClientErrorClassHeader {
		t.Fatal("Clone dropped client_error_class")
	}
}
