package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// claimRetries is how many times we retry binding after killing a stale
	// instance holding the port.
	claimRetries = 6
	// claimRetryDelay is the wait between bind attempts.
	claimRetryDelay = 300 * time.Millisecond
	// sigtermGrace is how long we wait for a stale instance to exit on SIGTERM
	// before escalating to SIGKILL. It must exceed the store flush interval so
	// the old instance can drain its write channel.
	sigtermGrace = 3 * time.Second
	// sigtermPoll is how often we check whether the stale instance has exited.
	sigtermPoll = 100 * time.Millisecond
)

// claimPort ensures only one proxy instance binds the listen address. If the
// port is already bound by another (stale) proxy instance, it kills that
// process and retries. It returns a listener ready for http.Serve.
//
// It only kills a process whose executable is identical to ours (resolved
// /proc/<pid>/exe symlink), never an unrelated service sharing the port.
func claimPort(listenAddr string) (net.Listener, error) {
	for attempt := 0; attempt < claimRetries; attempt++ {
		ln, err := net.Listen("tcp", listenAddr)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, err
		}
		// Port is taken. Try to kill the owning process if it's another proxy.
		if killed := killPortOwner(listenAddr); !killed {
			// Not a proxy instance; don't kill an unrelated service.
			return nil, fmt.Errorf("port %s in use by another process", listenAddr)
		}
		time.Sleep(claimRetryDelay)
	}
	return nil, fmt.Errorf("port %s still in use after %d retries", listenAddr, claimRetries)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// killPortOwner finds the process bound to the address and kills it if it
// looks like another instance of this proxy. Returns true if a kill happened.
func killPortOwner(addr string) bool {
	port := portFromAddr(addr)
	if port == 0 {
		return false
	}
	// Strategy 1: fuser (fast, standard on most Linux).
	if killWithFuser(port) {
		return true
	}
	// Strategy 2: lsof.
	if killWithLsof(port) {
		return true
	}
	// Strategy 3: ss + parse PID.
	if killWithSS(port) {
		return true
	}
	return false
}

// portFromAddr extracts the port from a listen address (":8080",
// "127.0.0.1:8080", "[::1]:8080", "localhost:8080"). Returns 0 on parse
// failure, which callers treat as "cannot determine owner".
func portFromAddr(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}

func killWithFuser(port int) bool {
	if _, err := exec.LookPath("fuser"); err != nil {
		return false
	}
	out, err := exec.Command("fuser", fmt.Sprintf("%d/tcp", port)).Output()
	if err != nil {
		return false
	}
	// fuser prints PIDs to stdout.
	fields := strings.Fields(string(out))
	return killIfProxy(fields)
}

func killWithLsof(port int) bool {
	if _, err := exec.LookPath("lsof"); err != nil {
		return false
	}
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf(":%d", port)).Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	return killIfProxy(fields)
}

func killWithSS(port int) bool {
	if _, err := exec.LookPath("ss"); err != nil {
		return false
	}
	out, err := exec.Command("ss", "-tlnpH", fmt.Sprintf("sport = :%d", port)).Output()
	if err != nil {
		return false
	}
	// Look for "pid=1234," in the output.
	pid := extractPID(string(out))
	if pid == "" {
		return false
	}
	return killIfProxy([]string{pid})
}

func extractPID(s string) string {
	i := strings.Index(s, "pid=")
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i+4:], ",")
	if j < 0 {
		return ""
	}
	return s[i+4 : i+4+j]
}

// killIfProxy kills each PID that is another instance of this exact binary,
// identified by executable identity (resolved /proc/<pid>/exe == our own exe).
// It never kills an unrelated process that merely shares a name or the port.
//
// Note: the port matchers (fuser/lsof/ss) match the port NUMBER only, ignoring
// the listen address's host component, so a same-binary instance bound to a
// different loopback IP on the same port would also be reclaimed. That is
// acceptable: the exe-identity gate makes the blast radius exactly "a stale
// instance of this very binary", and the kill is graceful (SIGTERM → drain →
// SIGKILL). Per-interface filtering would complicate all three tool parsers
// for no real-world gain.
func killIfProxy(pids []string) bool {
	myExe, err := os.Executable()
	if err != nil {
		return false // cannot identify ourselves; refuse to kill anything
	}
	killed := false
	for _, p := range pids {
		pid, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || pid <= 0 {
			continue
		}
		if pid == os.Getpid() {
			continue // never kill ourselves
		}
		if !isProxyProcess(pid, myExe) {
			continue // not another instance of this binary; leave it alone
		}
		// Graceful first: SIGTERM, poll for exit (long enough for the old
		// instance to flush its write channel), then SIGKILL if still alive.
		syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(sigtermGrace)
		for time.Now().Before(deadline) && processAlive(pid) {
			time.Sleep(sigtermPoll)
		}
		if processAlive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		killed = true
	}
	return killed
}

// isProxyProcess reports whether pid is running our exact executable. It
// resolves the target's /proc/<pid>/exe symlink and compares it to our own
// resolved path - identity, not a name/suffix guess, so a process that merely
// happens to be named like ours is never matched.
func isProxyProcess(pid int, myExe string) bool {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	// A binary replaced while running reads back with a " (deleted)" suffix.
	exe = strings.TrimSuffix(exe, " (deleted)")
	if myReal, err := filepath.EvalSymlinks(myExe); err == nil {
		myExe = myReal
	}
	return exe == myExe
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
