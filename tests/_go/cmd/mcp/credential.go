package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The credential these tests look for is a fixture value for a binary that is
// about to be refused or to print usage; it grants access to nothing.
const leakingToken = "mcp-usage-output-credential-fixture"

// leakingProxyURL carries userinfo, which is exactly the shape a usage or
// error line must never render. Neither half is a real credential.
const (
	leakingUser = "leakuser"
	leakingPass = "leakpass"
)

// TestProxyURLCredentialsNeverReachOutput pins the same property for the proxy
// URL. It used to take its flag default from MILLIVOLT_MCP_PROXY_URL, so
// `--help` printed `(default "http://leakuser:leakpass@...")`, and a malformed
// URL echoed the raw value through url.Parse's own error. The default is empty
// now and the environment is resolved after parsing, and the validation error
// names the failure without restating the value.
func TestProxyURLCredentialsNeverReachOutput(t *testing.T) {
	binary := buildBinary(t)
	env := []string{
		"MILLIVOLT_MCP_PROXY_URL=http://" + leakingUser + ":" + leakingPass + "@127.0.0.1:8081",
		"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken,
	}
	for _, tc := range []struct {
		name string
		args []string
		url  string
	}{
		{"help", []string{"-h"}, "http://" + leakingUser + ":" + leakingPass + "@127.0.0.1:8081"},
		{"unknown flag", []string{"--bogus"}, "http://" + leakingUser + ":" + leakingPass + "@127.0.0.1:8081"},
		{"malformed env url", nil, "http://" + leakingUser + ":" + leakingPass + "@127.0.0.1:notaport"},
		{"malformed flag url", []string{"--proxy-url", "http://" + leakingUser + ":" + leakingPass + "@[::1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(binary, tc.args...)
			environment := env
			if tc.url != "" {
				environment = []string{
					"MILLIVOLT_MCP_PROXY_URL=" + tc.url,
					"MILLIVOLT_MCP_OPERATOR_TOKEN=" + leakingToken,
				}
			}
			command.Env = environmentWithoutCredentials(environment)
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			_ = command.Run()
			for stream, text := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
				for _, secret := range []string{leakingUser, leakingPass, leakingToken} {
					if strings.Contains(text, secret) {
						t.Fatalf("proxy URL userinfo reached %s in plaintext:\n%s", stream, text)
					}
				}
			}
		})
	}
}

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
	if !strings.Contains(usage, "Usage of millivolt-mcp:") {
		t.Fatalf("standalone usage must name millivolt-mcp, got:\n%s", usage)
	}
	for _, flag := range []string{"-operator-token", "-proxy-url", "-query-max-rows", "-page-size", "MILLIVOLT_MCP_QUERY_MAX_BYTES"} {
		if !strings.Contains(usage, flag) {
			t.Fatalf("usage must still document %q, got:\n%s", flag, usage)
		}
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
