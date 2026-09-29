package mcp

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

// env builds an environment lookup over a fixed map, so a developer's own
// MILLIVOLT_* values cannot influence a test result.
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
		o, err := parse("millivolt-mcp", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", token}, env(nil), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defaults := DefaultLimits()
		if o.limits != defaults {
			t.Fatalf("limits = %+v, want the built-in defaults %+v", o.limits, defaults)
		}
		if o.proxyURL != "http://127.0.0.1:8081" || o.operatorToken != token {
			t.Fatalf("options = %+v", o)
		}
	})

	t.Run("environment supplies both", func(t *testing.T) {
		o, err := parse("millivolt-mcp", nil, env(map[string]string{
			"MILLIVOLT_MCP_PROXY_URL":         "http://127.0.0.1:9090",
			"MILLIVOLT_MCP_OPERATOR_TOKEN":    token,
			"MILLIVOLT_MCP_QUERY_MAX_ROWS":    "42",
			"MILLIVOLT_MCP_PAGE_SIZE":         "7",
			"MILLIVOLT_MCP_CAPTURE_MAX_BYTES": "4096",
			"MILLIVOLT_MCP_QUERY_TIMEOUT":     "90s",
			"MILLIVOLT_MCP_TIMEOUT":           "5s",
		}), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if o.proxyURL != "http://127.0.0.1:9090" || o.operatorToken != token {
			t.Fatalf("options = %+v", o)
		}
		if o.limits.QueryMaxRows != 42 || o.limits.PageSize != 7 || o.limits.CaptureBytes != 4096 ||
			o.limits.QueryTimeout != 90*time.Second || o.limits.Timeout != 5*time.Second {
			t.Fatalf("limits = %+v", o.limits)
		}
	})

	t.Run("flags win over the environment", func(t *testing.T) {
		o, err := parse("millivolt-mcp", []string{
			"--proxy-url", "http://127.0.0.1:8082", "--operator-token", token,
			"--query-max-rows", "5", "--page-size", "6", "--query-timeout", "1m", "--timeout", "2m",
		}, env(map[string]string{
			"MILLIVOLT_MCP_PROXY_URL":      "http://127.0.0.1:9090",
			"MILLIVOLT_MCP_OPERATOR_TOKEN": "environment-credential-value",
			"MILLIVOLT_MCP_QUERY_MAX_ROWS": "42",
			"MILLIVOLT_MCP_PAGE_SIZE":      "7",
			"MILLIVOLT_MCP_QUERY_TIMEOUT":  "90s",
			"MILLIVOLT_MCP_TIMEOUT":        "5s",
		}), io.Discard)
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
		o, err := parse("millivolt-mcp", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", token},
			env(map[string]string{
				"MILLIVOLT_MCP_PROXY_URL":      "",
				"MILLIVOLT_MCP_OPERATOR_TOKEN": "",
				"MILLIVOLT_MCP_QUERY_MAX_ROWS": "",
			}), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if o.proxyURL != "http://127.0.0.1:8081" || o.limits.QueryMaxRows != DefaultLimits().QueryMaxRows {
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
			_, err := parse("millivolt-mcp", nil, env(map[string]string{tc.variable: tc.value}), io.Discard)
			if err == nil {
				t.Fatalf("a malformed %s override must fail setup", tc.variable)
			}
			if !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("the error must name the offending variable: %v", err)
			}
		}
	})

	t.Run("unknown flags and stray arguments are refused", func(t *testing.T) {
		if _, err := parse("millivolt-mcp", []string{"--nope"}, env(nil), io.Discard); err == nil {
			t.Fatal("an unknown flag must be refused")
		}
		if _, err := parse("millivolt-mcp", []string{"serve"}, env(nil), io.Discard); err == nil ||
			!strings.Contains(err.Error(), "unexpected argument") {
			t.Fatalf("error = %v, want the stray-argument refusal", err)
		}
	})
}

// TestProgramNameOwnsUsageAndDiagnostics pins the invocation parameterization:
// the flag set and the diagnostic prefix carry the real command line, so
// millivolt-mcp and `millivolt mcp` each report themselves. The usage text is
// what an MCP host may never show, and it must still reach the caller's
// diagnostic stream, never stdout.
func TestProgramNameOwnsUsageAndDiagnostics(t *testing.T) {
	var usage strings.Builder
	if _, err := parse("millivolt mcp", []string{"-h"}, env(nil), &usage); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parse(-h) error = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(usage.String(), "Usage of millivolt mcp:") {
		t.Fatalf("usage must name the real invocation, got:\n%s", usage.String())
	}

	var stderr strings.Builder
	code := Run("millivolt mcp", []string{"--operator-token", "operator-credential-value"}, env(nil), &stderr)
	if code == 0 {
		t.Fatal("a missing proxy URL must fail setup")
	}
	if !strings.Contains(stderr.String(), "millivolt mcp: setup:") {
		t.Fatalf("diagnostics must be prefixed with the real invocation, got:\n%s", stderr.String())
	}
}

// TestRunFailsFastOnBadSetup pins that an unusable setup exits before any
// transport exists, and that the diagnostic never contains the credential.
func TestRunFailsFastOnBadSetup(t *testing.T) {
	const token = "operator-credential-value"

	for _, tc := range []struct {
		name   string
		args   []string
		lookup func(string) (string, bool)
		want   string
		// secret is the credential this row puts in scope, so the no-leak
		// assertion is live on every row.
		secret string
	}{
		// The named empty flag blocks the environment value, so the required
		// refusal fires while a credential is still in scope.
		{"empty token flag", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token="}, env(map[string]string{"MILLIVOLT_MCP_OPERATOR_TOKEN": token}), "operator token is required", token},
		{"no url", []string{"--operator-token", token}, env(nil), "proxy URL is required", token},
		{"bad url", []string{"--proxy-url", "ftp://127.0.0.1", "--operator-token", token}, env(nil), "http or https", token},
		{"short token", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", shortToken}, env(nil), "at least 16", shortToken},
		{"bad limits", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", token, "--page-size", "0"}, env(nil), "page size", token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.secret == "" {
				t.Fatal("every bad-setup row must name the credential it supplies")
			}
			var stderr strings.Builder
			code := Run("millivolt-mcp", tc.args, tc.lookup, &stderr)
			if code == 0 {
				t.Fatal("bad setup must exit nonzero")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("diagnostic %q must mention %q", stderr.String(), tc.want)
			}
			if strings.Contains(stderr.String(), tc.secret) {
				t.Fatalf("the credential must never be printed: %q", stderr.String())
			}
		})
	}
}

// TestParseResolvesTheCredentialAfterFlagParsing pins the resolution rule the
// usage guard depends on: the credential is not a flag default, and flags still
// win over the environment by what was NAMED, not by whether the result is
// empty.
func TestParseResolvesTheCredentialAfterFlagParsing(t *testing.T) {
	env := func(name string) (string, bool) {
		if name == "MILLIVOLT_MCP_OPERATOR_TOKEN" {
			return "environment-credential-value", true
		}
		return "", false
	}
	// Absent flag: the environment supplies it.
	o, err := parse("millivolt-mcp", []string{"--proxy-url", "http://127.0.0.1:8081"}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "environment-credential-value" {
		t.Fatalf("an absent flag must fall through to the environment, got %q", o.operatorToken)
	}
	// A named flag wins.
	o, err = parse("millivolt-mcp", []string{"--operator-token", "flag-credential-value"}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "flag-credential-value" {
		t.Fatalf("a named flag must win, got %q", o.operatorToken)
	}
	// A named but empty flag is a deliberate invalid choice, not a fall-through:
	// falling through would silently use a different credential than asked.
	o, err = parse("millivolt-mcp", []string{"--operator-token="}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "" {
		t.Fatalf("an explicit empty flag must not fall through to the environment, got %q", o.operatorToken)
	}
	// An empty environment value counts as unset.
	empty := func(name string) (string, bool) {
		if name == "MILLIVOLT_MCP_OPERATOR_TOKEN" {
			return "", true
		}
		return "", false
	}
	o, err = parse("millivolt-mcp", nil, empty, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "" {
		t.Fatalf("an empty environment value must count as unset, got %q", o.operatorToken)
	}
}

// The credential these cases look for is a fixture value for a setup path that
// is about to refuse; it grants access to nothing.
const leakingToken = "mcp-setup-output-credential-fixture"

// shortToken is below the operator band. A length refusal must name the band,
// never the value.
const shortToken = "operator-cred"

// TestSetupErrorsNeverCarryTheCredential pins that the setup path's own
// messages stay safe: they must name what to do, never the value.
func TestSetupErrorsNeverCarryTheCredential(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		env    map[string]string
		want   string
		secret string
	}{
		{"empty token flag", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token="},
			map[string]string{"MILLIVOLT_MCP_OPERATOR_TOKEN": leakingToken}, "operator token is required", leakingToken},
		{"short token", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", shortToken}, nil, "at least 16", shortToken},
		{"no url", []string{"--operator-token", leakingToken}, nil, "proxy URL is required", leakingToken},
		{"bad url", []string{"--proxy-url", "ftp://x", "--operator-token", leakingToken}, nil, "http or https", leakingToken},
		{
			"bad env integer", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", leakingToken},
			map[string]string{"MILLIVOLT_MCP_QUERY_MAX_ROWS": "many"}, "MILLIVOLT_MCP_QUERY_MAX_ROWS must be an integer", leakingToken,
		},
		{
			"bad env duration", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", leakingToken},
			map[string]string{"MILLIVOLT_MCP_QUERY_TIMEOUT": "soon"}, "MILLIVOLT_MCP_QUERY_TIMEOUT must be a duration", leakingToken,
		},
		{
			"refused limits", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", leakingToken, "--page-size", "0"},
			nil, "page size must be at least 1", leakingToken,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.secret == "" {
				t.Fatal("every setup-error case must name the credential it supplies")
			}
			lookup := func(name string) (string, bool) {
				value, ok := tc.env[name]
				return value, ok
			}
			o, err := parse("millivolt-mcp", tc.args, lookup, io.Discard)
			if err == nil {
				// parse succeeded; the refusal must come from construction, which
				// is the same setup check Run performs.
				_, err = NewService(o.proxyURL, o.operatorToken, o.limits)
			}
			if err == nil {
				t.Fatalf("%s must be a setup failure", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("a setup error carried the credential: %q", err)
			}
		})
	}
}
