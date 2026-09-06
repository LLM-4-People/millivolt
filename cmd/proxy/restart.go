package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The restart choreography (POST /admin/restart) performs a graceful binary
// upgrade with zero lost requests:
//
//  1. build   - `go build ./cmd/proxy` from the module root. Failure aborts
//     cleanly: nothing has been touched, the proxy keeps serving.
//  2. commit  - the listening socket is dup()ed (it must survive
//     httpSrv.Shutdown, which closes the server's listener) and the dashboard
//     SSE feeds are closed (ephemeral + resumable; browsers reconnect).
//  3. drain   - httpSrv.Shutdown waits for in-flight requests (LLM streams)
//     to finish, bounded by restart_drain_timeout (expiry aborts the restart).
//     The server stops
//     accepting, but the dup()ed socket keeps the kernel accept queue alive:
//     new connections complete (SYN-ACK) and queue instead of being refused.
//  4. flush   - the store's write channel is drained synchronously, so every
//     finalized record is durable before the child opens the database (the
//     child's totals scan + ring backfill see the complete history; no
//     concurrent-writer window exists). A failed flush aborts the restart.
//  5. handoff - the fresh binary is spawned with the dup()ed socket as fd 3;
//     it bypasses claimPort (which would exe-identity-match and SIGTERM this
//     still-draining parent), boots fully, signals readiness on fd 4, and
//     serves the queued + new connections. The old process then exits.
//
// A failure after the commit point kills the child and resumes serving on the
// dup()ed socket (the store was only flushed, never closed, so metrics stay
// fully functional).

// Internal guardrails bounding the mechanical steps. Not user-tunable - the
// user-facing drain bound is restart_drain_timeout in internal/config.
const (
	restartBuildTimeout = 5 * time.Minute  // cold builds (empty cache) can be slow
	restartReadyTimeout = 30 * time.Second // child boot: migrate + totals scan + backfill
)

// FD-handoff env keys. ExtraFiles start at fd 3: fd 3 is the listening
// socket, fd 4 the readiness pipe (child writes "ok" once fully booted).
const (
	envListenFD = "MILLIVOLT_LISTEN_FD"
	envReadyFD  = "MILLIVOLT_READY_FD"
)

// Restart phases (GET /admin/restart "phase"). They are the visible step
// list the dashboard renders: building → draining → flushing → handing_off,
// each reported the moment it actually starts (the watch stream below), never
// pre-claimed.
const (
	restartIdle = iota
	restartBuilding
	restartHandingOff
	restartDraining
	restartFlushing
)

func phaseName(p int) string {
	switch p {
	case restartBuilding:
		return "building"
	case restartHandingOff:
		return "handing_off"
	case restartDraining:
		return "draining"
	case restartFlushing:
		return "flushing"
	default:
		return "idle"
	}
}

// phaseRank orders the phases for the UI's step stepper (idle unranked).
func phaseRank(p int) int {
	switch p {
	case restartBuilding:
		return 0
	case restartDraining:
		return 1
	case restartFlushing:
		return 2
	case restartHandingOff:
		return 3
	default:
		return -1
	}
}

// restartDeps wires the restarter to the running process. Everything the
// choreography touches is injected so the state machine is testable.
type restartDeps struct {
	listener net.Listener
	server   *http.Server
	// cloneServer builds a fresh http.Server with the same handler, timeouts
	// and base context - used only by the failed-handoff resume path.
	cloneServer func() *http.Server
	closeFeeds  func()
	flushStore  func() error
	// drainTimeout reads the live config snapshot (restart_drain_timeout
	// hot-reloads). 0 = wait indefinitely.
	drainTimeout func() time.Duration
	// args are the child's argv (the parent's own flags, verbatim).
	args []string
}

type restarter struct {
	mu      sync.Mutex
	phase   int
	phaseAt time.Time
	errMsg  string
	bootAt  time.Time
	pid     int
	root    string
	rootWhy string
	goBin   string
	deps    *restartDeps

	// watchers are the live step streams (GET /admin/restart?watch=1). Each
	// holds a hijacked connection; setPhase/publishProgress fan every real
	// transition out to them - the only way a dashboard can observe the drain,
	// since the old server has stopped accepting and polls hang in the
	// inherited socket's backlog. Publish never blocks (drop, never stall the
	// choreography).
	watchers map[chan map[string]any]struct{}

	// inHandoff marks the commit point: the old server has stopped accepting
	// and the signal handler must not race the handoff with a second
	// shutdown sequence.
	inHandoff atomic.Bool
	// resumed is closed if a committed handoff fails and serving resumed -
	// the signal loop waits on it and keeps running.
	resumed chan struct{}
	// Failed timed drains leave active handlers on a retired server. A later
	// restart/shutdown must drain those too before declaring storage quiescent.
	retired []*http.Server
}

func newRestarter(deps *restartDeps) *restarter {
	root, why := findModuleRoot()
	goBin, _ := exec.LookPath("go")
	return &restarter{
		bootAt:   time.Now(),
		pid:      os.Getpid(),
		root:     root,
		rootWhy:  why,
		goBin:    goBin,
		deps:     deps,
		watchers: map[chan map[string]any]struct{}{},
		resumed:  make(chan struct{}),
	}
}

// findModuleRoot locates the Go module root this binary was built from, via
// the compile-time source path of this file (absolute unless built with
// -trimpath, which cannot self-locate its source - restart then stays
// unavailable and says so, never guesses).
func findModuleRoot() (string, string) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		return "", "cannot locate own source path"
	}
	return moduleRootFrom(file)
}

func moduleRootFrom(sourceFile string) (string, string) {
	if _, err := os.Stat(sourceFile); err != nil {
		return "", "source moved since build (" + sourceFile + " not found)"
	}
	dir := filepath.Dir(sourceFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if fi, err := os.Stat(filepath.Join(dir, "cmd", "proxy")); err == nil && fi.IsDir() {
				return dir, ""
			}
			return "", "module at " + dir + " has no cmd/proxy"
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "no go.mod above " + filepath.Dir(sourceFile)
		}
		dir = parent
	}
}

func (rs *restarter) setPhase(p int, errMsg string) {
	rs.mu.Lock()
	rs.phase = p
	rs.phaseAt = time.Now()
	rs.errMsg = errMsg
	rs.mu.Unlock()
	rs.publish()
}

// publish fans the current status out to every watch stream. Delivery drops
// rather than blocks - a slow dashboard must never stall the choreography.
func (rs *restarter) publish() {
	rs.mu.Lock()
	ev := rs.statusLocked()
	for ch := range rs.watchers {
		select {
		case ch <- ev:
		default:
		}
	}
	rs.mu.Unlock()
}

// publishProgress emits a same-phase progress beat (drain elapsed) so the
// dashboard can render a live timer inside the current step.
func (rs *restarter) publishProgress() { rs.publish() }

// addWatcher registers a watch stream; removeWatcher unregisters and closes
// the channel (the stream ends whenever the state machine returns to idle -
// the UI then falls back to polling).
func (rs *restarter) addWatcher(ch chan map[string]any) {
	rs.mu.Lock()
	rs.watchers[ch] = struct{}{}
	rs.mu.Unlock()
}

func (rs *restarter) removeWatcher(ch chan map[string]any) {
	rs.mu.Lock()
	if _, ok := rs.watchers[ch]; ok {
		delete(rs.watchers, ch)
		close(ch)
	}
	rs.mu.Unlock()
}

// available reports whether a restart can even be attempted (source located
// + go toolchain present). Computed at boot.
func (rs *restarter) available() bool {
	return rs.root != "" && rs.goBin != ""
}

func (rs *restarter) availReason() string {
	switch {
	case rs.root != "" && rs.goBin != "":
		return ""
	case rs.root == "":
		return rs.rootWhy
	default:
		return "go toolchain not found on PATH"
	}
}

func (rs *restarter) status() map[string]any {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.statusLocked()
}

// statusLocked builds the status document; caller holds rs.mu. The extra
// fields drive the dashboard's step tracker: phase_ms is when the current
// phase started, drain_timeout_ms/elapsed_ms render the live drain window.
func (rs *restarter) statusLocked() map[string]any {
	st := map[string]any{
		"ok":         true,
		"phase":      phaseName(rs.phase),
		"error":      rs.errMsg,
		"pid":        rs.pid,
		"started_at": rs.bootAt.UnixMilli(),
		"available":  rs.available(),
		"reason":     rs.availReason(),
		"phase_ms":   rs.phaseAt.UnixMilli(),
		"rank":       phaseRank(rs.phase),
	}
	if rs.deps != nil && rs.deps.drainTimeout != nil {
		st["drain_timeout_ms"] = rs.deps.drainTimeout().Milliseconds()
	}
	if rs.phase == restartDraining && !rs.phaseAt.IsZero() {
		st["drain_elapsed_ms"] = time.Since(rs.phaseAt).Milliseconds()
	}
	return st
}

// request starts a restart. Deny by default: only one may run at a time, and
// an unavailable source/toolchain is rejected before anything is touched.
func (rs *restarter) request() (bool, string) {
	rs.mu.Lock()
	if !rs.available() {
		rs.mu.Unlock()
		return false, rs.availReason()
	}
	if rs.phase != restartIdle {
		rs.mu.Unlock()
		return false, "restart already " + phaseName(rs.phase)
	}
	rs.phase = restartBuilding
	rs.phaseAt = time.Now()
	rs.errMsg = ""
	rs.mu.Unlock()
	// Publish the building step so the dashboard's watch stream renders it the
	// moment the run starts (the choreography itself streams the rest).
	rs.publish()
	go rs.run()
	return true, ""
}

// committed reports whether the handoff owns the shutdown sequence.
func (rs *restarter) committed() bool { return rs.inHandoff.Load() }

func (rs *restarter) servingServer() *http.Server {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.deps.server
}

// drain is the shared quiescence fence for restarts and process shutdown.
func (rs *restarter) drain(ctx context.Context) error {
	rs.mu.Lock()
	servers := append([]*http.Server{rs.deps.server}, rs.retired...)
	rs.mu.Unlock()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			return err
		}
	}
	rs.mu.Lock()
	rs.retired = nil
	rs.mu.Unlock()
	return nil
}

func (rs *restarter) prepareHandoff(ctx context.Context) error {
	if err := rs.drain(ctx); err != nil {
		return fmt.Errorf("drain failed; restart aborted: %w", err)
	}
	rs.setPhase(restartFlushing, "")
	if err := rs.deps.flushStore(); err != nil {
		return fmt.Errorf("metrics flush failed; restart aborted: %w", err)
	}
	return nil
}

func (rs *restarter) run() {
	var listenFile *os.File
	var bin string
	defer func() {
		if r := recover(); r != nil {
			log.Printf("restart: panic: %v", r)
			rs.failAndResume(fmt.Errorf("panic: %v", r), listenFile, bin)
		}
		if listenFile != nil {
			listenFile.Close()
		}
	}()

	// 1. Build. Nothing has been touched yet - failure aborts cleanly.
	var err error
	bin, err = rs.build()
	if err != nil {
		log.Printf("restart: %v (proxy untouched, still serving)", err)
		rs.finishFailure(err)
		return
	}
	defer os.Remove(bin) // safe on Linux even while the child runs from it

	// ---- commit point: from here the old server stops accepting. ----
	rs.inHandoff.Store(true)
	rs.setPhase(restartDraining, "")

	// 2. Duplicate the listening socket so it survives Shutdown (which closes
	//    the server's listener) and can be inherited by the child.
	tcp, ok := rs.deps.listener.(*net.TCPListener)
	if !ok {
		rs.failAndResume(errors.New("listener is not a TCPListener"), nil, bin)
		return
	}
	listenFile, err = tcp.File()
	if err != nil {
		rs.failAndResume(fmt.Errorf("dup listener: %w", err), nil, bin)
		return
	}

	// 3. End the dashboard SSE feeds now: they never finish on their own and
	//    would hold Shutdown open forever. They are ephemeral and resumable -
	//    browsers reconnect (to the child, or back here on a failed handoff).
	rs.deps.closeFeeds()

	// 4. Drain: wait for in-flight requests (LLM streams) to finish. New
	//    connections queue in the kernel backlog on the dup()ed socket. A
	//    1s progress ticker streams the live elapsed time to the dashboard's
	//    watch connection - polls cannot reach a process that has stopped
	//    accepting, but the already-hijacked watch connection can.
	drainStart := time.Now()
	drainTo := rs.deps.drainTimeout()
	err = rs.prepareWithProgress(drainTo)
	if err != nil {
		rs.failAndResume(err, listenFile, bin)
		return
	}
	log.Printf("restart: drained in %s (in-flight streams finished; restart_drain_timeout=%s)", time.Since(drainStart).Round(time.Millisecond), drainTo)

	// prepareHandoff already drained every server and flushed metrics. The
	// child may now open the database without overlapping predecessor writes.
	rs.setPhase(restartHandingOff, "")
	readyR, readyW, err := os.Pipe()
	if err != nil {
		rs.failAndResume(fmt.Errorf("ready pipe: %w", err), listenFile, bin)
		return
	}
	defer readyR.Close()
	cmd, err := rs.spawn(bin, listenFile, readyW)
	readyW.Close() // parent never writes; the child holds the only write end
	if err != nil {
		rs.failAndResume(err, listenFile, bin)
		return
	}
	if err := awaitReady(readyR, restartReadyTimeout); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		rs.failAndResume(err, listenFile, bin)
		return
	}

	log.Printf("restart: handed off to pid %d; old process (pid %d) exiting", cmd.Process.Pid, rs.pid)
	os.Remove(bin) // os.Exit skips defers; keep /tmp clean (safe: the child runs from its own inode)
	os.Exit(0)
}

// A scoped lifetime guarantees ticker/context cleanup even when a dependency
// panics and the outer restart recovery resumes serving.
func (rs *restarter) prepareWithProgress(drainTo time.Duration) error {
	stopTick := make(chan struct{})
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				rs.publishProgress()
			case <-stopTick:
				return
			}
		}
	}()
	defer func() { close(stopTick); <-tickDone }()
	var ctx context.Context
	var cancel context.CancelFunc
	if drainTo > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), drainTo)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	return rs.prepareHandoff(ctx)
}

// build compiles ./cmd/proxy from the module root into a fresh temp binary.
// The running binary is never touched, so a failed build leaves the proxy
// untouched (the same fail-fast rule scripts/dev.sh applies).
func (rs *restarter) build() (string, error) {
	if rs.root == "" {
		return "", fmt.Errorf("restart unavailable: %s", rs.rootWhy)
	}
	if rs.goBin == "" {
		return "", errors.New("restart unavailable: go toolchain not found on PATH")
	}
	f, err := os.CreateTemp("", "millivolt-rebuild-*.bin")
	if err != nil {
		return "", fmt.Errorf("temp binary: %w", err)
	}
	path := f.Name()
	f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), restartBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, rs.goBin, "build", "-o", path, "./cmd/proxy")
	cmd.Dir = rs.root
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(path)
		if ctx.Err() != nil {
			return "", fmt.Errorf("build timed out after %v", restartBuildTimeout)
		}
		return "", fmt.Errorf("build failed: %s", tailLines(string(out), 12))
	}
	return path, nil
}

func (rs *restarter) spawn(bin string, listenFile, readyW *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(bin, rs.deps.args...)
	// exec.Cmd dedupes env keys keeping the last occurrence, so these win
	// even when this process was itself started as a handoff child.
	cmd.Env = append(os.Environ(), envListenFD+"=3", envReadyFD+"=4")
	cmd.ExtraFiles = []*os.File{listenFile, readyW}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}
	return cmd, nil
}

func awaitReady(r *os.File, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		n, err := r.Read(buf)
		if err != nil {
			done <- err
			return
		}
		if strings.TrimSpace(string(buf[:n])) != "ok" {
			done <- fmt.Errorf("child readiness garbled: %q", string(buf[:n]))
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("child not ready within %v", timeout)
	}
}

// failAndResume is the post-commit failure path: kill the child (if any),
// resume serving on the dup()ed socket, and return the restarter to idle so
// another attempt is possible. The store was only flushed, never closed, so
// metrics keep working end to end.
func (rs *restarter) failAndResume(err error, listenFile *os.File, bin string) {
	if bin != "" {
		os.Remove(bin)
	}
	resumeErr := error(nil)
	if listenFile != nil {
		ln, lerr := net.FileListener(listenFile)
		if lerr != nil {
			resumeErr = fmt.Errorf("resume listener: %w", lerr)
		} else {
			srv := rs.deps.cloneServer()
			rs.mu.Lock()
			rs.retired = append(rs.retired, rs.deps.server)
			rs.deps.listener, rs.deps.server = ln, srv
			rs.mu.Unlock()
			go func() {
				if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
					// The resumed server is the process's only listener; a
					// mid-life death must not leave a silent zombie - die
					// loudly so the operator restarts manually.
					log.Fatalf("restart: resumed server failed: %v", serr)
				}
			}()
			log.Printf("restart: serving resumed on the inherited socket")
		}
	}
	if resumeErr != nil {
		// Nothing further can be done here - the process is alive but cannot
		// accept; say so loudly and let the operator restart it manually.
		log.Fatalf("restart: %v; resume failed: %v - manual restart required", err, resumeErr)
	}
	rs.finishFailure(err)
	log.Printf("restart: FAILED: %v (proxy resumed; no requests lost)", err)
}

// Both build failures and resumed handoffs retire the run atomically.
func (rs *restarter) finishFailure(err error) {
	// Publish completion, retire this run's watchers and re-arm its wait fence
	// atomically. A new restart cannot have its watchers closed by this run.
	rs.mu.Lock()
	rs.inHandoff.Store(false)
	rs.phase, rs.phaseAt, rs.errMsg = restartIdle, time.Now(), err.Error()
	ev := rs.statusLocked()
	for ch := range rs.watchers {
		select {
		case ch <- ev:
		default:
		}
		close(ch)
	}
	rs.watchers = map[chan map[string]any]struct{}{}
	close(rs.resumed)
	rs.resumed = make(chan struct{})
	rs.mu.Unlock()
}

func (rs *restarter) handoffDone() <-chan struct{} {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !rs.inHandoff.Load() {
		return nil
	}
	return rs.resumed
}

// watchIdleTimeout bounds a watch connection opened while no restart is
// running (the UI opens the watch before POSTing, so the window is meant to
// be milliseconds - anything longer is a stale page and gets released).
const watchIdleTimeout = 60 * time.Second

// watcherChanBuf bounds one watch stream's event buffer. publish() drops
// rather than blocks (a slow dashboard must never stall the choreography),
// so the buffer only smooths brief bursts of phase/progress events. Internal
// fan-out guardrail, not user-tunable.
const watcherChanBuf = 16

// serveWatch streams the restart choreography as NDJSON on a hijacked
// connection. This is the only way a dashboard can observe the drain live:
// after the commit point the old server stops accepting, so a new poll
// connection sits unanswered in the inherited socket's backlog until the
// child boots - while an already-accepted, hijacked connection keeps
// streaming (hijacked conns are not tracked by http.Server.Shutdown, so the
// choreography never blocks on one). The response head is written raw after
// the hijack with Connection: close: the body is EOF-delimited, ending when
// the parent exits (success) or the registry closes (failure). Framing the
// body manually avoids the pre-hijack Flush, which would put the server's
// writer into chunked encoding the raw conn cannot maintain.
func (rs *restarter) serveWatch(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		rs.writeStatus(w) // degraded client (h2): plain JSON snapshot
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		rs.writeStatus(w)
		return
	}
	defer conn.Close()

	ch := make(chan map[string]any, watcherChanBuf)
	rs.addWatcher(ch)
	defer rs.removeWatcher(ch)
	writeEvent := func(ev map[string]any) bool {
		b, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		if _, err := rw.WriteString(string(b) + "\n"); err != nil {
			return false
		}
		return rw.Flush() == nil
	}
	const head = "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/x-ndjson\r\n" +
		"Cache-Control: no-store\r\n" +
		"X-Accel-Buffering: no\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := rw.WriteString(head); err != nil {
		return
	}
	if !writeEvent(rs.status()) { // initial snapshot so the UI can seed the stepper
		return
	}
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // registry closed: this run is over (failed/resumed)
			}
			if !writeEvent(ev) {
				return
			}
		case <-r.Context().Done():
			return
		case <-time.After(watchIdleTimeout):
			// A watch opened before POST should not linger if the POST never
			// comes. Once a run is in progress, a quiet stretch is a long
			// build (no events until drain) - heartbeat the current status
			// so the stream stays alive instead of dropping back to polls.
			st := rs.status()
			if phase, _ := st["phase"].(string); phase != "" && phase != "idle" {
				if !writeEvent(st) {
					return
				}
				continue
			}
			return
		}
	}
}

func (rs *restarter) writeStatus(w http.ResponseWriter) {
	// The status document describes the running process: never cacheable.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rs.status())
}

// handleRestart serves GET (status), GET ?watch=1 (NDJSON step stream) and
// POST (start) on /admin/restart.
func (rs *restarter) handleRestart(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("watch") != "" {
			rs.serveWatch(w, r)
			return
		}
		rs.writeStatus(w)
	case http.MethodPost:
		ok, why := rs.request()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":`+strconv.Quote("restart refused: "+why)+`}`, http.StatusConflict)
			return
		}
		// The full status document: the dashboard seeds its step tracker
		// (and the drain countdown band) straight from this response.
		rs.writeStatus(w)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"GET or POST only"}`, http.StatusMethodNotAllowed)
	}
}

// inheritedListener returns the listening socket passed by a restarting
// parent (env MILLIVOLT_LISTEN_FD=3). The child must bypass claimPort: the
// parent still holds the port while draining, and claimPort's exe-identity
// check would match and SIGTERM it.
func inheritedListener() (net.Listener, bool) {
	v := os.Getenv(envListenFD)
	if v == "" {
		return nil, false
	}
	fd, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("restart: invalid %s=%q", envListenFD, v)
	}
	f := os.NewFile(uintptr(fd), "inherited-listen")
	ln, err := net.FileListener(f)
	if err != nil {
		log.Fatalf("restart: inherited listener: %v", err)
	}
	f.Close() // FileListener owns a duplicate; do not leak the inherited FD.
	log.Printf("restart: inherited listening socket (fd %d); claimPort bypassed", fd)
	return ln, true
}

// signalReady tells the restarting parent this child is fully booted (config
// loaded, store open + totals scanned, ring backfilled, holds restored) and
// is about to serve.
func signalReady() {
	v := os.Getenv(envReadyFD)
	if v == "" {
		return
	}
	fd, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), "ready-pipe")
	if f == nil {
		return
	}
	fmt.Fprintf(f, "ok\n")
	f.Close()
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
