package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func TestBackupRestoreConfigRoundTrip(t *testing.T) {
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
	start := liveCfg.Clone()
	start.MaxRetries = 9
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?config=1", nil))
	if rr.Code != 200 {
		t.Fatalf("backup status %d body %s", rr.Code, rr.Body.String())
	}
	raw := rr.Body.Bytes()
	arch, err := backup.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Validate(arch); err != nil {
		t.Fatal(err)
	}
	if len(arch.Database) != 0 || len(arch.Config) == 0 {
		t.Fatal("config-only archive had the wrong members")
	}

	changed := liveCfg.Clone()
	changed.MaxRetries = 2
	if err := config.WriteFile(liveConfigPath, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	if liveCfg.MaxRetries != 2 {
		t.Fatal("setup did not change retries")
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/restore?config=1", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("restore status %d body %s", rr.Code, rr.Body.String())
	}
	if liveCfg.MaxRetries != 9 {
		t.Fatalf("restore max_retries=%d, want 9", liveCfg.MaxRetries)
	}
}

func TestRestoreRejectsCorruptArchive(t *testing.T) {
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?config=1", bytes.NewReader([]byte("not a backup")))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

func TestBackupRequiresAPart(t *testing.T) {
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?config=0&database=0", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

func TestStageSnapshotIsAdmittedOnOpen(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := storage.Open(srcPath, storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  config.Default().StorageQueryMaxRows,
	})
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "keep-me", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	src.Close()
	dstPath := filepath.Join(dir, "dst.db")
	if err := storage.StageSnapshot(dstPath, data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(dstPath)); err != nil {
		t.Fatal(err)
	}
	dst, err := storage.Open(dstPath, storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  config.Default().StorageQueryMaxRows,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := os.Stat(storage.PendingSnapshotPath(dstPath)); !os.IsNotExist(err) {
		t.Fatal("pending snapshot was not consumed")
	}
	got, err := dst.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep-me" {
		t.Fatalf("restored rows = %#v", got)
	}
}

func TestBackupStatusJSON(t *testing.T) {
	oldCfg, oldStore, oldPath := liveCfg, liveStore, liveConfigPath
	t.Cleanup(func() { liveCfg, liveStore, liveConfigPath = oldCfg, oldStore, oldPath })
	liveCfg = config.Default()
	liveStore = nil
	liveConfigPath = filepath.Join(t.TempDir(), "c.yaml")
	st := backupStatus()
	if st["config"] != true || st["database"] != false {
		t.Fatalf("%v", st)
	}
}
