package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStressTargetSafety(t *testing.T) {
	o := options{levels: "1,8", duration: time.Second, timeout: time.Second, sample: time.Millisecond, chunks: 1, upstreams: 1, maxRSS: 128}
	for _, target := range []string{"", "http://127.0.0.1:8080", "http://example.com:8081", "http://0.0.0.0:8081", "http://127.0.0.1:8081/admin/restart", "http://user@127.0.0.1:8081", "http://127.0.0.1:8081?q=x"} {
		o.target = target
		if _, _, err := validate(o); err == nil {
			t.Errorf("unsafe target accepted: %s", target)
		}
	}
	o.target = "http://127.0.0.1:8081"
	if _, levels, err := validate(o); err != nil || len(levels) != 2 {
		t.Fatalf("valid dev workload rejected: %v", err)
	}
}

func TestStressSourceSafety(t *testing.T) {
	target := net.ParseIP("127.0.0.1")
	for _, value := range []string{"", "127.0.0.1,127.0.0.2"} {
		if _, err := sourceIPs(value, target); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"0.0.0.0", "example.com", "127.0.0.1,127.0.0.1", "::1", "127.0.0.1,"} {
		if _, err := sourceIPs(value, target); err == nil {
			t.Fatalf("unsafe sources accepted: %s", value)
		}
	}
}

func TestStressMetadataCannotIncludeProviderSecrets(t *testing.T) {
	got := performanceSettings(map[string]any{"providers": map[string]any{"headers": "secret"}, "max_conns_per_host": 512})
	if len(got) != 1 || got["max_conns_per_host"] != 512 {
		t.Fatal(got)
	}
}

func TestStressFirstFailureStopsNewWork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	o := options{duration: time.Second, timeout: time.Second, sample: time.Millisecond, chunks: 1, maxRSS: 2048}
	client := localClient(o.timeout, &http.Transport{})
	defer client.CloseIdleConnections()
	res, err := stage(t.Context(), control{client, server.URL}, []*http.Client{client}, os.Getpid(), 100, o, []string{server.URL}, newFixture(o), 1, false, "stress-test")
	if err != nil || res.Requests != 1 || res.Failures != 1 || res.HTTPResponses != 1 || res.Successes != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestStressInitialRSSGuardStartsNoWork(t *testing.T) {
	o := options{maxRSS: .0001}
	res, err := stage(context.Background(), control{}, nil, os.Getpid(), 100, o, nil, newFixture(o), 1, false, "stress-test")
	if err == nil || res.Requests != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNearestRank(t *testing.T) {
	if got := percentileMs([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, int64(time.Second)}, 95); got != 1000 {
		t.Fatal(got)
	}
}

func TestStorageDropDeltaRequiresOneEnabledEpoch(t *testing.T) {
	before := storageState{FeedID: "same-process"}
	before.Storage.Enabled, before.Storage.Dropped = true, 7
	for _, tc := range []struct {
		name    string
		feed    string
		enabled bool
		dropped uint64
		want    uint64
		valid   bool
	}{
		{"no loss", "same-process", true, 7, 0, true},
		{"confirmed loss", "same-process", true, 12, 5, true},
		{"counter regression", "same-process", true, 6, 0, false},
		{"restart", "new-process", true, 12, 0, false},
		{"missing epoch", "", true, 12, 0, false},
		{"disabled storage", "same-process", false, 12, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := storageState{FeedID: tc.feed}
			after.Storage.Enabled, after.Storage.Dropped = tc.enabled, tc.dropped
			got, err := storageDropDelta(before, after)
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("delta=%d err=%v, want=%d valid=%v", got, err, tc.want, tc.valid)
			}
		})
	}
	before.Storage.Enabled = false
	if _, err := storageDropDelta(before, before); err == nil {
		t.Fatal("missing initial storage signal was accepted")
	}
}

// This inert process supplies only the PID-file command-line identity. It is
// not a proxy, and it exits when its owning test closes the stdin pipe.
func TestStressPIDFixture(t *testing.T) {
	if os.Getenv("MILLIVOLT_STRESS_PID_FIXTURE") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

func TestStressRunCleansOnlyOwnedClients(t *testing.T) {
	for _, mode := range []string{"ramp", "request failure", "canceled", "cleanup failure", "changed database"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			o := options{levels: "1,1", duration: 20 * time.Millisecond, timeout: 2 * time.Second,
				sample: time.Millisecond, chunks: 1, upstreams: 1, maxRSS: 2048}
			expected := newFixture(o).expected
			var mu sync.Mutex
			records := map[string]int{"unrelated-client": 1}
			var tags, purged []string
			var cfg settings
			var pid, configReads int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/admin/config":
					configReads++
					if mode == "changed database" && configReads > 1 {
						// A private-looking config path must not authorize a
						// purge after storage has changed to an unrelated file.
						cfg.Overrides["db_path"] = "/var/lib/production.db"
						cfg.Effective["db_path"] = "/var/lib/production.db"
					}
					_ = json.NewEncoder(w).Encode(cfg)
				case "/admin/restart":
					_ = json.NewEncoder(w).Encode(map[string]int{"pid": pid})
				case "/metrics/bootstrap":
					_, _ = io.WriteString(w, `{"feed_id":"fixture","in_flight_records":[],"storage":{"enabled":false}}`)
				case "/v1/chat/completions":
					tag := r.Header.Get("X-Proxy-Client")
					if !strings.HasPrefix(tag, "stress-") {
						t.Errorf("missing unique fixture scope: %q", tag)
					}
					if _, ok := records[tag]; !ok {
						tags = append(tags, tag)
					}
					records[tag]++
					if mode == "canceled" {
						cancel()
					}
					if mode != "ramp" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = io.WriteString(w, expected)
				case "/metrics/purge":
					if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
						t.Error("cleanup must use a JSON POST")
					}
					var filter map[string]string
					if err := json.NewDecoder(r.Body).Decode(&filter); err != nil || len(filter) != 1 || filter["client"] == "" {
						t.Errorf("cleanup broadened its filter: %v, %v", filter, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					tag := filter["client"]
					if _, ok := records[tag]; !ok {
						t.Errorf("cleanup selected an unowned client: %q", tag)
					}
					if mode == "ramp" && len(tags) != 2 {
						t.Error("cleanup changed the ramp's cumulative-history baseline")
					}
					purged = append(purged, tag)
					if mode == "cleanup failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"ok":false}`)
						return
					}
					delete(records, tag)
					_, _ = io.WriteString(w, `{"ok":true}`)
				default:
					t.Errorf("unexpected fixture endpoint: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			o.target = server.URL
			u, _ := url.Parse(server.URL)
			base := "/tmp/millivolt/millivolt-dev-" + u.Port()
			if err := os.MkdirAll(filepath.Dir(base), 0700); err != nil {
				t.Fatal(err)
			}
			// Exclusive creation and exact cleanup never replace a dev file.
			file, err := os.OpenFile(base+".yaml", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			file.Close()
			defer os.Remove(base + ".yaml")
			proc := exec.Command(os.Args[0], "-test.run=^TestStressPIDFixture$", "--", "-pid-file", base+".pid")
			proc.Env = append(os.Environ(), "MILLIVOLT_STRESS_PID_FIXTURE=1")
			stdin, err := proc.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := proc.Start(); err != nil {
				stdin.Close()
				t.Fatal(err)
			}
			defer func() {
				stdin.Close()
				if err := proc.Wait(); err != nil {
					t.Error(err)
				}
			}()
			pid = proc.Process.Pid
			cfg = settings{Path: base + ".yaml", Overrides: map[string]string{"listen": u.Host, "db_path": "none"}, Effective: map[string]any{"db_path": ""}}
			err = run(ctx, o)
			mu.Lock()
			defer mu.Unlock()
			if (err == nil) != (mode == "ramp") {
				t.Errorf("run error = %v for mode %q", err, mode)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Errorf("cleanup lost workload cancellation: %v", err)
			}
			if records["unrelated-client"] != 1 {
				t.Error("unrelated history changed")
			}
			switch mode {
			case "changed database":
				if len(purged) != 0 || err == nil || !strings.Contains(err.Error(), "cleanup incomplete") {
					t.Errorf("changed storage was not rejected: purged=%v error=%v", purged, err)
				}
			case "cleanup failure":
				if len(purged) != 1 || err == nil || !strings.Contains(err.Error(), "cleanup incomplete") {
					t.Errorf("cleanup failure was not reported: purged=%v error=%v", purged, err)
				}
			default:
				if len(tags) == 0 || len(purged) != len(tags) || len(records) != 1 {
					t.Errorf("fixture records remain: tags=%v purged=%v records=%v", tags, purged, records)
				}
			}
		})
	}
}

func TestStressDevDatabaseBoundary(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:18099")
	for _, tc := range []struct {
		override  string
		effective any
		pending   bool
		valid     bool
	}{
		{"none", "", false, true},
		{"/tmp/millivolt/millivolt-dev.db", "/tmp/millivolt/millivolt-dev.db", false, true},
		{"/tmp/millivolt/millivolt-dev-run.db", "/tmp/millivolt/millivolt-dev-run.db", false, true},
		{"", "", false, false},
		{"none", nil, false, false},
		{"none", "/var/lib/production.db", false, false},
		{"/var/lib/production.db", "/var/lib/production.db", false, false},
		{"/tmp/millivolt/millivolt-dev.db", "/var/lib/production.db", false, false},
		{"/tmp/millivolt/millivolt-dev/other.db", "/tmp/millivolt/millivolt-dev/other.db", false, false},
		{"/tmp/millivolt/../millivolt/millivolt-dev.db", "/tmp/millivolt/../millivolt/millivolt-dev.db", false, false},
		{"none", "", true, false},
	} {
		cfg := settings{Path: "/tmp/millivolt/millivolt-dev-18099.yaml", Overrides: map[string]string{"listen": u.Host, "db_path": tc.override}, Effective: map[string]any{"db_path": tc.effective}}
		if tc.pending {
			cfg.RestartRequired = []string{"db_path"}
		}
		if err := validateDevSettings(cfg, u); (err == nil) != tc.valid {
			t.Errorf("override=%q effective=%v pending=%v: %v", tc.override, tc.effective, tc.pending, err)
		}
	}
}

func TestStressPrivateFileRejectsAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := privateFile(path); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := privateFile(alias); err == nil {
		t.Error("symlink accepted")
	}
	if err := os.Link(path, filepath.Join(dir, "hard.db")); err != nil {
		t.Fatal(err)
	}
	if err := privateFile(path); err == nil {
		t.Error("hard-linked database accepted")
	}
	if err := privateFile(dir); err == nil {
		t.Error("directory accepted")
	}
	if err := privateFile(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestStressCleanupWaitsForOwnedPendingOnly(t *testing.T) {
	for _, pending := range []string{`[{"client":"fixture"}]`, `[{"client":"other"}]`, "drained", "missing", "malformed", "identity changed"} {
		t.Run(pending, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, purges := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metrics/purge" {
					purges++
					_, _ = io.WriteString(w, `{"ok":true}`)
					return
				}
				calls++
				switch pending {
				case "missing":
					_, _ = io.WriteString(w, `{"feed_id":"fixture"}`)
				case "malformed":
					_, _ = io.WriteString(w, `{"feed_id":"fixture","in_flight_records":{}}`)
				case "identity changed":
					_, _ = io.WriteString(w, `{"feed_id":"fixture","in_flight_records":[]}`)
				case "drained":
					if calls == 1 {
						_, _ = io.WriteString(w, `{"feed_id":"fixture","in_flight_records":[{"client":"fixture"}]}`)
					} else {
						_, _ = io.WriteString(w, `{"feed_id":"fixture","in_flight_records":[]}`)
					}
				default:
					if pending == `[{"client":"fixture"}]` {
						cancel()
					}
					_, _ = io.WriteString(w, `{"feed_id":"fixture","in_flight_records":`+pending+`}`)
				}
			}))
			defer server.Close()
			client := localClient(time.Second, &http.Transport{})
			defer client.CloseIdleConnections()
			c := control{client, server.URL}
			verify := func(context.Context) error {
				if pending == "identity changed" {
					return errors.New("identity changed")
				}
				return nil
			}
			err := c.clearClients(ctx, []string{"fixture"}, time.Millisecond, verify)
			if pending == `[{"client":"other"}]` || pending == "drained" {
				if err != nil || purges != 1 {
					t.Fatalf("unrelated pending blocked cleanup: %v, %d", err, purges)
				}
			} else if err == nil || purges != 0 {
				t.Fatalf("unsafe pending state allowed cleanup: %v, %d", err, purges)
			}
			if err := c.clearClients(t.Context(), []string{""}, time.Millisecond, verify); err == nil {
				t.Error("empty client accepted")
			}
			wantCalls := 1
			if pending == "drained" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Errorf("empty-client rejection sent a request: %d", calls)
			}
		})
	}
}
