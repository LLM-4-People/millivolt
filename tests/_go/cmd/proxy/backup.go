package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

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

func TestRestoreValidatesWholeArchiveBeforeDatabaseApply(t *testing.T) {
	oldCfg, oldStore := liveCfg, liveStore
	t.Cleanup(func() { liveCfg, liveStore = oldCfg, oldStore })
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "live.db")
	opts := storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  config.Default().StorageQueryMaxRows,
	}
	src, err := storage.Open(dbPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw := decodeableArchive(t, 1|2, []byte{1, 2}, []byte("listen: 8080\n"), data)
	decoded, err := backup.Decode(raw)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	if err := backup.Validate(decoded); err == nil {
		t.Fatal("fixture must fail Validate")
	}
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = src
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?database=1", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s, want 400 from Validate before staging", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(dbPath)); !os.IsNotExist(err) {
		t.Fatal("invalid archive staged a database snapshot")
	}
}

func TestRestoreStagesDatabaseFromEncodedArchive(t *testing.T) {
	oldCfg, oldStore := liveCfg, liveStore
	t.Cleanup(func() { liveCfg, liveStore = oldCfg, oldStore })
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "live.db")
	opts := storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  config.Default().StorageQueryMaxRows,
	}
	src, err := storage.Open(dbPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	src.Record(&metrics.Record{ID: "via-http", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(backup.Archive{Database: data})
	if err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = src
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?database=1", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("restore status %d body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		OK              bool     `json:"ok"`
		RestartRequired []string `json:"restart_required"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || len(body.RestartRequired) != 1 || body.RestartRequired[0] != "db_path" {
		t.Fatalf("restore json %+v", body)
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(dbPath)); err != nil {
		t.Fatalf("handleRestore did not stage the snapshot: %v", err)
	}
}

func decodeableArchive(t *testing.T, flags byte, kinds []byte, members ...[]byte) []byte {
	t.Helper()
	var payload bytes.Buffer
	for i, data := range members {
		if err := payload.WriteByte(kinds[i]); err != nil {
			t.Fatal(err)
		}
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(data)))
		if _, err := payload.Write(n[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := payload.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(payload.Bytes())
	var zbuf bytes.Buffer
	enc, err := zstd.NewWriter(&zbuf, zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	out := append([]byte("MVB1"), 1, flags, 0, 0)
	out = binary.BigEndian.AppendUint64(out, 0)
	out = append(out, sum[:]...)
	return append(out, zbuf.Bytes()...)
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

func TestRestoreInspectAndConfigMerge(t *testing.T) {
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
	live := config.Default()
	live.MaxRetries = 9
	if err := config.WriteFile(liveConfigPath, live); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	bak := config.Default()
	bak.CaptureBodyPreview = true
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, bak); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(backup.Archive{Config: yaml.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("inspect status %d body %s", rr.Code, rr.Body.String())
	}
	var ins struct {
		OK     bool `json:"ok"`
		Config struct {
			Present  bool           `json:"present"`
			Modified []string       `json:"modified"`
			Values   map[string]any `json:"values"`
		} `json:"config"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&ins); err != nil {
		t.Fatal(err)
	}
	if !ins.OK || !ins.Config.Present {
		t.Fatalf("inspect %+v", ins)
	}
	found := false
	for _, k := range ins.Config.Modified {
		if k == "capture_body_preview" {
			found = true
		}
	}
	if !found {
		t.Fatalf("modified = %v, want capture_body_preview", ins.Config.Modified)
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&config_mode=merge", bytes.NewReader(raw))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("merge status %d body %s", rr.Code, rr.Body.String())
	}
	got, err := config.LoadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 9 {
		t.Fatalf("merge overwrote live max_retries=%d", got.MaxRetries)
	}
	if !got.CaptureBodyPreview {
		t.Fatal("merge did not apply modified capture_body_preview")
	}
}

func TestRestoreDatabaseMergeKeepsLiveRows(t *testing.T) {
	oldCfg, oldStore := liveCfg, liveStore
	t.Cleanup(func() { liveCfg, liveStore = oldCfg, oldStore })
	dir := t.TempDir()
	opts := storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  config.Default().StorageQueryMaxRows,
	}
	livePath := filepath.Join(dir, "live.db")
	live, err := storage.Open(livePath, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	live.Record(&metrics.Record{ID: "live-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "src.db")
	src, err := storage.Open(srcPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "backup-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(backup.Archive{Database: data})
	if err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = livePath
	liveStore = live
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?database=1&database_mode=merge", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("merge status %d body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		OK              bool     `json:"ok"`
		RestartRequired []string `json:"restart_required"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || len(body.RestartRequired) != 0 {
		t.Fatalf("merge json %+v", body)
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(livePath)); !os.IsNotExist(err) {
		t.Fatal("merge staged a replace snapshot")
	}
	got, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids["live-row"] || !ids["backup-row"] {
		t.Fatalf("merged rows = %#v", got)
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
