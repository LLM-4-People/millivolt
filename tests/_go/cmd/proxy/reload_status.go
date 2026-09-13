package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
)

// liveReloadFixture swaps the package-level reload state for a scratch
// config file, mirroring TestReloadRepairsDroppedKeys, and restores it after.
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
