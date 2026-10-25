package config

import (
	"strings"
	"testing"
	"time"
)

// The mcp category's whole band is enforced at the config boundary with the
// key named, so a Settings write or a YAML load can never arm the endpoint
// with an unusable policy. internal/mcp's Limits.Validate is the
// construction-time twin of the same band; this pins the config side.
func TestMCPSettingsValidateRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		key    string
	}{
		{"page size zero", func(c *Config) { c.MCPPageSize = 0 }, "mcp_page_size"},
		{"page size below floor", func(c *Config) { c.MCPPageSize = 9 }, "mcp_page_size"},
		{"page size above ceiling", func(c *Config) { c.MCPPageSize = 501 }, "mcp_page_size"},
		{"query rows zero", func(c *Config) { c.MCPQueryMaxRows = 0 }, "mcp_query_max_rows"},
		{"query rows above ceiling", func(c *Config) { c.MCPQueryMaxRows = 100001 }, "mcp_query_max_rows"},
		{"query bytes below floor", func(c *Config) { c.MCPQueryMaxBytes = 0 }, "mcp_query_max_bytes"},
		{"capture bytes below floor", func(c *Config) { c.MCPCaptureMaxBytes = 0 }, "mcp_capture_max_bytes"},
		{"query timeout zero", func(c *Config) { c.MCPQueryTimeout = 0 }, "mcp_query_timeout"},
		{"query timeout negative", func(c *Config) { c.MCPQueryTimeout = -time.Second }, "mcp_query_timeout"},
		{"request timeout zero", func(c *Config) { c.MCPTimeout = 0 }, "mcp_timeout"},
		{"request timeout negative", func(c *Config) { c.MCPTimeout = -time.Second }, "mcp_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want a rejection for %s", tc.name)
			}
			if name, _, ok := strings.Cut(err.Error(), ":"); !ok || name != tc.key {
				t.Fatalf("Validate() = %q, want the rejection to name %q", err, tc.key)
			}
		})
	}
}

// TestMCPBandBoundaries pins that both ends of the MCP bands are accepted at
// the config boundary, not only the floors the endpoint used to enforce.
func TestMCPBandBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"page size minimum", func(c *Config) { c.MCPPageSize = 10 }},
		{"page size maximum", func(c *Config) { c.MCPPageSize = 500 }},
		{"query rows minimum", func(c *Config) { c.MCPQueryMaxRows = 1 }},
		{"query rows maximum", func(c *Config) { c.MCPQueryMaxRows = 100000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want the boundary accepted", err)
			}
		})
	}
}

// TestMCPBandsInSchema pins the dashboard ranges to the same constants and the
// query field's help to its default-plus-explicit-ceiling contract.
func TestMCPBandsInSchema(t *testing.T) {
	for _, tc := range []struct {
		key      string
		min, max int
	}{
		{"mcp_page_size", 10, 500},
		{"mcp_query_max_rows", 1, 100000},
	} {
		field := FieldByKey(tc.key)
		if field == nil {
			t.Fatalf("%s is missing from Schema()", tc.key)
		}
		if field.Min == nil || int(*field.Min) != tc.min || field.Max == nil || int(*field.Max) != tc.max {
			t.Fatalf("%s range = %v..%v, want %d..%d", tc.key, field.Min, field.Max, tc.min, tc.max)
		}
	}
	help := FieldByKey("mcp_query_max_rows").Help
	for _, needle := range []string{"Default client-side row limit", "omits max_rows", "explicit max_rows"} {
		if !strings.Contains(help, needle) {
			t.Fatalf("mcp_query_max_rows help %q must state %q", help, needle)
		}
	}
}
