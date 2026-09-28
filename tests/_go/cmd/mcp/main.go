package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// TestFlagAndEnvironmentSetup pins the setup surface: flags win over the
// environment, an empty environment value is unset, and a malformed override
// fails setup rather than silently defaulting.
func TestFlagAndEnvironmentSetup(t *testing.T) {
	const token = "operator-credential-value"

	t.Run("defaults", func(t *testing.T) {
		o, err := parse([]string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", token}, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		defaults := mcp.DefaultLimits()
		if o.limits != defaults {
			t.Fatalf("limits = %+v, want the built-in defaults %+v", o.limits, defaults)
		}
		if o.proxyURL != "http://127.0.0.1:8081" || o.operatorToken != token {
			t.Fatalf("options = %+v", o)
		}
	})

	t.Run("environment supplies both", func(t *testing.T) {
		o, err := parse(nil, env(map[string]string{
			"MILLIVOLT_MCP_PROXY_URL":      "http://127.0.0.1:9090",
			"MILLIVOLT_MCP_OPERATOR_TOKEN": token,
			"MILLIVOLT_MCP_QUERY_MAX_ROWS": "42",
			"MILLIVOLT_MCP_PAGE_SIZE":      "7",
			"MILLIVOLT_MCP_QUERY_TIMEOUT":  "90s",
			"MILLIVOLT_MCP_TIMEOUT":        "5s",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if o.proxyURL != "http://127.0.0.1:9090" || o.operatorToken != token {
			t.Fatalf("options = %+v", o)
		}
		if o.limits.QueryMaxRows != 42 || o.limits.PageSize != 7 ||
			o.limits.QueryTimeout != 90*time.Second || o.limits.Timeout != 5*time.Second {
			t.Fatalf("limits = %+v", o.limits)
		}
	})

	t.Run("flags win over the environment", func(t *testing.T) {
		o, err := parse([]string{
			"--proxy-url", "http://127.0.0.1:8082", "--operator-token", token,
			"--query-max-rows", "5", "--page-size", "6", "--query-timeout", "1m", "--timeout", "2m",
		}, env(map[string]string{
			"MILLIVOLT_MCP_PROXY_URL":      "http://127.0.0.1:9090",
			"MILLIVOLT_MCP_OPERATOR_TOKEN": "environment-credential-value",
			"MILLIVOLT_MCP_QUERY_MAX_ROWS": "42",
			"MILLIVOLT_MCP_PAGE_SIZE":      "7",
			"MILLIVOLT_MCP_QUERY_TIMEOUT":  "90s",
			"MILLIVOLT_MCP_TIMEOUT":        "5s",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if o.proxyURL != "http://127.0.0.1:8082" || o.operatorToken != token {
			t.Fatalf("flags must win: %+v", o)
		}
		if o.limits.QueryMaxRows != 5 || o.limits.PageSize != 6 ||
			o.limits.QueryTimeout != time.Minute || o.limits.Timeout != 2*time.Minute {
			t.Fatalf("limits = %+v", o.limits)
		}
	})

	t.Run("empty environment values are unset", func(t *testing.T) {
		o, err := parse([]string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", token},
			env(map[string]string{
				"MILLIVOLT_MCP_PROXY_URL":      "",
				"MILLIVOLT_MCP_OPERATOR_TOKEN": "",
				"MILLIVOLT_MCP_QUERY_MAX_ROWS": "",
			}))
		if err != nil {
			t.Fatal(err)
		}
		if o.proxyURL != "http://127.0.0.1:8081" || o.limits.QueryMaxRows != mcp.DefaultLimits().QueryMaxRows {
			t.Fatalf("an empty override must fall through to the flag: %+v", o)
		}
	})

	t.Run("malformed overrides fail setup", func(t *testing.T) {
		for _, tc := range []struct {
			variable string
			value    string
		}{
			{"MILLIVOLT_MCP_QUERY_MAX_ROWS", "many"},
			{"MILLIVOLT_MCP_PAGE_SIZE", "10.5"},
			{"MILLIVOLT_MCP_QUERY_TIMEOUT", "soon"},
			{"MILLIVOLT_MCP_TIMEOUT", "30"},
		} {
			_, err := parse(nil, env(map[string]string{tc.variable: tc.value}))
			if err == nil {
				t.Fatalf("a malformed %s override must fail setup", tc.variable)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("the error must name the offending variable: %v", err)
			}
		}
	})

	t.Run("unknown flags and stray arguments are refused", func(t *testing.T) {
		if _, err := parse([]string{"--nope"}, env(nil)); err == nil {
			t.Fatal("an unknown flag must be refused")
		}
		if _, err := parse([]string{"serve"}, env(nil)); err == nil ||
			!strings.Contains(err.Error(), "unexpected argument") {
			t.Fatalf("error = %v, want the stray-argument refusal", err)
		}
	})
}

// TestRunFailsFastOnBadSetup pins that an unusable setup exits before any
// transport exists, and that the diagnostic never contains the credential.
func TestRunFailsFastOnBadSetup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		lookup func(string) (string, bool)
		want   string
	}{
		{"no token", []string{"--proxy-url", "http://127.0.0.1:8081"}, env(nil), "operator token is required"},
		{"no url", []string{"--operator-token", "operator-credential-value"}, env(nil), "proxy URL is required"},
		{"bad url", []string{"--proxy-url", "ftp://127.0.0.1", "--operator-token", "operator-credential-value"}, env(nil), "http or https"},
		{"short token", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", "short"}, env(nil), "at least 16"},
		{"bad limits", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", "operator-credential-value", "--page-size", "0"}, env(nil), "page size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			code := run(tc.args, tc.lookup, io.Writer(&stderr))
			if code == 0 {
				t.Fatal("bad setup must exit nonzero")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("diagnostic %q must mention %q", stderr.String(), tc.want)
			}
			if strings.Contains(stderr.String(), "operator-credential-value") {
				t.Fatalf("the credential must never be printed: %q", stderr.String())
			}
		})
	}
}
