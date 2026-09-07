package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func loadStormConfig(t *testing.T, raw string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return LoadFile(path)
}

func TestStormConfigBoundsAndSchema(t *testing.T) {
	category := false
	for _, c := range Categories() {
		if c.ID == "storm" && c.Label == "Error storm protection" {
			category = true
		}
	}
	if !category {
		t.Fatal("missing error storm settings category")
	}
	for _, field := range []struct {
		key      string
		min, max int64
	}{
		{"storm_min_samples", 1, 1_000_000},
		{"storm_error_percent", 1, 100},
		{"storm_backoff_multiplier", 2, 10},
		{"storm_jitter_percent", 0, 100},
		{"storm_recovery_successes", 1, 100},
		{"storm_max_queue", 1, 100000},
		{"storm_max_scopes", 2, 100000},
		{"storm_max_retries", 0, 1000},
	} {
		t.Run(field.key, func(t *testing.T) {
			schema := FieldByKey(field.key)
			if schema == nil || schema.Category != "storm" || schema.Kind != KindInt || !schema.HotReload || schema.Min == nil || schema.Max == nil || *schema.Min != float64(field.min) || *schema.Max != float64(field.max) {
				t.Fatalf("schema does not expose the allowed bounds: %+v", schema)
			}
			for _, invalid := range []string{fmt.Sprint(field.min - 1), fmt.Sprint(field.max + 1), "2.5", "2.0", "null", "true", "\"2\"", "bad"} {
				c, err := loadStormConfig(t, field.key+": "+invalid)
				if err != nil {
					t.Errorf("YAML %s: %v", invalid, err)
				} else if !reflect.DeepEqual(c.Map()[field.key], Default().Map()[field.key]) {
					t.Errorf("applied invalid YAML %s", invalid)
				}
			}
			for _, valid := range []int64{field.min, field.max} {
				if _, err := loadStormConfig(t, fmt.Sprintf("%s: %d", field.key, valid)); err != nil {
					t.Errorf("boundary %d: %v", valid, err)
				}
			}
			for _, invalid := range []int64{field.min - 1, field.max + 1} {
				c := Default()
				c.fieldRV(field.key).SetInt(invalid)
				if err := c.Validate(); err == nil {
					t.Errorf("Validate accepted %d", invalid)
				}
			}
		})
	}
	for _, field := range []struct {
		key      string
		min, max time.Duration
	}{
		{"storm_window", time.Second, time.Hour},
		{"storm_initial_backoff", time.Millisecond, time.Hour},
		{"storm_max_backoff", time.Millisecond, time.Hour},
		{"storm_max_wait", time.Millisecond, 24 * time.Hour},
	} {
		t.Run(field.key, func(t *testing.T) {
			schema := FieldByKey(field.key)
			if schema == nil || schema.Category != "storm" || schema.Kind != KindDuration || !schema.HotReload || schema.Min == nil || schema.Max == nil || *schema.Min != float64(field.min) || *schema.Max != float64(field.max) {
				t.Fatalf("schema does not expose the duration bounds: %+v", schema)
			}
			for _, invalid := range []string{"0", "0s", "null", "false", "bad", FormatDuration(field.min - 1), FormatDuration(field.max + 1)} {
				c, err := loadStormConfig(t, field.key+": "+invalid)
				if err != nil {
					t.Errorf("YAML %s: %v", invalid, err)
				} else if !reflect.DeepEqual(c.Map()[field.key], Default().Map()[field.key]) {
					t.Errorf("applied invalid YAML %s", invalid)
				}
			}
			for _, valid := range []time.Duration{field.min, field.max} {
				raw := fmt.Sprintf("%s: %s\n", field.key, FormatDuration(valid))
				if field.key == "storm_initial_backoff" {
					raw += "storm_max_backoff: 1h\n"
				} else if field.key == "storm_max_backoff" {
					raw += "storm_initial_backoff: 1ms\n"
				}
				got, err := loadStormConfig(t, raw)
				if err != nil {
					t.Errorf("boundary %s: %v", valid, err)
				} else if !reflect.DeepEqual(got.fieldValue(field.key), valid) {
					t.Errorf("boundary %s: got %v", valid, got.fieldValue(field.key))
				}
			}
			for _, invalid := range []time.Duration{field.min - 1, field.max + 1} {
				c := Default()
				c.fieldRV(field.key).SetInt(int64(invalid))
				if err := c.Validate(); err == nil {
					t.Errorf("Validate accepted %s", invalid)
				}
			}
		})
	}
	c, err := loadStormConfig(t, "storm_initial_backoff: 1m\nstorm_max_backoff: 59s")
	if err != nil {
		t.Fatal(err)
	}
	if c.StormInitialBackoff == time.Minute && c.StormMaxBackoff == 59*time.Second {
		t.Error("applied initial backoff greater than maximum")
	}
	c, err = loadStormConfig(t, "storm_max_retry: 20")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Map(), Default().Map()) {
		t.Error("unknown storm setting applied")
	}
}

func TestStormConfigStrictFlagsAndStatuses(t *testing.T) {
	for _, key := range []string{"storm_enabled", "storm_provider_enabled", "storm_model_enabled", "storm_banner_enabled", "storm_transport_errors", "storm_stream_errors"} {
		t.Run(key, func(t *testing.T) {
			for _, invalid := range []string{"null", "yes", "no", "\"true\"", "\"false\"", "0", "[]"} {
				c, err := loadStormConfig(t, key+": "+invalid)
				if err != nil {
					t.Errorf("YAML %s: %v", invalid, err)
				} else if !reflect.DeepEqual(c.Map()[key], Default().Map()[key]) {
					t.Errorf("applied invalid YAML %s", invalid)
				}
			}
			for _, valid := range []bool{false, true} {
				got, err := loadStormConfig(t, fmt.Sprintf("%s: %t", key, valid))
				if err != nil || got.fieldValue(key) != valid {
					t.Errorf("explicit %t did not survive: %v", valid, err)
				}
			}
		})
	}
	for _, invalid := range []string{"null", "[500]", "[true]", "[null]", "500", "\"500\"", "[\"429\", \"429\"]", "[\"499\"]", "[\"600\"]", "[\"0500\"]", "[\"5xx\"]", "[\"500 \"]", "[\" 500\"]", "[\"\"]"} {
		c, err := loadStormConfig(t, "storm_status_codes: "+invalid)
		if err != nil {
			t.Errorf("status YAML %s: %v", invalid, err)
		} else if !reflect.DeepEqual(c.StormStatusCodes, Default().StormStatusCodes) {
			t.Errorf("applied invalid status YAML %s", invalid)
		}
	}
	for _, valid := range []string{"[]", "[\"429\", \"500\", \"599\"]"} {
		if _, err := loadStormConfig(t, "storm_status_codes: "+valid); err != nil {
			t.Errorf("valid statuses %s: %v", valid, err)
		}
	}
	for _, invalid := range [][]string{{"429", "429"}, {"401"}, {"600"}, {"+500"}, {"500\n"}, {""}} {
		c := Default()
		if err := c.Apply(map[string]any{"storm_status_codes": invalid}); err == nil && c.Validate() == nil {
			t.Errorf("Settings accepted invalid statuses %q", invalid)
		}
	}
	if err := Default().Apply(map[string]any{"storm_status_codes": nil}); err == nil {
		t.Error("Settings null silently disabled HTTP storm triggers")
	}
}

func TestStormConfigRoundtripAndIsolation(t *testing.T) {
	defaults := Default()
	if defaults.fieldValue("storm_enabled") != false {
		t.Fatal("storm protection must require opt-in")
	}
	if !reflect.DeepEqual(defaults.fieldValue("storm_status_codes"), []string{"500", "502", "503", "504"}) {
		t.Fatal("default storm triggers must exclude credential-specific 429")
	}
	changed := Default()
	updates := map[string]any{
		"storm_enabled": true, "storm_provider_enabled": false, "storm_model_enabled": false,
		"storm_banner_enabled": false, "storm_transport_errors": false, "storm_stream_errors": false, "storm_window": "7s",
		"storm_min_samples": 6, "storm_error_percent": 77,
		"storm_initial_backoff": "17ms", "storm_max_backoff": "3m", "storm_backoff_multiplier": 3,
		"storm_jitter_percent": 0, "storm_recovery_successes": 7, "storm_max_queue": 14,
		"storm_max_wait": "19s", "storm_max_scopes": 123, "storm_max_retries": 0,
		"storm_status_codes": []string{},
	}
	if err := changed.Apply(updates); err != nil {
		t.Fatal(err)
	}
	if err := changed.Validate(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WriteYAML(&out, changed); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadStormConfig(t, out.String())
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range updates {
		f := FieldByKey(key)
		if f == nil || f.Category != "storm" || !f.HotReload {
			t.Errorf("field missing from hot-reload storm settings: %s", key)
		}
		if got := loaded.Map()[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s roundtrip = %#v, want %#v", key, got, want)
		}
	}
	if changes := StartupBoundChanges(defaults, changed); len(changes) != 0 {
		t.Errorf("storm changes incorrectly require restart: %v", changes)
	}
	if !strings.Contains(FieldByKey("storm_max_wait").Help, "each") || !strings.Contains(FieldByKey("storm_min_samples").Help, "attempts") {
		t.Error("settings must explain wait and sample accounting scope")
	}
	for _, term := range []string{"all active models", "same detection window", "idle models expire"} {
		if !strings.Contains(FieldByKey("storm_provider_enabled").Help, term) {
			t.Errorf("provider threshold help omits %q", term)
		}
	}
	for _, removed := range []string{"storm_provider_model_percent", "storm_provider_min_models"} {
		if FieldByKey(removed) != nil {
			t.Errorf("provider-wide detection must not expose %s", removed)
		}
	}
	clone := defaults.Clone()
	clone.fieldValue("storm_status_codes").([]string)[0] = "599"
	if defaults.fieldValue("storm_status_codes").([]string)[0] != "500" {
		t.Error("Clone shares mutable storm status codes")
	}
	defaults.fieldValue("storm_status_codes").([]string)[0] = "598"
	if Default().fieldValue("storm_status_codes").([]string)[0] != "500" {
		t.Error("Default shares mutable storm status codes")
	}
}
