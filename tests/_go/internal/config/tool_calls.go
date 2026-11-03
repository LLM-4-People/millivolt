package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format"
)

func TestToolCallSettingsBoundsAndPersistence(t *testing.T) {
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
	if err != nil || got.RequestToolCallsMax != defaults.RequestToolCallsMax || got.StreamToolCallsMax != defaults.StreamToolCallsMax {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	for _, field := range []struct {
		key      string
		min, max int
	}{
		{"request_tool_calls_max", RequestToolCallsMin, RequestToolCallsBandMax},
		{"stream_tool_calls_max", StreamToolCallsMin, StreamToolCallsBandMax},
	} {
		for _, invalid := range []string{"-1", "0", fmt.Sprint(field.max + 1), "4096.5", "null", "bad"} {
			got, err := load(field.key + ": " + invalid)
			if err != nil {
				t.Errorf("%s=%s: %v", field.key, invalid, err)
			} else if !reflect.DeepEqual(got.Map()[field.key], defaults.Map()[field.key]) {
				t.Errorf("applied invalid %s=%s", field.key, invalid)
			}
		}
		for _, valid := range []string{fmt.Sprint(field.min), fmt.Sprint(field.max)} {
			if _, err := load(field.key + ": " + valid); err != nil {
				t.Errorf("boundary %s=%s: %v", field.key, valid, err)
			}
		}
	}
	// The rejection wording is a pinned contract: the YAML key, the inclusive
	// band, and the offending value, with no formatting drift.
	for _, tc := range []struct {
		edit func(*Config)
		want string
	}{
		{func(c *Config) { c.RequestToolCallsMax = 0 }, "request_tool_calls_max: must be 1..100000, got 0"},
		{func(c *Config) { c.RequestToolCallsMax = RequestToolCallsBandMax + 1 }, "request_tool_calls_max: must be 1..100000, got 100001"},
		{func(c *Config) { c.StreamToolCallsMax = 0 }, "stream_tool_calls_max: must be 1..65536, got 0"},
		{func(c *Config) { c.StreamToolCallsMax = StreamToolCallsBandMax + 1 }, "stream_tool_calls_max: must be 1..65536, got 65537"},
	} {
		c := Default()
		tc.edit(c)
		if err := c.Validate(); err == nil || err.Error() != tc.want {
			t.Errorf("Validate error = %v, want %q", err, tc.want)
		}
	}
	// A written value round-trips through the generated YAML.
	c := Default()
	c.RequestToolCallsMax = 2048
	c.StreamToolCallsMax = 4096
	var buf strings.Builder
	if err := WriteYAML(&buf, c); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(buf.String()), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestToolCallsMax != 2048 || got.StreamToolCallsMax != 4096 {
		t.Fatalf("round trip lost tool-call bounds: %+v", got)
	}
}

func TestToolCallSettingsSchema(t *testing.T) {
	for _, tc := range []struct {
		key      string
		category string
		min, max int
		help     string
	}{
		{"request_tool_calls_max", "request", RequestToolCallsMin, RequestToolCallsBandMax, "whole message history"},
		{"stream_tool_calls_max", "format", StreamToolCallsMin, StreamToolCallsBandMax, "translated response stream"},
	} {
		f := FieldByKey(tc.key)
		if f == nil {
			t.Fatalf("%s missing from Schema()", tc.key)
		}
		if f.Category != tc.category || f.Kind != KindInt || !f.HotReload {
			t.Errorf("%s schema = %+v", tc.key, f)
		}
		if f.Min == nil || *f.Min != float64(tc.min) || f.Max == nil || *f.Max != float64(tc.max) {
			t.Errorf("%s band = %v..%v, want %d..%d", tc.key, f.Min, f.Max, tc.min, tc.max)
		}
		if !strings.Contains(f.Help, tc.help) {
			t.Errorf("%s help does not explain what it bounds: %q", tc.key, f.Help)
		}
	}
	// The direct-library codec defaults are the same documented values, so a
	// caller that bypasses the proxy never silently accepts more than the
	// default configuration. Default() references those constants directly;
	// these assertions keep a future decoupling from drifting.
	if got, want := Default().RequestToolCallsMax, format.WireToolCallsMax; got != want {
		t.Fatalf("request_tool_calls_max default = %d, codec default = %d", got, want)
	}
	if got, want := Default().StreamToolCallsMax, format.StreamToolCallsMax; got != want {
		t.Fatalf("stream_tool_calls_max default = %d, codec default = %d", got, want)
	}
}
