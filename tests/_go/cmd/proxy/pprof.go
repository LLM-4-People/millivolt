package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClampProfileSeconds pins the capture-window bound: pprof accepts any
// whole-second value, so an operator (or a script) could hold a connection and
// the one CPU-profiler slot far past the delivery write bound. The table covers
// the absent parameter, pprof's own default, an in-range request, an oversized
// one, a zero and negative one, and a non-positive bound.
func TestClampProfileSeconds(t *testing.T) {
	const bound = 10 * time.Minute
	for _, row := range []struct {
		name string
		raw  string
		want int
	}{
		{"absent keeps the default", "", defaultProfileSeconds},
		{"in range is preserved", "45", 45},
		{"oversized is clamped to the bound", "86400", 600},
		{"zero keeps the default", "0", defaultProfileSeconds},
		{"negative keeps the default", "-5", defaultProfileSeconds},
		{"garbage keeps the default", "2m", defaultProfileSeconds},
		{"the bound itself is allowed", "600", 600},
	} {
		if got := profileSeconds(row.raw, bound); got != row.want {
			t.Errorf("%s: profileSeconds(%q) = %d, want %d", row.name, row.raw, got, row.want)
		}
	}
	// A bound below one second still yields a usable capture window instead of
	// a zero (pprof reads 0 as "use 30").
	if got := profileSeconds("60", 500*time.Millisecond); got != 1 {
		t.Errorf("sub-second bound: profileSeconds = %d, want 1", got)
	}
}

// TestClampProfileSecondsRewritesTheQuery pins that the wrapper hands the
// clamped value to pprof itself, not only to a header: the mounted handler is
// reached with the rewritten query and the delivery path keeps working.
func TestClampProfileSecondsRewritesTheQuery(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.FormValue("seconds")
		w.WriteHeader(http.StatusOK)
	})
	handler := clampProfileSeconds(next, func() time.Duration { return time.Minute })

	req := httptest.NewRequest(http.MethodGet, "/metrics/pprof/profile?seconds=3600", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if seen != "60" {
		t.Fatalf("handler saw seconds=%q, want 60", seen)
	}

	// An absent parameter still reaches the handler as an explicit value.
	seen = ""
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/pprof/profile", nil))
	if seen != "30" {
		t.Fatalf("absent parameter reached the handler as %q, want the default 30", seen)
	}
}

// pprofFixture mounts the pprof subtree behind the real gate. The gate is the
// production wrapper, not a stub, so placement is part of the observation.
func pprofFixture(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	const token = "pprof-fixture-credential"
	mux := http.NewServeMux()
	registerPprofRoutes(mux, func() time.Duration { return 2 * time.Minute })
	srv := httptest.NewServer(protectOperatorRequests(mux, newOperatorGate(token)))
	t.Cleanup(srv.Close)
	return srv, token
}

func pprofGet(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestPprofGateAndDisclosure pins that the surface sits behind the one
// operator credential and that each mounted name answers its own profile: the
// index lists the names, the protobuf profiles are gzip, and the debug=1
// renders name their subject. An unregistered subpath is a 404, never the
// index again.
func TestPprofGateAndDisclosure(t *testing.T) {
	srv, token := pprofFixture(t)
	for _, path := range []string{"/metrics/pprof/", "/metrics/pprof/heap", "/metrics/pprof/goroutine"} {
		resp := pprofGet(t, srv.URL+path, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a credential = %d, want 401", path, resp.StatusCode)
		}
	}

	index := pprofGet(t, srv.URL+"/metrics/pprof/", token)
	body, _ := io.ReadAll(index.Body)
	index.Body.Close()
	if index.StatusCode != http.StatusOK || !strings.Contains(string(body), "heap") {
		t.Fatalf("index = %d, body %q", index.StatusCode, body[:min(len(body), 120)])
	}

	for _, name := range []string{"allocs", "block", "heap", "mutex", "threadcreate"} {
		resp := pprofGet(t, srv.URL+"/metrics/pprof/"+name, token)
		head := make([]byte, 2)
		_, _ = io.ReadFull(resp.Body, head)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || head[0] != 0x1f || head[1] != 0x8b {
			t.Errorf("%s = %d %x, want 200 gzip", name, resp.StatusCode, head)
		}
	}

	for name, want := range map[string]string{"goroutine": "goroutine", "heap": "heap profile"} {
		resp := pprofGet(t, srv.URL+"/metrics/pprof/"+name+"?debug=1", token)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("%s?debug=1 = %d, body does not name %q", name, resp.StatusCode, want)
		}
	}

	resp := pprofGet(t, srv.URL+"/metrics/pprof/not-a-profile", token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unregistered subpath = %d, want 404", resp.StatusCode)
	}
}

// TestPprofIsGetOnly pins the shared method gate on every mounted handler: a
// wrong method must be a 405 with the method named, not the profile.
func TestPprofIsGetOnly(t *testing.T) {
	srv, token := pprofFixture(t)
	for _, path := range []string{"/metrics/pprof/", "/metrics/pprof/profile", "/metrics/pprof/heap"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
			t.Errorf("POST %s = %d Allow=%q, want 405 GET", path, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
}

// TestPprofOneCaptureAtATime pins the process-wide CPU profiler slot: while
// one profile runs, a second is refused instead of silently interleaving.
func TestPprofOneCaptureAtATime(t *testing.T) {
	srv, token := pprofFixture(t)
	first := make(chan *http.Response, 1)
	go func() { first <- pprofGet(t, srv.URL+"/metrics/pprof/profile?seconds=3", token) }()

	// The profiler must already be running when the second request arrives,
	// or the second would simply be the first. Poll until it refuses.
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		resp := pprofGet(t, srv.URL+"/metrics/pprof/profile?seconds=1", token)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusInternalServerError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a second concurrent profile was never refused with 500")
		}
	}
	resp := <-first
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first profile = %d, want 200", resp.StatusCode)
	}
}
