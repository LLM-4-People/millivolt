package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// selfSocketInodes returns the inode of every socket currently open in this
// process. A listener and the connections to it are all sockets, so the set
// proves which of them the LISTEN table does and does not name.
func selfSocketInodes(t *testing.T) map[string]struct{} {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	inodes := map[string]struct{}{}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") {
			continue
		}
		inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = struct{}{}
	}
	return inodes
}

// TestListenerInodesExcludesConnectedSockets pins the ownership rule: only a
// LISTEN socket owns the port. A same-binary process that merely holds a
// client connection to :PORT used to be reclaimed alongside the listener, and
// an unrelated client of a remote :PORT could consume the whole retry loop.
func TestListenerInodesExcludesConnectedSockets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	listeners := listenerInodes(port)
	if len(listeners) == 0 {
		t.Fatalf("no listening socket found for :%d", port)
	}
	before := selfSocketInodes(t)
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	after := selfSocketInodes(t)
	var opened []string
	for inode := range after {
		if _, had := before[inode]; !had {
			opened = append(opened, inode)
		}
	}
	if len(opened) < 2 {
		t.Fatalf("expected the client and accepted sockets to be new, got %v", opened)
	}
	for _, inode := range opened {
		if _, listed := listeners[inode]; listed {
			t.Errorf("connected socket inode %s was reported as the listener", inode)
		}
	}
	// The process holding the listener resolves back as its owner.
	owners := listenerOwners(listeners)
	found := false
	for _, pid := range owners {
		if pid == strconv.Itoa(os.Getpid()) {
			found = true
		}
	}
	if !found {
		t.Errorf("listenerOwners(%v) = %v, want it to include this pid %d", listeners, owners, os.Getpid())
	}
}

// TestOwnsListenerRejectsAConnectedOnlyProcess pins the socket-level check
// used by every probe strategy.
func TestOwnsListenerRejectsAConnectedOnlyProcess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	listeners := listenerInodes(port)
	if !ownsListener(os.Getpid(), listeners) {
		t.Fatalf("this process owns the listener on :%d but was rejected", port)
	}
	// A port with no listening socket at all: nothing can own it.
	if ownsListener(os.Getpid(), listenerInodes(1)) {
		t.Error("ownsListener matched a listener on port 1")
	}
}

// TestKillWithLsofRestrictsToListeners pins that the lsof probe asks for
// LISTEN state itself; without it the probe also returns client sockets.
func TestKillWithLsofRestrictsToListeners(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	killWithLsof(8080, nil, time.Second)
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the lsof probe never ran: %v", err)
	}
	if !strings.Contains(string(args), "-sTCP:LISTEN") {
		t.Errorf("lsof args = %q, want -sTCP:LISTEN", args)
	}
}

// TestProcessAliveTreatsZombieAsDead pins the reclaim wait: a zombie has
// exited, holds no socket and cannot answer a signal, so it must not consume
// the whole SIGKILL grace window.
func TestProcessAliveTreatsZombieAsDead(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a fixture child: %v", err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state, err := processState(pid); err == nil && state == 'Z' {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state, err := processState(pid); err != nil || state != 'Z' {
		_ = cmd.Wait()
		t.Skipf("fixture child never reached zombie state: %v %q", err, state)
	}
	// Still unreaped here: the parent process group has not called Wait.
	if processAlive(pid) {
		t.Error("a zombie reports alive")
	}
	_ = cmd.Wait()
	if processAlive(pid) {
		t.Error("a reaped pid reports alive")
	}
}

// claimChild starts this test binary (or a copy of it) as a listener on addr
// and returns once it answers. The copy is a different executable file, so
// the identity gate must leave it alone.
func claimChild(t *testing.T, binary, addr string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary, "-test.run=^TestClaimPortReclaimsStaleSameBinaryOwner$")
	cmd.Env = append(os.Environ(), "MILLIVOLT_TEST_PORTCLAIM_ADDR="+addr)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start claim child: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Fatalf("claim child never listened: %s", stderr.String())
	return nil
}

// TestClaimPortReclaimsStaleSameBinaryOwner pins the whole reclaimer against a
// live keep-alive child: a listener running this exact binary is signalled and
// the port is rebound, a listener running a copied binary is left alive and the
// claim reports the conflict, and the ss parser accepts only a numeric
// "pid=NNN," field.
func TestClaimPortReclaimsStaleSameBinaryOwner(t *testing.T) {
	if addr := os.Getenv("MILLIVOLT_TEST_PORTCLAIM_ADDR"); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			os.Exit(2)
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
		select {} // the accept goroutine keeps the runtime awake
	}

	myExe, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path: %v", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))

	t.Run("same binary is reclaimed", func(t *testing.T) {
		child := claimChild(t, myExe, addr)
		ln, err := claimPort(addr, 5*time.Second)
		if err != nil {
			_ = child.Process.Kill()
			t.Fatalf("claimPort = %v, want the stale listener reclaimed", err)
		}
		ln.Close()
		// The child is a plain listener with no handler, so SIGTERM terminates
		// it: Wait reports the signal as an error, and the signal itself is the
		// proof the reclaimer used the graceful stop rather than SIGKILL.
		err = child.Wait()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("reclaimed child did not exit on SIGTERM: %v", err)
		}
	})

	if residual := listenerInodes(portFromAddr(addr)); len(residual) != 0 {
		t.Fatalf("port %s still has a listener after the reclaim", addr)
	}

	t.Run("another binary is left alive", func(t *testing.T) {
		copied := filepath.Join(t.TempDir(), "other-binary")
		data, err := os.ReadFile(myExe)
		if err != nil {
			t.Fatalf("read own binary: %v", err)
		}
		if err := os.WriteFile(copied, data, 0o755); err != nil {
			t.Fatalf("copy own binary: %v", err)
		}
		child := claimChild(t, copied, addr)
		t.Cleanup(func() {
			_ = child.Process.Kill()
			_ = child.Wait()
		})
		if ln, err := claimPort(addr, time.Second); err == nil {
			ln.Close()
			t.Fatal("claimPort rebound through another binary; want the conflict reported")
		}
		if !processAlive(child.Process.Pid) {
			t.Fatal("the other binary was killed; want it left alive")
		}
	})

	for _, row := range []struct {
		in   string
		want string
	}{
		{`users:(("proxy",pid=1234,fd=7))`, "1234"},
		{`users:(("a",pid=1,fd=3),("b",pid=99,fd=4))`, "1"},
		{`pid=1234`, ""}, // no comma terminator
		{`pid=,`, ""},    // empty field
		{`pid=abc,`, ""}, // not numeric
		{`pid=12a,`, ""}, // partly numeric
		{`no field at all`, ""},
	} {
		if got := extractPID(row.in); got != row.want {
			t.Errorf("extractPID(%q) = %q, want %q", row.in, got, row.want)
		}
	}
}
