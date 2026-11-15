package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// devScriptFixture starts a listener that carries the dev -pid-file flag in its
// own argv, which is exactly the shape the script's identity check looks for.
// Python is the smallest available stand-in for the proxy binary.
func devScriptFixture(t *testing.T, port int, pidFile string) *exec.Cmd {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("the dev script identity fixture needs python3")
	}
	code := `import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", ` + strconv.Itoa(port) + `))
s.listen()
time.sleep(120)
`
	cmd := exec.Command(python, "-c", code, "-pid-file", pidFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// freePort asks the kernel for one and gives it back.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := exec.Command("python3", "-c", `
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
`).Output()
	if err != nil {
		t.Skipf("cannot allocate a fixture port: %v", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(ln)))
	if err != nil {
		t.Skipf("bad fixture port %q", ln)
	}
	return port
}

// TestDevScriptStatusFindsTheListenerWhenThePIDFileIsStale pins O5: the handoff
// child rewrites the pid file before it is ready, so a child that dies first
// leaves the file naming a dead pid. status and stop must still find the
// serving instance by the listener on the instance's port, and only when that
// process carries this exact -pid-file flag.
func TestDevScriptStatusFindsTheListenerWhenThePIDFileIsStale(t *testing.T) {
	const dir = "/tmp/millivolt"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	base := filepath.Join(dir, "millivolt-dev-"+strconv.Itoa(port))
	pidFile, configFile := base+".pid", base+".yaml"
	if err := os.WriteFile(configFile, []byte("shutdown_timeout: 1s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(configFile) })
	// A pid that cannot be alive: the child's pid after a pre-readiness death.
	if err := os.WriteFile(pidFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(pidFile) })

	fixture := devScriptFixture(t, port, pidFile)
	// The listener may need a moment to bind.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("ss", "-tlnH", "sport = :"+strconv.Itoa(port)).Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("ss cannot see the fixture listener")
		}
		time.Sleep(20 * time.Millisecond)
	}

	env := append(os.Environ(), "DEV_PORT="+strconv.Itoa(port), "DEV_HOST=127.0.0.1", "DEV_DB=none")
	script := "../../scripts/dev.sh"
	status := exec.Command("bash", script, "status")
	status.Env = env
	out, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("status with a stale pid file failed: %v: %s", err, out)
	}
	want := "dev instance running (pid " + strconv.Itoa(fixture.Process.Pid) + ")"
	if !strings.Contains(string(out), want) {
		t.Fatalf("status = %q, want it to name the listener pid %d", out, fixture.Process.Pid)
	}

	// stop resolves the same identity, signals it, and clears the pid file.
	stop := exec.Command("bash", script, "stop")
	stop.Env = env
	out, err = stop.CombinedOutput()
	_ = out
	if err != nil {
		t.Fatalf("stop with a stale pid file failed: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "stopped dev instance (pid "+strconv.Itoa(fixture.Process.Pid)+")") {
		t.Fatalf("stop = %q, want it to name the listener pid", out)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stop left the pid file behind: %v", err)
	}
}

// TestDevScriptRefusesAnotherProcessOnThePort pins the narrowness of the
// fallback: a listener on the instance's port that does not carry this
// -pid-file flag is not the instance.
func TestDevScriptRefusesAnotherProcessOnThePort(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	port := freePort(t)
	base := "/tmp/millivolt/millivolt-dev-" + strconv.Itoa(port)
	pidFile := base + ".pid"
	if err := os.MkdirAll("/tmp/millivolt", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(pidFile) })
	// Same shape, but no -pid-file flag in the argv.
	cmd := exec.Command(python, "-c", `
import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", `+strconv.Itoa(port)+`))
s.listen()
time.sleep(120)
`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("ss", "-tlnH", "sport = :"+strconv.Itoa(port)).Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("ss cannot see the fixture listener")
		}
		time.Sleep(20 * time.Millisecond)
	}
	env := append(os.Environ(), "DEV_PORT="+strconv.Itoa(port), "DEV_HOST=127.0.0.1", "DEV_DB=none")
	status := exec.Command("bash", "../../scripts/dev.sh", "status")
	status.Env = env
	out, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed: %v: %s", err, out)
	}
	if strings.Contains(string(out), "dev instance running") {
		t.Fatalf("status accepted a process without the pid-file flag: %q", out)
	}
}
