package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// liveReloadFixture swaps the package-level reload state for a scratch
// config file and restores it after. It is the shared fixture owner for
// every test that drives reloadConfig or serves the /admin/config document
// (reload status, config reload, backup/restore), not a mirror of one test.
func liveReloadFixture(t *testing.T) {
	t.Helper()
	oldCfg, oldBoot, oldProxy, oldBuf, oldStore := liveCfg, bootCfg, liveProxy, liveBuf, liveStore
	oldPath, oldListen, oldDB := liveConfigPath, liveListenOverride, liveDBOverride
	t.Cleanup(func() {
		liveCfg, bootCfg, liveProxy, liveBuf, liveStore = oldCfg, oldBoot, oldProxy, oldBuf, oldStore
		liveConfigPath, liveListenOverride, liveDBOverride = oldPath, oldListen, oldDB
	})
	liveCfg = config.Default()
	bootCfg = liveCfg.Clone()
	liveBuf = metrics.NewBuffer(liveCfg.HistorySize)
	liveProxy = proxy.New(liveCfg, liveBuf)
	liveStore = nil
	liveListenOverride, liveDBOverride = "", ""
	liveConfigPath = filepath.Join(t.TempDir(), "config.yaml")
}

// TestAdminConfigReportsLastReload drives one real reload through
// reloadConfig, then asserts the /admin/config document carries the
// outcome under last_reload.
func TestAdminConfigReportsLastReload(t *testing.T) {
	liveReloadFixture(t)
	// One hot-reloadable change, one dropped key, one startup-bound change:
	// all three outcomes must be visible in the state document.
	raw := "max_retries: 3\nmax_retriez: 9\nhistory_size: " +
		strconv.Itoa(config.Default().HistorySize+1) + "\n"
	if err := os.WriteFile(liveConfigPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	// The shared constructor is the same wiring main serves /admin/config
	// with, so a dropped ReloadStatus field fails here too.
	h := adminConfigHandler()
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d %s", rr.Code, rr.Body.Bytes())
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var last struct {
		OK              bool     `json:"ok"`
		Error           string   `json:"error"`
		DroppedKeys     []string `json:"dropped_keys"`
		RestartRequired []string `json:"restart_required"`
		At              int64    `json:"at"`
	}
	if err := json.Unmarshal(doc["last_reload"], &last); err != nil {
		t.Fatalf("/admin/config carries no last_reload section: %v", err)
	}
	if !last.OK || last.Error != "" {
		t.Fatalf("last_reload = %+v, want a successful reload", last)
	}
	if len(last.DroppedKeys) != 1 || last.DroppedKeys[0] != "max_retriez" {
		t.Fatalf("dropped_keys = %v, want [max_retriez]", last.DroppedKeys)
	}
	if len(last.RestartRequired) != 1 || last.RestartRequired[0] != "history_size" {
		t.Fatalf("restart_required = %v, want [history_size]", last.RestartRequired)
	}
	if last.At <= 0 {
		t.Fatalf("at = %d, want a unix-millisecond timestamp", last.At)
	}
}

// TestAdminConfigOmitsLastReloadWhenAbsent pins the nil guard: a Handler
// whose status provider returns nil omits the section entirely.
func TestAdminConfigOmitsLastReloadWhenAbsent(t *testing.T) {
	h := &config.Handler{
		ReloadStatus: func() any { return nil },
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d %s", rr.Code, rr.Body.Bytes())
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["last_reload"]; ok {
		t.Fatal("last_reload must be omitted when the status callback returns nil")
	}
}

// lastReloadFrom serves GET /admin/config through the same constructor main
// wires and decodes the last_reload section.
func lastReloadFrom(t *testing.T) (ok bool, errMsg string, dropped []string, at int64) {
	t.Helper()
	rr := httptest.NewRecorder()
	adminConfigHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d %s", rr.Code, rr.Body.Bytes())
	}
	var doc struct {
		LastReload struct {
			OK          bool     `json:"ok"`
			Error       string   `json:"error"`
			DroppedKeys []string `json:"dropped_keys"`
			At          int64    `json:"at"`
		} `json:"last_reload"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("/admin/config carries no last_reload section: %v", err)
	}
	return doc.LastReload.OK, doc.LastReload.Error, doc.LastReload.DroppedKeys, doc.LastReload.At
}

// TestReloadFailureRecordedLoadError pins the load-error branch of
// reloadConfig: an unreadable config path fails the reload AND publishes
// ok:false with the wrapped "read config" cause in the mutable last_reload
// slot (the single structured view SIGHUP, POST /admin/reload, Settings saves
// and backup restores all share; the process log is the history).
func TestReloadFailureRecordedLoadError(t *testing.T) {
	liveReloadFixture(t)
	// A directory where the config file should be: the load fails at the
	// read (not a parse error, so LoadFileRepair performs no repair).
	if err := os.Mkdir(liveConfigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err == nil {
		t.Fatal("reload against a directory config path: want a load error")
	}
	// The GET must serve: remove the directory and write a valid config so
	// the handler's own file read works, while last_reload still shows the
	// failure outcome.
	if err := os.Remove(liveConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteFile(liveConfigPath, config.Default()); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, dropped, at := lastReloadFrom(t)
	if ok {
		t.Fatal("last_reload.ok = true after a failed reload, want false")
	}
	if !strings.Contains(errMsg, "read config") {
		t.Fatalf("last_reload.error = %q, want the wrapped read-config cause", errMsg)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped_keys = %v, want none on a load failure", dropped)
	}
	if at <= 0 {
		t.Fatalf("last_reload.at = %d, want a unix-millisecond timestamp", at)
	}
}

// TestReloadFailureRecordedRenameError pins the provider-aliases rename
// branch: a config that changes provider_aliases while the live store is
// closed must fail the reload with the wrapped "provider_aliases" cause,
// keep the current config, and retain the load step's dropped keys in the
// failure record (they were already reported; the rename failure must not
// erase them).
func TestReloadFailureRecordedRenameError(t *testing.T) {
	liveReloadFixture(t)
	// A closed store on a scratch path: RenameProviders must fail on it.
	oldStore := liveStore
	t.Cleanup(func() { liveStore = oldStore })
	d := config.Default()
	closed, err := storage.Open(filepath.Join(t.TempDir(), "closed.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
		QueryMaxBytes: int(d.StorageQueryMaxBytes),
		QueryMaxRows:  d.StorageQueryMaxRows,
	})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	liveStore = closed

	raw := "max_retries: 3\nmax_retriez: 9\nprovider_aliases:\n  old.example: new.example\n"
	if err := os.WriteFile(liveConfigPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err == nil {
		t.Fatal("reload with a closed live store and changed aliases: want a rename error")
	}
	ok, errMsg, dropped, at := lastReloadFrom(t)
	if ok {
		t.Fatal("last_reload.ok = true after a failed rename, want false")
	}
	if !strings.Contains(errMsg, "provider_aliases") {
		t.Fatalf("last_reload.error = %q, want the wrapped provider_aliases cause", errMsg)
	}
	if len(dropped) != 1 || dropped[0] != "max_retriez" {
		t.Fatalf("dropped_keys = %v, want the pre-failure [max_retriez] retained", dropped)
	}
	if at <= 0 {
		t.Fatalf("last_reload.at = %d, want a unix-millisecond timestamp", at)
	}
}

// TestAdminReloadEndpointShape drives the real POST /admin/reload handler
// (the reloadHandler constructor main wires, the R5 lost finding: reload
// plumbing was pinned but never the endpoint's response contract). The
// success row reuses the last_reload fixture's config (one hot-applied
// field, one dropped key, one startup-bound change) so the body bytes are
// deterministic; the failure row reuses the load-error induction (a
// directory where the config file should be).
func TestAdminReloadEndpointShape(t *testing.T) {
	post := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		reloadHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/reload", nil))
		return w
	}

	t.Run("success", func(t *testing.T) {
		liveReloadFixture(t)
		raw := "max_retries: 3\nmax_retriez: 9\nhistory_size: " +
			strconv.Itoa(config.Default().HistorySize+1) + "\n"
		if err := os.WriteFile(liveConfigPath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		w := post(t)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /admin/reload = %d %s, want 200", w.Code, w.Body.Bytes())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		// The success path never sends nosniff (only the WriteError failure
		// transport does, via http.Error) - the recorded position per row.
		if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "" {
			t.Errorf("X-Content-Type-Options = %q, want unset on success", xcto)
		}
		if got, want := w.Body.String(), `{"ok":true,"restart_required":["history_size"]}`; got != want {
			t.Errorf("success body = %q, want %q", got, want)
		}
	})

	t.Run("load failure", func(t *testing.T) {
		liveReloadFixture(t)
		// A directory where the config file should be: the load fails at
		// the read, the endpoint keeps the running config and answers with
		// the flat operator error body. The failure transport is
		// adminjson.WriteError (http.Error), whose Go 1.26 contract resets
		// Content-Type to text/plain even though the handler declared
		// application/json first - the body stays the flat JSON error
		// either way. This declare-JSON-then-WriteError reset is the whole
		// admin mutation family's contract, not a reload quirk: ~40 pinned
		// sites across the operator plane answer the same way, with
		// config/admin.go's settings handler the closest twin, and
		// adminjson.WriteErrorJSON is reserved for the surfaces that
		// answered application/json before W13 plus the /healthz method
		// gate. Changing the family is one owner-wide decision at
		// internal/adminjson (all pinned contracts change together),
		// recorded with the nosniff record-accept in the wave-6 L3
		// reload-transport decision - a reload-only switch would be the
		// per-caller patch that decision declined.
		if err := os.Mkdir(liveConfigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		w := post(t)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("POST /admin/reload against a directory config path = %d %s, want 400", w.Code, w.Body.Bytes())
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type = %q, want http.Error's text/plain reset (observed transport)", ct)
		}
		// WriteError goes through http.Error, which always sets nosniff -
		// the recorded position, asserted so the transport cannot drift.
		if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff (http.Error's contract)", xcto)
		}
		var doc struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("failure body is not the flat error JSON: %v (%q)", err, w.Body.String())
		}
		if !strings.Contains(doc.Error, "read config") {
			t.Errorf("failure error = %q, want the wrapped read-config cause", doc.Error)
		}
		if !strings.HasSuffix(w.Body.String(), "\n") {
			t.Errorf("failure body %q lost the transport's trailing newline", w.Body.String())
		}
	})

	t.Run("method gate", func(t *testing.T) {
		liveReloadFixture(t)
		w := httptest.NewRecorder()
		reloadHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/reload", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET /admin/reload = %d %s, want 405", w.Code, w.Body.Bytes())
		}
		if allow := w.Header().Get("Allow"); allow != http.MethodPost {
			t.Errorf("Allow = %q, want POST", allow)
		}
		if got, want := w.Body.String(), "{\"error\":\"POST only\"}\n"; got != want {
			t.Errorf("method-gate body = %q, want %q", got, want)
		}
	})
}

// TestReloadStatusConcurrentRecordAndRead pins the mutex contract of the
// reload-status pair under -race: concurrent recordReloadStatus writers and
// lastReloadDoc readers must be safe, and once the writers finish a recorded
// final state must read back exactly (the mutable single slot always serves
// the most recent application outcome).
func TestReloadStatusConcurrentRecordAndRead(t *testing.T) {
	// Seed the slot so every concurrent read sees a live section (the
	// pre-first-application nil guard is pinned separately).
	recordReloadStatus(false, "seed", nil, nil)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			for j := 0; j < 500; j++ {
				recordReloadStatus(j%2 == 0, fmt.Sprintf("iteration %d-%d", i, j), []string{"dropped"}, []string{"restart"})
				if _, ok := lastReloadDoc().(map[string]any); !ok {
					panic("lastReloadDoc lost its map shape")
				}
			}
		})
	}
	wg.Wait()
	recordReloadStatus(true, "final state", []string{"final-dropped"}, []string{"final-restart"})
	doc, ok := lastReloadDoc().(map[string]any)
	if !ok {
		t.Fatalf("lastReloadDoc = %T, want the state map", lastReloadDoc())
	}
	if doc["ok"] != true || doc["error"] != "final state" {
		t.Fatalf("final state = %v, want ok=true error=%q", doc, "final state")
	}
	if fmt.Sprint(doc["dropped_keys"]) != "[final-dropped]" || fmt.Sprint(doc["restart_required"]) != "[final-restart]" {
		t.Fatalf("final state lists = %v / %v, want the recorded values",
			doc["dropped_keys"], doc["restart_required"])
	}
}
