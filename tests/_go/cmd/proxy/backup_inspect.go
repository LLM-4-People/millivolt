package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

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

// restoreInspectWire is the strict decoder for the POST /admin/restore
// ?inspect=1 document. The W15 drop removed the flag-echo key "inspect"
// (no consumer); a re-added or renamed key must redden here instead of
// passing the lenient reads, as the round-seven mutation round proved it
// would. Both member shapes (present or absent) decode through the one
// struct: absent sections simply stay zero.
type restoreInspectWire struct {
	OK      bool   `json:"ok"`
	Created string `json:"created"`
	Config  struct {
		Present  bool           `json:"present"`
		Bytes    int            `json:"bytes"`
		Values   map[string]any `json:"values"`
		Modified []string       `json:"modified"`
		VsLive   []string       `json:"vs_live"`
	} `json:"config"`
	Database struct {
		Present     bool   `json:"present"`
		Bytes       int    `json:"bytes"`
		Requests    int64  `json:"requests"`
		Debug       int64  `json:"debug"`
		OldestMs    int64  `json:"oldest_ms"`
		NewestMs    int64  `json:"newest_ms"`
		Overlap     int64  `json:"overlap"`
		Inspectable *bool  `json:"inspectable"`
		Reason      string `json:"reason"`
	} `json:"database"`
}

func decodeRestoreInspectStrict(t *testing.T, body []byte) restoreInspectWire {
	t.Helper()
	var ins restoreInspectWire
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ins); err != nil {
		t.Fatalf("strict decode of the restore-inspect document: %v (%s)", err, body)
	}
	return ins
}

func TestRestoreInspectAndConfigMerge(t *testing.T) {
	liveReloadFixture(t)
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
	raw, err := backup.Encode("", backup.Archive{Config: yaml.Bytes()})
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
	ins := decodeRestoreInspectStrict(t, rr.Body.Bytes())
	if ins.Database.Present || ins.Database.Bytes != 0 || ins.Database.Requests != 0 {
		t.Fatalf("config-only inspect reported database members: %+v", ins.Database)
	}
	if !ins.OK || !ins.Config.Present || ins.Created == "" || ins.Config.Bytes == 0 {
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
	if ins.Config.Values["capture_body_preview"] != true {
		t.Fatalf("values = %#v, want capture_body_preview true", ins.Config.Values)
	}
	vsLive := map[string]bool{}
	for _, k := range ins.Config.VsLive {
		vsLive[k] = true
	}
	if !vsLive["capture_body_preview"] || !vsLive["max_retries"] {
		t.Fatalf("vs_live = %v, want capture_body_preview and max_retries", ins.Config.VsLive)
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

func TestRestoreInspectDatabase(t *testing.T) {
	live, opts, livePath, dir := openLiveStoreFixture(t)
	oldAt := time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC)
	newAt := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200, Start: oldAt})
	live.Record(&metrics.Record{ID: "live-only", Provider: "live.example", Model: "n", StatusCode: 200, Start: oldAt})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src, err := storage.Open(filepath.Join(dir, "src.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "shared", Provider: "backup.example", Model: "n", StatusCode: 200, Start: oldAt})
	src.Record(&metrics.Record{ID: "backup-only", Provider: "backup.example", Model: "n", StatusCode: 200, Start: newAt})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	raw, err := backup.Encode(dir, backup.Archive{Created: created, Database: data})
	if err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = livePath
	liveStore = live
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("inspect status %d body %s", rr.Code, rr.Body.String())
	}
	ins := decodeRestoreInspectStrict(t, rr.Body.Bytes())
	if ins.Config.Present || ins.Config.Bytes != 0 || len(ins.Config.Values) != 0 {
		t.Fatalf("database-only inspect reported config members: %+v", ins.Config)
	}
	if !ins.OK || !ins.Database.Present || ins.Database.Bytes == 0 {
		t.Fatalf("inspect %+v", ins)
	}
	if ins.Created != created.UTC().Format(time.RFC3339) {
		t.Fatalf("created = %q", ins.Created)
	}
	if ins.Database.Requests != 2 || ins.Database.Overlap != 1 {
		t.Fatalf("requests=%d overlap=%d", ins.Database.Requests, ins.Database.Overlap)
	}
	if live.Totals().Requests != 2 {
		t.Fatalf("live totals = %d, overlap must be the id intersection not the live count", live.Totals().Requests)
	}
	if ins.Database.OldestMs != oldAt.UnixMilli() || ins.Database.NewestMs != newAt.UnixMilli() {
		t.Fatalf("span oldest=%d newest=%d", ins.Database.OldestMs, ins.Database.NewestMs)
	}
	assertNoRestoreStaging(t, dir)
}

func TestRestoreDatabaseMergeKeepsLiveRows(t *testing.T) {
	live, opts, livePath, dir := openLiveStoreFixture(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "src.db")
	src, err := storage.Open(srcPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "shared", Provider: "backup.example", Model: "n", StatusCode: 200})
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Database: data})
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
	byID := map[string]*metrics.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if byID["shared"] == nil || byID["shared"].Provider != "live.example" {
		t.Fatalf("live row lost on conflict: %#v", byID["shared"])
	}
	if byID["live-row"] == nil || byID["backup-row"] == nil {
		t.Fatalf("merged rows = %#v", got)
	}
	assertNoRestoreStaging(t, dir)
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

// backupStatusWire is the strict decoder for the backup section GET
// /admin/config serves (backupStatus plus the modified list the settings
// handler appends). The W15 drop removed restart_for_database after
// re-verifying it had no consumer; the round-seven mutation round proved a
// re-added key passes the lenient reads unnoticed, so the served document
// must decode against exactly the current key set.
type backupStatusWire struct {
	Config          bool     `json:"config"`
	Database        bool     `json:"database"`
	PendingDatabase bool     `json:"pending_database"`
	Requests        int64    `json:"requests"`
	Modified        []string `json:"modified"`
}

// TestBackupStatusDocWireKeysStrict drives the production wiring
// (adminConfigHandler wires the real backupStatus) with a live store and a
// modified config file, then strict-decodes the served backup section: the
// re-added restart_for_database, or any renamed or new key, reddens.
func TestBackupStatusDocWireKeysStrict(t *testing.T) {
	liveReloadFixture(t)
	changed := config.Default()
	changed.CaptureBodyPreview = true
	if err := config.WriteFile(liveConfigPath, changed); err != nil {
		t.Fatal(err)
	}
	d := config.Default()
	dbPath := filepath.Join(t.TempDir(), "live.db")
	live, err := storage.Open(dbPath, storage.Options{
		WriteChanCap: d.StorageWriteChanCap, BatchCap: d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval, QueryTimeout: d.StorageQueryTimeout,
		QueryMaxBytes: int(d.StorageQueryMaxBytes), QueryMaxRows: d.StorageQueryMaxRows,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	liveCfg.DBPath = dbPath
	liveStore = live
	live.Record(&metrics.Record{ID: "one", Provider: "neutral.example", StatusCode: 200, Start: time.Now()})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	adminConfigHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d %s", rr.Code, rr.Body.String())
	}
	var doc struct {
		Backup json.RawMessage `json:"backup"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Backup) == 0 {
		t.Fatalf("GET /admin/config served no backup section: %s", rr.Body.String())
	}
	var status backupStatusWire
	dec := json.NewDecoder(bytes.NewReader(doc.Backup))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&status); err != nil {
		t.Fatalf("strict decode of the backup section: %v (%s)", err, doc.Backup)
	}
	if !status.Config || !status.Database || status.PendingDatabase {
		t.Fatalf("backup status members = %+v, want live config and database, no pending snapshot", status)
	}
	if status.Requests != 1 {
		t.Fatalf("requests = %d, want the one recorded request", status.Requests)
	}
	if len(status.Modified) != 1 || status.Modified[0] != "capture_body_preview" {
		t.Fatalf("modified = %v, want [capture_body_preview]", status.Modified)
	}
}

// Every backup/restore refusal for a missing durable store carries the one
// storage.ErrStorageDisabled identity (each surface keeps its own HTTP
// status), so callers classify with errors.Is instead of matching wording.
func TestBackupSurfacesReportStorageDisabledSentinel(t *testing.T) {
	oldCfg, oldStore, oldPath := liveCfg, liveStore, liveConfigPath
	t.Cleanup(func() { liveCfg, liveStore, liveConfigPath = oldCfg, oldStore, oldPath })
	liveCfg, liveStore, liveConfigPath = nil, nil, ""

	if _, err := dbStageDir(); !errors.Is(err, storage.ErrStorageDisabled) {
		t.Errorf("dbStageDir: %v, want ErrStorageDisabled", err)
	}
	if _, _, err := buildBackup(t.Context(), false, true); !errors.Is(err, storage.ErrStorageDisabled) {
		t.Errorf("buildBackup database member: %v, want ErrStorageDisabled", err)
	}
	if _, err := restoreDatabase(t.Context(), "", "replace"); !errors.Is(err, storage.ErrStorageDisabled) {
		t.Errorf("restoreDatabase without db_path: %v, want ErrStorageDisabled", err)
	}

	// A configured db_path with no live store still refuses a merge with
	// the same identity.
	liveCfg = config.Default()
	liveCfg.DBPath = filepath.Join(t.TempDir(), "live.db")
	if _, err := restoreDatabase(t.Context(), "", "merge"); !errors.Is(err, storage.ErrStorageDisabled) {
		t.Errorf("restoreDatabase merge without store: %v, want ErrStorageDisabled", err)
	}

	// The HTTP surface keeps its 400 action-plane status.
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?database=1", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("backup without storage: status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
}
