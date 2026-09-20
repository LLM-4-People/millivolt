package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadQuotaConfig(t *testing.T, raw string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return LoadFile(path)
}

func TestQuotaPauseModeEnumAndDefaults(t *testing.T) {
	if got := Default().QuotaPauseMode; got != QuotaPauseOff {
		t.Fatalf("default quota_pause_mode = %q, want off", got)
	}
	if got := Default().QuotaPauseRecoverySuccesses; got != 1 {
		t.Fatalf("default quota_pause_recovery_successes = %d, want 1", got)
	}
	category := false
	for _, c := range Categories() {
		if c.ID == "quota" && c.Label == "Quota pause" {
			category = true
		}
	}
	if !category {
		t.Fatal("missing quota pause settings category")
	}
	for _, key := range []string{"quota_pause_mode", "quota_pause_recovery_successes"} {
		f := FieldByKey(key)
		if f == nil || f.Category != "quota" || !f.HotReload {
			t.Fatalf("schema does not expose a hot-reload quota field: %+v", f)
		}
	}
	if f := FieldByKey("quota_pause_mode"); f.Kind != KindString {
		t.Fatalf("quota_pause_mode kind = %v, want string", f.Kind)
	}
	// The enum is exact lowercase; unknown spellings are rejected at load
	// with the allowed set in the message (deny by default).
	for _, mode := range []string{QuotaPauseOff, QuotaPauseRetry, QuotaPauseManual} {
		c, err := loadQuotaConfig(t, "quota_pause_mode: "+mode)
		if err != nil {
			t.Fatalf("valid mode %q rejected: %v", mode, err)
		}
		if c.QuotaPauseMode != mode {
			t.Fatalf("mode %q did not survive load: %q", mode, c.QuotaPauseMode)
		}
	}
	for _, invalid := range []string{"OFF", "Retry", " automatic", `"off"`, "alt-off", "", "pause"} {
		c, err := loadQuotaConfig(t, "quota_pause_mode: "+invalid)
		if err != nil || c.QuotaPauseMode != QuotaPauseOff {
			t.Fatalf("invalid mode %q accepted (%v, %q)", invalid, err, c.QuotaPauseMode)
		}
		if c, err := loadQuotaConfig(t, "quota_pause_mode: off\nquota_pause_mode: "+invalid); err == nil && c.QuotaPauseMode != "off" {
			t.Fatalf("overlay accepted invalid mode %q", invalid)
		}
	}
	// Settings Apply coerces; Validate is the enum gate (same contract as
	// the storm bounds).
	c := Default()
	for _, mode := range []string{QuotaPauseRetry, QuotaPauseManual, QuotaPauseOff} {
		if err := c.Apply(map[string]any{"quota_pause_mode": mode}); err != nil {
			t.Fatalf("Apply %q: %v", mode, err)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate %q: %v", mode, err)
		}
	}
	if err := c.Apply(map[string]any{"quota_pause_mode": "sometimes"}); err != nil ||
		c.Validate() == nil || !strings.Contains(c.Validate().Error(), "must be one of off, retry, manual") {
		t.Fatalf("Settings accepted invalid mode (apply=%v)", err)
	}
}

func TestQuotaPauseRecoverySuccessesBounds(t *testing.T) {
	for _, invalid := range []string{"0", "101", "-1", "2.5", "null", "true", `"2"`} {
		c, err := loadQuotaConfig(t, "quota_pause_recovery_successes: "+invalid)
		if err != nil {
			t.Errorf("YAML %s: %v", invalid, err)
		} else if got := c.QuotaPauseRecoverySuccesses; got != 1 {
			t.Errorf("applied invalid YAML %s (got %d)", invalid, got)
		}
	}
	for _, valid := range []string{"1", "100"} {
		if c, err := loadQuotaConfig(t, "quota_pause_recovery_successes: "+valid); err != nil {
			t.Fatalf("valid %s rejected: %v", valid, err)
		} else if c.QuotaPauseRecoverySuccesses < 1 {
			t.Fatalf("valid %s did not apply", valid)
		}
	}
	c := Default()
	if err := c.Apply(map[string]any{"quota_pause_recovery_successes": 0}); err != nil || c.Validate() == nil {
		t.Fatal("Settings accepted recovery successes 0")
	}
	if err := c.Apply(map[string]any{"quota_pause_recovery_successes": 101}); err != nil || c.Validate() == nil {
		t.Fatal("Settings accepted recovery successes 101")
	}
	if !reflect.DeepEqual(Default().Clone(), Default()) {
		t.Fatal("clone drifted")
	}
}
