package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{":8080", 8080},
		{"127.0.0.1:8080", 8080},
		{"[::1]:8080", 8080},
		{"localhost:8080", 8080},
		{"[::]:9000", 9000},
		{"8080", 0},      // no colon → SplitHostPort error
		{"host", 0},      // no port
		{"host:http", 0}, // non-numeric port
		{"", 0},          // empty
	}
	for _, c := range cases {
		if got := portFromAddr(c.addr); got != c.want {
			t.Errorf("portFromAddr(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

func TestIsProxyProcessSelf(t *testing.T) {
	// The current process's own exe must match itself.
	myExe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	if !isProxyProcess(os.Getpid(), myExe) {
		t.Errorf("isProxyProcess(self) = false, want true (identity match)")
	}
}

func TestIsProxyProcessOther(t *testing.T) {
	myExe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	// PID 1 (init/systemd) runs a different executable - must not match ours.
	if isProxyProcess(1, myExe) {
		t.Errorf("isProxyProcess(pid 1) = true, want false (different exe)")
	}
	// A nonexistent pid must not match.
	if isProxyProcess(1<<30, myExe) {
		t.Errorf("isProxyProcess(bogus pid) = true, want false")
	}
}

// TestProbePIDsPATHFixture pins the port-probe extraction through real PATH
// fixtures: a stub tool emitting crafted output yields exactly its PID
// tokens (whitespace-split, any layout), a failing probe and a missing tool
// yield none, and the caller treats both as "strategy unavailable". Nothing
// exercised probePIDs before; a parsing or lookup regression (the stale
// instance reclaimer's first two strategies) went unnoticed.
func TestProbePIDsPATHFixture(t *testing.T) {
	dir := t.TempDir()
	stub := func(name, script string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// The unique tool name cannot shadow a real system tool when PATH is
	// narrowed to the fixture directory.
	const tool = "millivolt-probe-stub"
	run := func(script string) []string {
		t.Helper()
		stub(tool, script)
		t.Setenv("PATH", dir)
		return probePIDs(tool, "8080/tcp")
	}
	if got := run("#!/bin/sh\nprintf '1234\\n5678 90\\n'"); strings.Join(got, ",") != "1234,5678,90" {
		t.Fatalf("crafted multi-line output = %v, want [1234 5678 90]", got)
	}
	if got := run("#!/bin/sh\nprintf ''"); len(got) != 0 {
		t.Fatalf("empty output = %v, want no PIDs", got)
	}
	if got := run("#!/bin/sh\nexit 1"); len(got) != 0 {
		t.Fatalf("failing probe = %v, want no PIDs (strategy unavailable)", got)
	}
	if got := run("#!/bin/sh\nprintf 'nope' 1>&2\nexit 0"); len(got) != 0 {
		t.Fatalf("stderr-only probe = %v, want no PIDs (stdout carries them)", got)
	}
	// A missing tool reports no PIDs, never an error path.
	t.Setenv("PATH", dir)
	if err := os.Remove(filepath.Join(dir, tool)); err != nil {
		t.Fatal(err)
	}
	if got := probePIDs(tool, "8080/tcp"); len(got) != 0 {
		t.Fatalf("missing tool = %v, want no PIDs", got)
	}
}
