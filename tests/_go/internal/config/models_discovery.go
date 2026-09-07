package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestModelsDiscoveryConfigBoundsAndPersistence(t *testing.T) {
	load := func(raw string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		return LoadFile(path)
	}
	defaults := Default()
	got, err := load("{}")
	if err != nil || got.ModelsDiscoveryTimeout != defaults.ModelsDiscoveryTimeout || got.ModelsDiscoveryMaxBytes != defaults.ModelsDiscoveryMaxBytes || got.ModelsDiscoveryMaxPages != defaults.ModelsDiscoveryMaxPages {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	for _, field := range []struct {
		key      string
		min, max int64
	}{
		{"models_discovery_max_bytes", ModelsDiscoveryMaxBytesMin, ModelsDiscoveryMaxBytesMax},
		{"models_discovery_max_pages", ModelsDiscoveryMaxPagesMin, ModelsDiscoveryMaxPagesMax},
	} {
		for _, invalid := range []string{"-1", "0", fmt.Sprint(field.min - 1), fmt.Sprint(field.max + 1), "4096.5", "4096.0", "null", "bad"} {
			got, err := load(field.key + ": " + invalid)
			if err != nil {
				t.Errorf("%s=%s: %v", field.key, invalid, err)
			} else if !reflect.DeepEqual(got.Map()[field.key], defaults.Map()[field.key]) {
				t.Errorf("applied invalid %s=%s", field.key, invalid)
			}
		}
		for _, valid := range []int64{field.min, field.max} {
			if _, err := load(fmt.Sprintf("%s: %d", field.key, valid)); err != nil {
				t.Errorf("boundary %s=%d: %v", field.key, valid, err)
			}
		}
	}
	for _, invalid := range []string{"0", "0s", "-1s", "999ms", "5m1s", "null", "bad"} {
		got, err := load("models_discovery_timeout: " + invalid)
		if err != nil {
			t.Errorf("timeout %s: %v", invalid, err)
		} else if got.ModelsDiscoveryTimeout != defaults.ModelsDiscoveryTimeout {
			t.Errorf("applied invalid timeout %s", invalid)
		}
	}
	for _, valid := range []string{FormatDuration(ModelsDiscoveryTimeoutMin), FormatDuration(ModelsDiscoveryTimeoutMax)} {
		if _, err := load("models_discovery_timeout: " + valid); err != nil {
			t.Errorf("timeout boundary %s: %v", valid, err)
		}
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.ModelsDiscoveryTimeout = ModelsDiscoveryTimeoutMin - 1 },
		func(c *Config) { c.ModelsDiscoveryMaxBytes = ModelsDiscoveryMaxBytesMax + 1 },
		func(c *Config) { c.ModelsDiscoveryMaxPages = ModelsDiscoveryMaxPagesMax + 1 },
	} {
		c := Default()
		edit(c)
		if err := c.Validate(); err == nil {
			t.Error("Validate accepted invalid discovery limit")
		}
	}
	changed := Default()
	changed.ModelsDiscoveryTimeout = ModelsDiscoveryTimeoutMin
	changed.ModelsDiscoveryMaxBytes = ModelsDiscoveryMaxBytesMin
	changed.ModelsDiscoveryMaxPages = ModelsDiscoveryMaxPagesMin
	var out bytes.Buffer
	if err := WriteYAML(&out, changed); err != nil {
		t.Fatal(err)
	}
	got, err = load(out.String())
	if err != nil || got.ModelsDiscoveryTimeout != changed.ModelsDiscoveryTimeout || got.ModelsDiscoveryMaxBytes != changed.ModelsDiscoveryMaxBytes || got.ModelsDiscoveryMaxPages != changed.ModelsDiscoveryMaxPages {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	n := 0
	for _, field := range Schema() {
		if strings.HasPrefix(field.Key, "models_discovery_") {
			n++
			if !field.HotReload {
				t.Errorf("%s should hot-reload for new requests", field.Key)
			}
		}
	}
	if n != 3 {
		t.Fatalf("schema discovery fields=%d", n)
	}
	got, err = load("models_discovery_max_byte: 8192")
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelsDiscoveryMaxBytes != defaults.ModelsDiscoveryMaxBytes {
		t.Fatal("unknown discovery setting applied")
	}
}
