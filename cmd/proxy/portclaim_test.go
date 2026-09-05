package main

import (
	"os"
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
