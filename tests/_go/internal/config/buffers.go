package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestUpstreamBufferSettingsBoundsAndSchema(t *testing.T) {
	load := func(raw string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		return LoadFile(path)
	}
	defaults := Default()
	for _, tc := range []struct {
		key      string
		min, max int64
		hot      bool
		field    func(*Config) int64
		set      func(*Config, int64)
	}{
		{"upstream_response_header_max", UpstreamResponseHeaderMaxMin, UpstreamResponseHeaderMaxMax, false,
			func(c *Config) int64 { return int64(c.UpstreamResponseHeaderMax) },
			func(c *Config, v int64) { c.UpstreamResponseHeaderMax = ByteSize(v) }},
		{"non_stream_capture_max", NonStreamCaptureMaxMin, NonStreamCaptureMaxMax, true,
			func(c *Config) int64 { return int64(c.NonStreamCaptureMax) },
			func(c *Config, v int64) { c.NonStreamCaptureMax = ByteSize(v) }},
		{"stream_terminal_hold_max", StreamTerminalHoldMaxMin, StreamTerminalHoldMaxMax, true,
			func(c *Config) int64 { return int64(c.StreamTerminalHoldMax) },
			func(c *Config, v int64) { c.StreamTerminalHoldMax = ByteSize(v) }},
		{"token_response_max", TokenResponseMaxMin, TokenResponseMaxMax, true,
			func(c *Config) int64 { return int64(c.TokenResponseMaxBytes) },
			func(c *Config, v int64) { c.TokenResponseMaxBytes = ByteSize(v) }},
		{"stream_event_line_max", StreamEventLineMaxMin, StreamEventLineMaxMax, true,
			func(c *Config) int64 { return int64(c.StreamEventLineMax) },
			func(c *Config, v int64) { c.StreamEventLineMax = ByteSize(v) }},
	} {
		got, err := load("{}")
		if err != nil || tc.field(got) != tc.field(defaults) {
			t.Errorf("%s default: %+v %v", tc.key, got, err)
		}
		for _, invalid := range []string{"0", "-1", fmt.Sprint(tc.max + 1), "bad", "null"} {
			got, err := load(tc.key + ": " + invalid)
			if err != nil {
				t.Errorf("%s=%s: %v", tc.key, invalid, err)
			} else if tc.field(got) != tc.field(defaults) {
				t.Errorf("applied invalid %s=%s", tc.key, invalid)
			}
		}
		for _, valid := range []int64{tc.min, tc.max} {
			got, err := load(fmt.Sprintf("%s: %d", tc.key, valid))
			if err != nil || tc.field(got) != valid {
				t.Errorf("%s=%d: %+v %v", tc.key, valid, got, err)
			}
		}
		f := FieldByKey(tc.key)
		if f == nil || f.Kind != KindBytes || f.HotReload != tc.hot {
			t.Errorf("%s schema = %+v", tc.key, f)
		}
		c := Default()
		tc.set(c, tc.max+1)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an over-band value", tc.key)
		}
	}
}
