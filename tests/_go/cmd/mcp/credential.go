package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// The credential these tests look for is a fixture value for a binary that is
// about to be refused or to print usage; it grants access to nothing.
const leakingToken = "mcp-usage-output-credential-fixture"

// TestCredentialNeverReachesFlagOutput RUNS the built binary and checks both of
// its streams. Testing parse() in-process is not enough, and was the reason this
// defect survived: flag.NewFlagSet's Output() defaults to os.Stderr, so a
// harness that captured only the function's return value never saw the print.
// flag.PrintDefaults runs on -h, on --help and on every flag parse error, and it
// renders `(default "...")` for any flag whose default is not its zero value, so
// a credential used as a flag default is printed in plaintext.
func TestCredentialNeverReachesFlagOutput(t *testing.T) {
	binary := buildBinary(t)

	// Each argument form reaches flag.PrintDefaults by a different route: an
	// explicit help request, a parse error, and a bad value for another flag.
	// The credential is supplied two ways, because a flag default taken from the
	// environment leaks the same bytes.
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{"env supplied, -h", []string{"-h"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken}},
		{"env supplied, --help", []string{"--help"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken}},
		{"env supplied, unknown flag", []string{"--bogus"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken}},
		{"env supplied, bad flag value", []string{"--timeout", "nope"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken}},
		{"env supplied, no arguments", nil, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken}},
		{"flag supplied, -h", []string{"-h", "--operator-token", leakingToken}, nil},
		{"flag supplied, --help", []string{"--help", "--operator-token", leakingToken}, nil},
		{"flag supplied, unknown flag", []string{"--bogus", "--operator-token", leakingToken}, nil},
		{"flag supplied, bad flag value", []string{"--timeout", "nope", "--operator-token", leakingToken}, nil},
		{"flag supplied, positional argument", []string{"stray", "--operator-token", leakingToken}, nil},
		// The other setup flags must not become a channel for it either.
		{"operator token before help", []string{"--operator-token", leakingToken, "-h"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(binary, tc.args...)
			command.Env = environmentWithoutCredentials(tc.env)
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			// The server must not be reached: every case here fails or prints
			// usage before any transport exists.
			_ = command.Run()
			for stream, text := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
				if strings.Contains(text, leakingToken) {
					t.Fatalf("the operator credential reached %s in plaintext:\n%s", stream, text)
				}
			}
		})
	}
	// The usage text is still printed, and it must still name the flag: a guard
	// that silenced the whole output would hide the setup surface.
	command := exec.Command(binary, "-h")
	command.Env = environmentWithoutCredentials([]string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken})
	var stderr bytes.Buffer
	command.Stderr = &stderr
	_ = command.Run()
	usage := stderr.String()
	for _, flag := range []string{"-operator-token", "-proxy-url", "-query-max-rows", "-page-size", "MILLIVOLT_MCP_QUERY_MAX_BYTES"} {
		if !strings.Contains(usage, flag) {
			t.Fatalf("usage must still document %q, got:\n%s", flag, usage)
		}
	}
	if strings.Contains(usage, "default \"") && strings.Contains(usage, "MILLIVOLT_MCP_PROXY_URL=") {
		// A non-secret default may still be rendered; only the credential's
		// absence is load-bearing, and the map above already proved it.
		t.Logf("usage renders a non-secret default: %s", usage)
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
	o, err := parse([]string{"--proxy-url", "http://127.0.0.1:8081"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "environment-credential-value" {
		t.Fatalf("an absent flag must fall through to the environment, got %q", o.operatorToken)
	}
	// A named flag wins.
	o, err = parse([]string{"--operator-token", "flag-credential-value"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "flag-credential-value" {
		t.Fatalf("a named flag must win, got %q", o.operatorToken)
	}
	// A named but empty flag is a deliberate invalid choice, not a fall-through:
	// falling through would silently use a different credential than asked.
	o, err = parse([]string{"--operator-token="}, env)
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
	o, err = parse(nil, empty)
	if err != nil {
		t.Fatal(err)
	}
	if o.operatorToken != "" {
		t.Fatalf("an empty environment value must count as unset, got %q", o.operatorToken)
	}
}

// TestSetupErrorsNeverCarryTheCredential pins that the setup path's own
// messages stay safe: they must name what to do, never the value.
func TestSetupErrorsNeverCarryTheCredential(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no token", []string{"--proxy-url", "http://127.0.0.1:8081"}, nil, "operator token is required"},
		{"short token", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", "short"}, nil, "at least 16"},
		{"no url", []string{"--operator-token", leakingToken}, nil, "proxy URL is required"},
		{"bad url", []string{"--proxy-url", "ftp://x", "--operator-token", leakingToken}, nil, "http or https"},
		{
			"bad env integer", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", leakingToken},
			map[string]string{"MILLIVOLT_MCP_QUERY_MAX_ROWS": "many"}, "MILLIVOLT_MCP_QUERY_MAX_ROWS must be an integer",
		},
		{
			"bad env duration", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", leakingToken},
			map[string]string{"MILLIVOLT_MCP_QUERY_TIMEOUT": "soon"}, "MILLIVOLT_MCP_QUERY_TIMEOUT must be a duration",
		},
		{
			"refused limits", []string{"--proxy-url", "http://127.0.0.1:8081", "--operator-token", leakingToken, "--page-size", "0"},
			nil, "page size must be at least 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				value, ok := tc.env[name]
				return value, ok
			}
			o, err := parse(tc.args, lookup)
			if err == nil {
				// parse succeeded; the refusal must come from construction, which
				// is the same setup check main() runs.
				_, err = mcp.NewService(o.proxyURL, o.operatorToken, o.limits)
			}
			if err == nil {
				t.Fatalf("%s must be a setup failure", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), leakingToken) {
				t.Fatalf("a setup error carried the credential: %q", err)
			}
		})
	}
}

// environmentWithoutCredentials builds a clean environment carrying only the
// supplied overrides, so a developer's own MILLIVOLT_* values cannot influence
// the result and leak into the assertion.
func environmentWithoutCredentials(overrides []string) []string {
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOTOOLCHAIN=local"}
	return append(environment, overrides...)
}

// buildBinary compiles the command under test once per run and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "millivolt-mcp")
	build := exec.Command("go", "build", "-o", binary, "./cmd/mcp")
	build.Dir = repositoryRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/mcp: %v\n%s", err, output)
	}
	return binary
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "scripts", "dev.sh")); err != nil {
		t.Fatalf("cannot locate the repository root from the test working directory: %v", err)
	}
	return root
}
