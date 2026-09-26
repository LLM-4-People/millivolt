package config

import (
	"strings"
	"testing"
)

// TestAppendRetriesConfig pins the append_retries setting: the class is
// opt-in (0 by default, so no existing deployment changes behavior), a valid
// value round-trips through YAML and the Settings schema, and the trust
// boundary rejects out-of-band values exactly like the quality and thinking
// budgets it sits beside.
func TestAppendRetriesConfig(t *testing.T) {
	if got := Default().AppendRetries; got != 0 {
		t.Fatalf("default append_retries = %d, want 0 (the class is opt-in)", got)
	}
	if AppendRetriesMax != QualityRetriesMax {
		t.Fatalf("append_retries band %d must match the sibling budgets %d", AppendRetriesMax, QualityRetriesMax)
	}

	for _, raw := range []string{"append_retries: 0", "append_retries: 1", "append_retries: 3"} {
		c, err := loadStormConfig(t, raw)
		if err != nil {
			t.Errorf("valid YAML %s: %v", raw, err)
			continue
		}
		if err := c.Validate(); err != nil {
			t.Errorf("valid YAML %s rejected by Validate: %v", raw, err)
		}
	}

	for _, bad := range []int{-1, AppendRetriesMax + 1} {
		c := Default()
		c.AppendRetries = bad
		err := c.Validate()
		if err == nil {
			t.Errorf("append_retries %d accepted, want rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "append_retries") {
			t.Errorf("append_retries %d error must name the key, got %v", bad, err)
		}
	}

	// The schema is the settings-form owner: the key must be present, typed,
	// banded and documented, or the dashboard cannot offer it.
	found := false
	for _, f := range Schema() {
		if f.Key != "append_retries" {
			continue
		}
		found = true
		if f.Kind != KindInt {
			t.Errorf("append_retries kind = %v, want %v", f.Kind, KindInt)
		}
		if f.Min == nil || f.Max == nil || *f.Min != 0 || *f.Max != AppendRetriesMax {
			t.Errorf("append_retries band = %v..%v, want 0..%d", f.Min, f.Max, AppendRetriesMax)
		}
		if !f.HotReload {
			t.Error("append_retries must hot-reload (it only gates new rescue decisions)")
		}
		if f.ZeroMeans == "" {
			t.Error("append_retries must state that 0 disables it")
		}
	}
	if !found {
		t.Fatal("append_retries missing from the settings schema")
	}
}
