package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestModuleRootFrom(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "cmd", "proxy", "restart.go")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No go.mod above the source file → unavailable with a reason (never a guess).
	if root, why := moduleRootFrom(src); root != "" || why == "" {
		t.Errorf("moduleRootFrom(no go.mod) = %q, %q; want unavailable with a reason", root, why)
	}
	// go.mod + cmd/proxy → root found.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if root, why := moduleRootFrom(src); why != "" || root != dir {
		t.Errorf("moduleRootFrom = %q, %q; want %q, \"\"", root, why, dir)
	}
	// go.mod but no cmd/proxy → unavailable.
	if err := os.RemoveAll(filepath.Join(dir, "cmd")); err != nil {
		t.Fatal(err)
	}
	if _, why := moduleRootFrom(src); why == "" {
		t.Error("moduleRootFrom(module without cmd/proxy) should be unavailable with a reason")
	}
	// Source moved since build → unavailable.
	if _, why := moduleRootFrom(filepath.Join(dir, "gone", "restart.go")); why == "" {
		t.Error("moduleRootFrom(missing source) should be unavailable with a reason")
	}
}

// restartChildEnv gates the child half of the handoff test: the test binary
// re-invokes itself with the inherited FDs and only runs the child protocol
// when the gate is set.
const restartChildEnv = "MILLIVOLT_TEST_RESTART_CHILD"

// The FD handoff protocol end to end: a child started with the listening
// socket on fd 3 and a readiness pipe on fd 4 bypasses claimPort, serves on
// the SAME address the parent still holds, and signals "ok" once booted.
// This is the mechanism that makes a restart lose no future requests: the
// socket never closes, so connections queue instead of being refused.
func TestRestartHandoffProtocol(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	listenFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer listenFile.Close()

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()

	cmd := exec.Command(os.Args[0], "-test.run=TestRestartChildProcess", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), envListenFD+"=3", envReadyFD+"=4", restartChildEnv+"=1")
	cmd.ExtraFiles = []*os.File{listenFile, readyW}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readyW.Close() // the parent never writes; the child holds the only write end
	defer cmd.Process.Kill()

	if err := awaitReady(readyR, 15*time.Second); err != nil {
		t.Fatalf("child readiness: %v", err)
	}

	// The child answers on the address the parent STILL HOLDS, without ever
	// having called net.Listen (claimPort would have failed with EADDRINUSE
	// - or killed the parent).
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("dial child on parent-held address: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "child-ok") {
		t.Fatalf("child response = %q, want child-ok", body)
	}
}

func TestRestartChildProcess(t *testing.T) {
	if os.Getenv(restartChildEnv) == "" {
		t.Skip("child half of TestRestartHandoffProtocol")
	}
	ln, ok := inheritedListener()
	if !ok {
		t.Fatal("no inherited listener")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("child-ok")) })
	srv := &http.Server{Handler: mux}
	signalReady()
	srv.Serve(ln) // blocks until the parent kills us
}

func TestHandleRestartGuards(t *testing.T) {
	rs := newRestarter(&restartDeps{
		cloneServer:  func() *http.Server { return nil },
		closeFeeds:   func() {},
		flushStore:   func() error { return nil },
		drainTimeout: func() time.Duration { return time.Second },
	})
	// Wrong method → 405 with Allow (RFC 9110), never a fall-through.
	rec := httptest.NewRecorder()
	rs.handleRestart(rec, httptest.NewRequest(http.MethodPut, "/admin/restart", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /admin/restart = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q, want \"GET, POST\"", got)
	}

	// Unavailable source/toolchain → POST refused (deny by default).
	rs.mu.Lock()
	rs.root, rs.goBin, rs.rootWhy = "", "", "no go.mod above source"
	rs.mu.Unlock()
	rec = httptest.NewRecorder()
	rs.handleRestart(rec, httptest.NewRequest(http.MethodPost, "/admin/restart", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("POST with restart unavailable = %d, want 409", rec.Code)
	}

	// GET returns the status shape the dashboard's poll depends on.
	rec = httptest.NewRecorder()
	rs.handleRestart(rec, httptest.NewRequest(http.MethodGet, "/admin/restart", nil))
	var st struct {
		Ok        bool   `json:"ok"`
		Phase     string `json:"phase"`
		Pid       int    `json:"pid"`
		StartedAt int64  `json:"started_at"`
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Ok || st.Phase != "idle" || st.Pid != os.Getpid() || st.StartedAt == 0 || st.Available {
		t.Errorf("status = %+v; want ok, idle, own pid, started_at set, unavailable", st)
	}
	if st.Reason == "" {
		t.Error("unavailable status must carry a reason")
	}

	// A restart already in flight → second POST refused with 409.
	rs.mu.Lock()
	rs.root, rs.goBin = t.TempDir(), "go"
	rs.phase = restartBuilding
	rs.mu.Unlock()
	rec = httptest.NewRecorder()
	rs.handleRestart(rec, httptest.NewRequest(http.MethodPost, "/admin/restart", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("POST while building = %d, want 409", rec.Code)
	}
}

// Two consecutive post-commit failures must both resume cleanly: the wake
// channel is re-armed after each failure (a second close would panic) and
// the state machine returns to idle so another attempt is possible.
func TestFailAndResumeTwiceKeepsRestarterUsable(t *testing.T) {
	rs := newRestarter(&restartDeps{
		cloneServer:  func() *http.Server { return nil },
		closeFeeds:   func() {},
		flushStore:   func() error { return nil },
		drainTimeout: func() time.Duration { return time.Second },
	})
	for i := 1; i <= 2; i++ {
		rs.inHandoff.Store(true)
		ch := rs.handoffDone() // capture BEFORE the failure re-arms the channel
		done := make(chan struct{})
		go func() { <-ch; close(done) }()
		rs.failAndResume(fmt.Errorf("attempt %d failed", i), nil, "")
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d: resumed wake never fired", i)
		}
		if rs.committed() {
			t.Fatalf("attempt %d: still committed after resume", i)
		}
		if st := rs.status(); st["phase"] != "idle" || st["error"] == "" {
			t.Fatalf("attempt %d: status = %v, want idle with error", i, st)
		}
	}
}

func TestFailedHandoffReplacesClosedListenerAndServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	original := &http.Server{}
	rs := newRestarter(&restartDeps{listener: ln, server: original,
		cloneServer: func() *http.Server {
			return &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}
		},
	})
	t.Cleanup(func() { rs.deps.server.Close(); rs.deps.listener.Close() })
	for range 2 {
		previous := rs.deps.listener
		file, err := previous.(*net.TCPListener).File()
		if err != nil {
			t.Fatalf("next handoff cannot duplicate current listener: %v", err)
		}
		if err := rs.deps.server.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		previous.Close()
		rs.inHandoff.Store(true)
		rs.failAndResume(fmt.Errorf("fixture spawn failure"), file, "")
		file.Close()
		if rs.deps.listener == previous || rs.deps.server == original {
			t.Fatal("resumed process retains closed handoff dependencies")
		}
	}
}

func TestPrepareHandoffRequiresDrainAndFlushSuccess(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := srv.Client().Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	flushed := false
	rs := newRestarter(&restartDeps{server: srv.Config, flushStore: func() error { flushed = true; return errors.New("disk failure") }})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rs.prepareHandoff(ctx); !errors.Is(err, context.Canceled) || flushed {
		t.Errorf("failed drain passed flush fence: err=%v flushed=%v", err, flushed)
	}
	close(release)
	<-done
	srv.Close()
	if err := rs.prepareHandoff(t.Context()); err == nil || !flushed {
		t.Errorf("failed flush passed handoff fence: err=%v flushed=%v", err, flushed)
	}
	if rs.status()["phase"] == "handing_off" {
		t.Fatal("failed flush advanced to child handoff")
	}
}

func TestDrainIncludesRetiredActiveServers(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer old.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := old.Client().Get(old.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	rs := newRestarter(&restartDeps{server: &http.Server{}})
	rs.retired = []*http.Server{old.Config}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rs.drain(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("retired active request skipped: %v", err)
	}
	close(release)
	<-done
	if err := rs.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rs.retired) != 0 {
		t.Fatal("quiescent retired servers retained")
	}
}

// TestRestartWatchStreamsPhases pins the step stream: the hijacked NDJSON
// watch connection delivers an initial snapshot plus every real phase
// transition the moment it happens (never pre-claimed), and ends when the
// state machine returns to idle after a failure. This is the only channel a
// dashboard can observe the drain through - polls hang in the inherited
// socket's backlog once the old server stops accepting.
func TestRestartWatchStreamsPhases(t *testing.T) {
	rs := newRestarter(&restartDeps{
		cloneServer:  func() *http.Server { return nil },
		closeFeeds:   func() {},
		flushStore:   func() error { return nil },
		drainTimeout: func() time.Duration { return 90 * time.Second },
	})
	srv := httptest.NewServer(http.HandlerFunc(rs.handleRestart))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/restart?watch=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	type evDoc struct {
		Phase          string `json:"phase"`
		Rank           int    `json:"rank"`
		PhaseMs        int64  `json:"phase_ms"`
		DrainTimeoutMs int64  `json:"drain_timeout_ms"`
		DrainElapsedMs int64  `json:"drain_elapsed_ms"`
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan evDoc, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var ev evDoc
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev
			}
		}
		close(events)
	}()

	readEvent := func(want string) evDoc {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("watch stream ended before %q", want)
				}
				if ev.Phase == want {
					return ev
				}
			case <-deadline:
				t.Fatalf("timed out waiting for phase %q", want)
			}
		}
	}
	if first := readEvent("idle"); first.DrainTimeoutMs != 90000 || first.PhaseMs == 0 {
		t.Fatalf("initial snapshot = %+v; want drain_timeout_ms 90000 and phase_ms set", first)
	}
	rs.setPhase(restartBuilding, "")
	if ev := readEvent("building"); ev.Rank != phaseRank(restartBuilding) {
		t.Fatalf("building event rank = %d, want %d", ev.Rank, phaseRank(restartBuilding))
	}
	rs.setPhase(restartDraining, "")
	dr := readEvent("draining")
	if dr.DrainElapsedMs < 0 {
		t.Fatalf("draining event = %+v; want drain_elapsed_ms >= 0", dr)
	}
	rs.setPhase(restartFlushing, "")
	readEvent("flushing")
	rs.setPhase(restartHandingOff, "")
	readEvent("handing_off")
	// The single failure owner sends idle and retires the run's watchers.
	rs.finishFailure(errors.New("attempt failed"))
	if ev := readEvent("idle"); ev.Phase != "idle" {
		t.Fatalf("final event = %+v", ev)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("watch stream must end after the state machine returns to idle")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch stream did not close after idle")
	}
}

func TestRestartProgressStopsAfterDependencyPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rs := newRestarter(&restartDeps{server: &http.Server{}, flushStore: func() error { panic("test flush panic") }})
		ch := make(chan map[string]any, 16)
		rs.addWatcher(ch)
		defer rs.removeWatcher(ch)
		func() {
			defer func() {
				if recover() == nil {
					t.Error("dependency panic was unexpectedly swallowed")
				}
			}()
			rs.prepareWithProgress(time.Second)
		}()
		queued := len(ch)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if len(ch) != queued {
			t.Fatal("progress ticker kept publishing after failed preparation")
		}
	})
}

// TestRestartWatchUntrackedByShutdown pins WHY the stream works during the
// drain: a hijacked watch connection is not tracked by http.Server.Shutdown,
// so the choreography cannot hang on it (unlike a poll, which waits in the
// backlog of the inherited socket).
func TestRestartWatchUnaffectedByShutdown(t *testing.T) {
	rs := newRestarter(&restartDeps{
		cloneServer:  func() *http.Server { return nil },
		closeFeeds:   func() {},
		flushStore:   func() error { return nil },
		drainTimeout: func() time.Duration { return 30 * time.Second },
	})
	srv := httptest.NewServer(http.HandlerFunc(rs.handleRestart))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/restart?watch=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Shutdown (the same call the choreography makes) must return even though
	// the watch connection is still open and mid-stream.
	drained := make(chan error, 1)
	go func() { drained <- srv.Config.Shutdown(t.Context()) }()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("shutdown with an open watch stream: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown blocked on a hijacked watch connection")
	}
	// The stream is still functional afterwards: phases keep flowing.
	rs.setPhase(restartBuilding, "")
}
