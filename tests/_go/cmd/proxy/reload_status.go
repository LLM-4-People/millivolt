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
