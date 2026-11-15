package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFatalDrainRunsProcessDrain pins the one fatal boot/resume exit: it must
// run the drain before exiting, or a resumed-but-unusable process drops every
// buffered record. The child process installs a marker drain and calls
// fatalDrain, so the exit code and the drain side effect are both the real
// ones; a log.Fatalf regression fails on the missing marker.
func TestFatalDrainRunsProcessDrain(t *testing.T) {
	if marker := os.Getenv("MILLIVOLT_TEST_FATAL_DRAIN_MARKER"); marker != "" {
		processDrain = func() {
			if err := os.WriteFile(marker, []byte("drained"), 0o600); err != nil {
				os.Exit(2)
			}
		}
		fatalDrain("restart: %v", "boom")
		os.Exit(3) // fatalDrain must not return
	}
	marker := filepath.Join(t.TempDir(), "drained")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFatalDrainRunsProcessDrain$")
	cmd.Env = append(os.Environ(), "MILLIVOLT_TEST_FATAL_DRAIN_MARKER="+marker)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("fatalDrain returned; want exit status 1")
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if body, readErr := os.ReadFile(marker); readErr != nil || string(body) != "drained" {
		t.Fatalf("drain did not run before exit: %v %q", readErr, body)
	}
	if !strings.Contains(stderr.String(), "restart: boom") {
		t.Fatalf("fatal reason missing from the log: %s", stderr.String())
	}
}

// TestFatalDrainWithoutDrain pins the pre-store path: inheritedListener runs
// before main installs the drain, so a nil processDrain must still exit.
func TestFatalDrainWithoutDrain(t *testing.T) {
	if os.Getenv("MILLIVOLT_TEST_FATAL_DRAIN_NIL") != "" {
		processDrain = nil
		fatalDrain("restart: invalid %s=%q", envListenFD, "x")
		os.Exit(3)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFatalDrainWithoutDrain$")
	cmd.Env = append(os.Environ(), "MILLIVOLT_TEST_FATAL_DRAIN_NIL=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("fatalDrain returned with no drain installed; want exit status 1")
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
}
