package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt"
)

// The credential these tests look for is a fixture value for a binary that is
// about to be refused or to print usage; it grants access to nothing.
const subcommandLeakingToken = "proxy-subcommand-usage-credential-fixture"

// TestMCPSubcommandSetupAndUsage pins that `millivolt mcp` is the MCP
// entrypoint inside the proxy binary: a missing setup exits nonzero with the
// diagnostic on stderr, help prints the MCP usage rather than the proxy's, and
// a tokenless start is refused before any transport exists. stdout stays empty
// on every one of those paths, because stdout carries the MCP protocol only.
func TestMCPSubcommandSetupAndUsage(t *testing.T) {
	binary := buildProxyBinary(t)

	t.Run("missing setup", func(t *testing.T) {
		stdout, stderr, code := runProxy(t, binary, nil, "mcp")
		if code == 0 {
			t.Fatal("millivolt mcp without setup must exit nonzero")
		}
		if stdout != "" {
			t.Fatalf("stdout must stay empty on a setup failure, got:\n%s", stdout)
		}
		if !strings.Contains(stderr, "millivolt mcp: setup:") {
			t.Fatalf("diagnostics must name the subcommand invocation, got:\n%s", stderr)
		}
		if !strings.Contains(stderr, "proxy URL is required") {
			t.Fatalf("stderr must carry the setup error, got:\n%s", stderr)
		}
	})

	t.Run("help is the mcp usage", func(t *testing.T) {
		stdout, stderr, code := runProxy(t, binary, nil, "mcp", "-h")
		if code == 0 {
			t.Fatal("millivolt mcp -h must not start anything")
		}
		if stdout != "" {
			t.Fatalf("stdout must stay empty on help, got:\n%s", stdout)
		}
		if !strings.Contains(stderr, "Usage of millivolt mcp:") {
			t.Fatalf("help must name the subcommand invocation, got:\n%s", stderr)
		}
		if !strings.Contains(stderr, "-operator-token") {
			t.Fatalf("help must print the MCP setup surface, got:\n%s", stderr)
		}
		if strings.Contains(stderr, "print-config") {
			t.Fatalf("help must not print the proxy usage, got:\n%s", stderr)
		}
	})

	t.Run("tokenless start refuses before transport", func(t *testing.T) {
		stdout, stderr, code := runProxy(t, binary, nil, "mcp", "--proxy-url", "http://127.0.0.1:1")
		if code == 0 {
			t.Fatal("a tokenless start must fail setup before the transport exists")
		}
		if stdout != "" {
			t.Fatalf("stdout must stay empty before any transport, got:\n%s", stdout)
		}
		if !strings.Contains(stderr, "operator token is required") {
			t.Fatalf("stderr must carry the setup error, got:\n%s", stderr)
		}
	})
}

// TestMCPSubcommandCredentialNeverReachesOutput runs the built proxy binary and
// checks both streams: flag.PrintDefaults runs on -h, --help and every parse
// error, so a credential used as a flag default would be printed in plaintext
// to a stream an MCP host does not capture.
func TestMCPSubcommandCredentialNeverReachesOutput(t *testing.T) {
	binary := buildProxyBinary(t)
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{"env supplied, -h", []string{"mcp", "-h"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + subcommandLeakingToken}},
		{"env supplied, --help", []string{"mcp", "--help"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + subcommandLeakingToken}},
		{"env supplied, unknown flag", []string{"mcp", "--bogus"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + subcommandLeakingToken}},
		{"env supplied, bad flag value", []string{"mcp", "--timeout", "nope"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + subcommandLeakingToken}},
		{"env supplied, bare start", []string{"mcp"}, []string{"MILLIVOLT_MCP_OPERATOR_TOKEN=" + subcommandLeakingToken}},
		{"flag supplied, -h", []string{"mcp", "-h", "--operator-token", subcommandLeakingToken}, nil},
		{"flag supplied, --help", []string{"mcp", "--help", "--operator-token", subcommandLeakingToken}, nil},
		{"flag supplied, unknown flag", []string{"mcp", "--bogus", "--operator-token", subcommandLeakingToken}, nil},
		{"flag supplied, bad flag value", []string{"mcp", "--timeout", "nope", "--operator-token", subcommandLeakingToken}, nil},
		{"flag supplied, positional argument", []string{"mcp", "stray", "--operator-token", subcommandLeakingToken}, nil},
		{"operator token before help", []string{"mcp", "--operator-token", subcommandLeakingToken, "-h"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, _ := runProxy(t, binary, tc.env, tc.args...)
			if stdout != "" {
				t.Fatalf("stdout must stay empty on error and help paths, got:\n%s", stdout)
			}
			for stream, text := range map[string]string{"stdout": stdout, "stderr": stderr} {
				if strings.Contains(text, subcommandLeakingToken) {
					t.Fatalf("the operator credential reached %s in plaintext:\n%s", stream, text)
				}
			}
		})
	}
}

// TestSubcommandLeavesVersionUntouched pins the dispatch boundary: only a
// leading "mcp" argument reaches the MCP entrypoint, so -version still prints
// the JSON build metadata on stdout with nothing on stderr.
func TestSubcommandLeavesVersionUntouched(t *testing.T) {
	binary := buildProxyBinary(t)
	stdout, stderr, code := runProxy(t, binary, nil, "-version")
	if code != 0 {
		t.Fatalf("millivolt -version exited %d, stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("millivolt -version must not write to stderr, got:\n%s", stderr)
	}
	var got millivolt.BuildInfo
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version output is not the JSON metadata: %q, %v", stdout, err)
	}
	if got.Version != millivolt.Version() || got.Revision == "" || got.GoVersion == "" {
		t.Fatalf("version metadata = %+v", got)
	}
}

// runProxy runs the built proxy binary with a clean environment carrying only
// the supplied overrides, so a developer's own MILLIVOLT_* values cannot
// influence the result or leak into an assertion.
func runProxy(t *testing.T, binary string, overrides []string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOTOOLCHAIN=local"}
	command.Env = append(environment, overrides...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	code := 0
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %s %v: %v", binary, args, err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// buildProxyBinary compiles ./cmd/proxy once for the calling test and returns
// its path.
func buildProxyBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "millivolt")
	build := exec.Command("go", "build", "-o", binary, "./cmd/proxy")
	build.Dir = proxyRepositoryRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/proxy: %v\n%s", err, output)
	}
	return binary
}

func proxyRepositoryRoot(t *testing.T) string {
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
