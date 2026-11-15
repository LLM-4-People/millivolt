package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
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
	// sigtermCloseDrainMargin bounds the synchronous write-channel drain
	// and final flush of store.Close, which no configuration value caps;
	// the claim caller adds it to the stale instance's own shutdown
	// timeout to compute the full SIGTERM kill grace.
	sigtermCloseDrainMargin = 5 * time.Second
	// sigtermPoll is how often we check whether the stale instance has exited.
	sigtermPoll = 100 * time.Millisecond
	// tcpListenState is the LISTEN value of the kernel's per-socket state
	// column in /proc/net/tcp and /proc/net/tcp6.
	tcpListenState = "0A"
)

// claimPort ensures only one proxy instance binds the listen address. If the
// port is already bound by another (stale) proxy instance, it kills that
// process and retries. grace bounds how long that stale instance may take to
// exit on SIGTERM before we escalate to SIGKILL; the caller derives it from
// the live configuration (see main.go). It returns a listener ready for
// http.Serve.
//
// It only kills a process whose executable is identical to ours (resolved
// /proc/<pid>/exe symlink) and which owns the LISTENING socket, never an
// unrelated service that merely shares the port number or a client socket.
func claimPort(listenAddr string, grace time.Duration) (net.Listener, error) {
	for attempt := 0; attempt < claimRetries; attempt++ {
		ln, err := net.Listen("tcp", listenAddr)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, err
		}
		// Port is taken. Try to kill the owning process if it's another proxy.
		if killed := killPortOwner(listenAddr, grace); !killed {
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
//
// The kernel's own tables are the primary source: only a socket in LISTEN
// state owns the address, while the tool probes below also match client
// sockets that merely use the same port number against another host. Every
// candidate, from whichever source, is therefore checked against the listening
// socket set before it is signalled; a same-binary client of a remote :PORT
// is left alone instead of being SIGTERMed for nothing.
func killPortOwner(addr string, grace time.Duration) bool {
	port := portFromAddr(addr)
	if port == 0 {
		return false
	}
	listeners := listenerInodes(port)
	// Strategy 1: /proc alone. Exact, and no external tool required.
	if killIfProxy(listenerOwners(listeners), listeners, grace) {
		return true
	}
	// Strategy 2: fuser (fast, standard on most Linux).
	if killWithFuser(port, listeners, grace) {
		return true
	}
	// Strategy 3: lsof.
	if killWithLsof(port, listeners, grace) {
		return true
	}
	// Strategy 4: ss + parse PID.
	if killWithSS(port, listeners, grace) {
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

// listenerInodes returns the socket inodes currently in LISTEN state on port
// in this network namespace, from the kernel's own tables. The key is the
// inode as it appears in a /proc/<pid>/fd symlink target, so ownership is
// decided on the socket, not on a port number. An unreadable table yields an
// empty set, which callers read as "ownership cannot be verified".
func listenerInodes(port int) map[string]struct{} {
	inodes := map[string]struct{}{}
	want := ":" + strings.ToUpper(strconv.FormatInt(int64(port), 16))
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			// sl local_address rem_address st ... uid timeout inode
			if len(fields) < 10 || fields[3] != tcpListenState {
				continue
			}
			if !strings.HasSuffix(strings.ToUpper(fields[1]), want) {
				continue
			}
			inodes[fields[9]] = struct{}{}
		}
	}
	return inodes
}

// listenerOwners returns the pids holding any socket in the set, by matching
// the socket:[inode] symlink targets under /proc/<pid>/fd. Pids whose fd
// directory is unreadable (another user's process without privileges) are
// skipped: the tool strategies still see them.
func listenerOwners(listeners map[string]struct{}) []string {
	if len(listeners) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var owners []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := "/proc/" + entry.Name() + "/fd"
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := strings.CutPrefix(target, "socket:[")
			if !ok {
				continue
			}
			inode = strings.TrimSuffix(inode, "]")
			if _, held := listeners[inode]; held {
				owners = append(owners, entry.Name())
				break
			}
		}
	}
	return owners
}

// probePIDs runs one port-probe tool and returns the PID tokens it printed.
// A missing tool or a failing probe reports no PIDs (the caller then treats
// the strategy as unavailable).
func probePIDs(tool string, args ...string) []string {
	if _, err := exec.LookPath(tool); err != nil {
		return nil
	}
	out, err := exec.Command(tool, args...).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// killWithFuser reclaims the port via fuser, which prints the owning PIDs to
// stdout. fuser has no listening-only selector, so its candidates rely on the
// shared ownership check.
func killWithFuser(port int, listeners map[string]struct{}, grace time.Duration) bool {
	return killIfProxy(probePIDs("fuser", fmt.Sprintf("%d/tcp", port)), listeners, grace)
}

// killWithLsof is the same probe through lsof. -sTCP:LISTEN narrows it to
// listening sockets, which the shared check then confirms.
func killWithLsof(port int, listeners map[string]struct{}, grace time.Duration) bool {
	return killIfProxy(probePIDs("lsof", "-ti", "-sTCP:LISTEN", fmt.Sprintf(":%d", port)), listeners, grace)
}

func killWithSS(port int, listeners map[string]struct{}, grace time.Duration) bool {
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
	return killIfProxy([]string{pid}, listeners, grace)
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
// identified by executable identity (resolved /proc/<pid>/exe == our own exe)
// and by holding one of the LISTENING sockets in the passed set. When the set
// is empty (the kernel tables were unreadable) socket ownership cannot be
// verified and every identity match is accepted. It never kills an unrelated
// process that merely shares a name or the port.
//
// The stale instance's exit path is SIGTERM → HTTP drain bounded by its own
// shutdown_timeout → synchronous store Close (write-channel drain plus final
// flush), so the kill grace must exceed both bounds or the SIGKILL lands
// mid-shutdown. The stale instance's own boot-time value is unknowable; the
// caller's fresh, hot-reloadable shutdown_timeout is the best available
// estimate of it, and the margin covers the drain.
//
// The listen address's host component is not compared: the ownership check is
// the listening socket on the port, so a same-binary instance bound to a
// different loopback IP on that port is still reclaimed. That is intentional -
// the exe-identity gate makes the blast radius exactly "a stale instance of
// this very binary holding this port", and the kill is graceful.
func killIfProxy(pids []string, listeners map[string]struct{}, grace time.Duration) bool {
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
		if len(listeners) > 0 && !ownsListener(pid, listeners) {
			continue // a client socket on the same port, not the listener
		}
		if !isProxyProcess(pid, myExe) {
			continue // not another instance of this binary; leave it alone
		}
		// Graceful first: SIGTERM, poll for exit (long enough for the old
		// instance's shutdown_timeout plus its synchronous store drain),
		// then SIGKILL if still alive. A failed signal is reported: it
		// otherwise only shows up as the retry loop running out.
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			log.Printf("port claim: SIGTERM pid %d: %v", pid, err)
		}
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) && processAlive(pid) {
			time.Sleep(sigtermPoll)
		}
		if processAlive(pid) {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				log.Printf("port claim: SIGKILL pid %d: %v", pid, err)
			}
		}
		killed = true
	}
	return killed
}

// ownsListener reports whether pid holds one of the listening sockets, so a
// socket:[inode] target under its fd table matches the set.
func ownsListener(pid int, listeners map[string]struct{}) bool {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		// Ownership unverifiable (a permission-restricted probe result):
		// fall back to the identity match instead of rejecting a real owner.
		return true
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		inode, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		if _, held := listeners[strings.TrimSuffix(inode, "]")]; held {
			return true
		}
	}
	return false
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

// processAlive reports whether pid still runs. A zombie has already exited and
// only waits for its parent to reap it: it holds no socket any more and cannot
// answer a signal, so it counts as dead instead of consuming the whole grace
// window. The kill-0 probe stays as the fallback when /proc is unavailable.
func processAlive(pid int) bool {
	if state, err := processState(pid); err == nil {
		return state != 'Z' && state != 'X'
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processState reads the single-letter run state from /proc/<pid>/stat. The
// comm field can contain spaces and parentheses, so the state is the field
// after the LAST ')'. A vanished process answers an error.
func processState(pid int) (byte, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 >= len(data) {
		return 0, errors.New("malformed stat")
	}
	return data[end+2], nil
}
