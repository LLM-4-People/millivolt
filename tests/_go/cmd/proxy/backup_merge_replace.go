package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestRestoreRejectsDatabaseFailingWriterPreflight pins the restore-time
// counterpart of boot admission: an archive whose database passes integrity
// but cannot accept the writer INSERT (an extra required column, or a trigger
// that aborts it) must answer 400 after staging, before any config byte is
// written, with the pre-existing pending slot and live config untouched. A
// valid archive still stages and applies.
func TestRestoreRejectsDatabaseFailingWriterPreflight(t *testing.T) {
	liveReloadFixture(t)
	live, _, livePath, dir := openLiveStoreFixture(t)
	validData, err := live.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	start := config.Default()
	start.MaxRetries = 1
	start.DBPath = livePath
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	liveCfg = start.Clone()
	liveStore = live
	bootCfg = start.Clone()

	archived := config.Default()
	archived.MaxRetries = 42
	archived.DBPath = livePath
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, archived); err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeLive := liveCfg.Clone()
	pending := storage.PendingSnapshotPath(livePath)
	oldPending := []byte("pre-existing pending snapshot")
	assertUntouched := func(t *testing.T) {
		t.Helper()
		gotConfig, err := os.ReadFile(liveConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotConfig, beforeConfig) {
			t.Fatalf("failed restore rewrote the config file:\nbefore=%q\nafter=%q", beforeConfig, gotConfig)
		}
		if diff := config.DiffKeys(liveCfg, beforeLive); len(diff) != 0 {
			t.Fatalf("failed restore changed liveCfg keys %v", diff)
		}
		gotPending, err := os.ReadFile(pending)
		if err != nil {
			t.Fatalf("failed restore did not preserve the pending slot: %v", err)
		}
		if !bytes.Equal(gotPending, oldPending) {
			t.Fatalf("pending slot = %q, want the preserved pre-existing bytes", gotPending)
		}
	}

	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	for _, tc := range []struct {
		name       string
		statements []string
	}{
		{
			name: "extra-required-column",
			statements: []string{
				`DELETE FROM requests`,
				`ALTER TABLE requests ADD COLUMN audit_extra TEXT NOT NULL`,
			},
		},
		{
			name: "writer-abort-trigger",
			statements: []string{
				`CREATE TRIGGER audit_writer_probe BEFORE INSERT ON requests
					BEGIN SELECT RAISE(ABORT, 'audit writer rejected'); END`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badData := mutatePackedDatabase(t, validData, tc.statements...)
			raw, err := backup.Encode(dir, backup.Archive{Config: yamlBuf.Bytes(), Database: badData})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pending, oldPending, 0o600); err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1", bytes.NewReader(raw)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %s, want 400 from the writer preflight", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "writer insert probe") {
				t.Fatalf("body %q does not name the writer insert preflight", rr.Body.String())
			}
			assertUntouched(t)
			assertNoPreservedPending(t, dir)
			assertNoRestoreStaging(t, dir)
		})
	}

	raw, err := backup.Encode(dir, backup.Archive{Config: yamlBuf.Bytes(), Database: validData})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, oldPending, 0o600); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("valid archive status %d body %s", rr.Code, rr.Body.String())
	}
	got, err := config.LoadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 42 || got.DBPath != livePath {
		t.Fatalf("config after valid restore max_retries=%d db_path=%q, want 42 and %q", got.MaxRetries, got.DBPath, livePath)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("valid restore did not publish the pending snapshot: %v", err)
	}
	if err := storage.ValidatePendingSnapshot(livePath); err != nil {
		t.Fatalf("staged valid snapshot no longer passes preflight: %v", err)
	}
	assertNoPreservedPending(t, dir)
	assertNoRestoreStaging(t, dir)
}

// TestRestoreConfigApplyFailureRollsBackConfigAndPending covers both
// post-staging config failures (write and reload) with and without a
// pre-existing pending snapshot: the config file and liveCfg return to their
// exact pre-restore values, a newly staged snapshot is removed, and a
// pre-existing snapshot is restored byte-for-byte.
func TestRestoreConfigApplyFailureRollsBackConfigAndPending(t *testing.T) {
	for _, phase := range []string{"write", "reload"} {
		for _, preexisting := range []bool{false, true} {
			name := phase + "/empty"
			if preexisting {
				name = phase + "/preexisting"
			}
			t.Run(name, func(t *testing.T) {
				liveReloadFixture(t)
				store, _, livePath, liveDir := openLiveStoreFixture(t)
				store.Record(&metrics.Record{ID: "apply-failure", Provider: "neutral.example", Model: "n", StatusCode: 200})
				if err := store.Flush(); err != nil {
					t.Fatal(err)
				}
				data, err := store.Snapshot(t.Context())
				if err != nil {
					t.Fatal(err)
				}

				start := config.Default()
				start.MaxRetries = 1
				start.DBPath = livePath
				if err := config.WriteFile(liveConfigPath, start); err != nil {
					t.Fatal(err)
				}
				liveCfg = start.Clone()
				liveStore = store

				targetDir := t.TempDir()
				target := filepath.Join(targetDir, "restored.db")
				archived := config.Default()
				archived.MaxRetries = 42
				archived.DBPath = target
				var yaml bytes.Buffer
				if err := config.WriteYAML(&yaml, archived); err != nil {
					t.Fatal(err)
				}
				raw, err := backup.Encode(liveDir, backup.Archive{Config: yaml.Bytes(), Database: data})
				if err != nil {
					t.Fatal(err)
				}
				beforeConfig, err := os.ReadFile(liveConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				beforeLive := liveCfg.Clone()
				oldPending := []byte("pre-existing pending snapshot")
				if preexisting {
					if err := os.WriteFile(storage.PendingSnapshotPath(target), oldPending, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					restoreConfigWriteHook = nil
					restoreConfigReloadHook = nil
				})
				switch phase {
				case "write":
					restoreConfigWriteHook = func(string, *config.Config) error {
						return errors.New("injected config write failure")
					}
				case "reload":
					restoreConfigReloadHook = func() ([]string, error) {
						// Model a reload that touched the live snapshot before
						// failing so rollback has real state to put back.
						liveMu.Lock()
						liveCfg = config.Default()
						liveCfg.MaxRetries = 99
						liveMu.Unlock()
						return nil, errors.New("injected config reload failure")
					}
				}

				mux := http.NewServeMux()
				registerBackupRoutes(mux)
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1", bytes.NewReader(raw)))
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("status %d body %s, want 400 from the config phase", rr.Code, rr.Body.String())
				}
				afterConfig, err := os.ReadFile(liveConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(beforeConfig, afterConfig) {
					t.Fatalf("failed restore did not restore the config bytes:\nbefore=%q\nafter=%q", beforeConfig, afterConfig)
				}
				if diff := config.DiffKeys(liveCfg, beforeLive); len(diff) != 0 {
					t.Fatalf("failed restore did not restore liveCfg keys %v", diff)
				}
				if liveCfg.MaxRetries != 1 || liveCfg.DBPath != livePath {
					t.Fatalf("liveCfg max_retries=%d db_path=%q, want the pre-restore values", liveCfg.MaxRetries, liveCfg.DBPath)
				}
				pending := storage.PendingSnapshotPath(target)
				if preexisting {
					got, err := os.ReadFile(pending)
					if err != nil {
						t.Fatalf("pre-existing pending snapshot was not restored: %v", err)
					}
					if !bytes.Equal(got, oldPending) {
						t.Fatalf("pending snapshot = %q, want the preserved pre-existing bytes", got)
					}
				} else if _, err := os.Stat(pending); !os.IsNotExist(err) {
					t.Fatal("failed restore left the newly staged pending snapshot")
				}
				assertNoPreservedPending(t, targetDir)
				assertNoRestoreStaging(t, liveDir)
			})
		}
	}
}

// TestConcurrentRestoresSerializeAdmissionSlot pins the shared admission-slot
// transaction: a combined config+database restore paused before its config
// write must not let a database-only restore publish a newer pending snapshot
// that the combined restore's rollback then deletes. The database-only
// handler waits on reloadMu; after the combined restore fails, its own
// rollback runs before the database-only publication exists, so the later
// snapshot survives.
func TestConcurrentRestoresSerializeAdmissionSlot(t *testing.T) {
	liveReloadFixture(t)
	live, opts, livePath, dir := openLiveStoreFixture(t)
	start := config.Default()
	start.MaxRetries = 1
	start.DBPath = livePath
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	liveCfg = start.Clone()
	liveStore = live

	source := func(name, id string) []byte {
		t.Helper()
		src, err := storage.Open(filepath.Join(dir, name), opts)
		if err != nil {
			t.Fatal(err)
		}
		src.Record(&metrics.Record{ID: id, Provider: "neutral.example", Model: "n", StatusCode: 200})
		if err := src.Flush(); err != nil {
			src.Close()
			t.Fatal(err)
		}
		data, err := src.Snapshot(t.Context())
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	combinedData := source("combined-src.db", "combined-row")
	laterData := source("later-src.db", "later-row")

	archived := config.Default()
	archived.MaxRetries = 42
	archived.DBPath = livePath
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, archived); err != nil {
		t.Fatal(err)
	}
	combinedArchive, err := backup.Encode(dir, backup.Archive{Config: yaml.Bytes(), Database: combinedData})
	if err != nil {
		t.Fatal(err)
	}
	laterArchive, err := backup.Encode(dir, backup.Archive{Database: laterData})
	if err != nil {
		t.Fatal(err)
	}

	paused, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		restoreConfigWriteHook = nil
		restoreConfigReloadHook = nil
	})
	restoreConfigWriteHook = func(string, *config.Config) error {
		close(paused)
		<-release
		return errors.New("injected config write failure")
	}

	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	combinedResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1", bytes.NewReader(combinedArchive)))
		combinedResult <- rr
	}()
	<-paused

	laterResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?database=1", bytes.NewReader(laterArchive)))
		laterResult <- rr
	}()
	// Before the fix the database-only handler completes while the combined
	// restore is paused. With both handlers sharing reloadMu it stays blocked,
	// so release the combined restore after the serialization window.
	var later *httptest.ResponseRecorder
	select {
	case later = <-laterResult:
	case <-time.After(2 * time.Second):
	}
	close(release)

	if rr := <-combinedResult; rr.Code != http.StatusBadRequest {
		t.Fatalf("combined restore status %d body %s, want 400 from the config phase", rr.Code, rr.Body.String())
	}
	if later == nil {
		later = <-laterResult
	}
	if later.Code != http.StatusOK {
		t.Fatalf("database-only restore status %d body %s, want 200", later.Code, later.Body.String())
	}
	pending, err := os.ReadFile(storage.PendingSnapshotPath(livePath))
	if err != nil {
		t.Fatalf("failed combined restore deleted the later database-only publication: %v", err)
	}
	if !bytes.Equal(pending, laterData) {
		t.Fatal("pending snapshot is not the later database-only restore's publication")
	}
	assertNoPreservedPending(t, dir)
}

// TestRestoreConfigDatabaseReplacePublishesBoth pins the successful
// config+database replace: the prepared config is written and hot-reloaded, the
// snapshot is published at the effective db_path (replacing any older pending
// slot), and the restart contract still reports db_path.
func TestRestoreConfigDatabaseReplacePublishesBoth(t *testing.T) {
	liveReloadFixture(t)
	store, _, livePath, liveDir := openLiveStoreFixture(t)
	store.Record(&metrics.Record{ID: "replace-success", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	start := config.Default()
	start.MaxRetries = 1
	start.DBPath = livePath
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	liveCfg = start.Clone()
	liveStore = store

	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "restored.db")
	archived := config.Default()
	archived.MaxRetries = 42
	archived.DBPath = target
	// The real pre-restore boot snapshot: the reload reports db_path as
	// startup-bound because it changed, and the handler appends its own
	// db_path for the staged replacement. The response must still carry one
	// db_path, not the duplicate this wiring used to emit.
	bootCfg = start.Clone()
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(liveDir, backup.Archive{Config: yaml.Bytes(), Database: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storage.PendingSnapshotPath(target), []byte("older pending snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("restore status %d body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		OK              bool     `json:"ok"`
		RestartRequired []string `json:"restart_required"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || !slices.Equal(body.RestartRequired, []string{"db_path"}) {
		t.Fatalf("restore json %+v, want ok with restart_required [db_path]", body)
	}
	got, err := config.LoadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != 42 || got.DBPath != target {
		t.Fatalf("config file max_retries=%d db_path=%q, want 42 and %q", got.MaxRetries, got.DBPath, target)
	}
	if liveCfg.MaxRetries != 42 || liveCfg.DBPath != target {
		t.Fatalf("liveCfg max_retries=%d db_path=%q, want 42 and %q", liveCfg.MaxRetries, liveCfg.DBPath, target)
	}
	pending, err := os.ReadFile(storage.PendingSnapshotPath(target))
	if err != nil {
		t.Fatalf("successful restore did not publish the pending snapshot: %v", err)
	}
	if !bytes.Equal(pending, data) {
		t.Fatal("pending snapshot bytes differ from the archive member")
	}
	assertNoPreservedPending(t, targetDir)
	assertNoRestoreStaging(t, liveDir)
}

// TestRestoreConfigDatabaseMergeStillMerges preserves the combined merge path:
// the database merge runs before the config phase, both members apply, no
// pending replacement is staged, and a merge needs no restart.
func TestRestoreConfigDatabaseMergeStillMerges(t *testing.T) {
	liveReloadFixture(t)
	live, opts, livePath, dir := openLiveStoreFixture(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src, err := storage.Open(filepath.Join(dir, "merge-src.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	src.Close()
	if err != nil {
		t.Fatal(err)
	}

	start := config.Default()
	start.MaxRetries = 1
	start.DBPath = livePath
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	liveCfg = start.Clone()
	liveStore = live

	archived := config.Default()
	archived.MaxRetries = 42
	archived.DBPath = livePath
	bootCfg = archived.Clone()
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Config: yaml.Bytes(), Database: data})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1&database_mode=merge", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
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
		t.Fatalf("merge json %+v, want ok with no restart", body)
	}
	if got, err := config.LoadFile(liveConfigPath); err != nil || got.MaxRetries != 42 {
		t.Fatalf("merged config max_retries=%v err=%v, want 42", got, err)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]bool{}
	for _, r := range rows {
		byID[r.ID] = true
	}
	if !byID["live-row"] || !byID["backup-row"] {
		t.Fatalf("merged rows = %v, want the live and backup rows", byID)
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(livePath)); !os.IsNotExist(err) {
		t.Fatal("merge staged a replace snapshot")
	}
	assertNoRestoreStaging(t, dir)
}

// TestRestoreCombinedMergeRejectsChangedDBPath pins the merge-path guard: a
// config+database merge whose restored effective db_path differs from the
// running store's file would write rows into the old database while the next
// boot opens the new one. It must answer 400 before the config or any row
// changes. A database-only merge keeps merging into the running store even
// when the live config's startup-bound db_path already differs.
func TestRestoreCombinedMergeRejectsChangedDBPath(t *testing.T) {
	liveReloadFixture(t)
	live, opts, livePath, dir := openLiveStoreFixture(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src, err := storage.Open(filepath.Join(dir, "merge-other.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	src.Close()
	if err != nil {
		t.Fatal(err)
	}

	start := config.Default()
	start.MaxRetries = 1
	start.DBPath = livePath
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	liveCfg = start.Clone()
	liveStore = live
	bootCfg = start.Clone()

	nextPath := filepath.Join(dir, "next.db")
	archived := config.Default()
	archived.MaxRetries = 42
	archived.DBPath = nextPath
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Config: yamlBuf.Bytes(), Database: data})
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeLive := liveCfg.Clone()

	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1&database_mode=merge", bytes.NewReader(raw)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("combined merge status %d body %s, want 400 from the db_path guard", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "db_path") {
		t.Fatalf("body %q does not name db_path", rr.Body.String())
	}
	afterConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatalf("rejected merge rewrote the config file:\nbefore=%q\nafter=%q", beforeConfig, afterConfig)
	}
	if diff := config.DiffKeys(liveCfg, beforeLive); len(diff) != 0 {
		t.Fatalf("rejected merge changed liveCfg keys %v", diff)
	}
	rows, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == "backup-row" {
			t.Fatal("rejected merge inserted a backup row")
		}
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(nextPath)); !os.IsNotExist(err) {
		t.Fatal("rejected merge staged a pending snapshot at the restored db_path")
	}
	assertNoRestoreStaging(t, dir)

	// Database-only merge is not this guard's target: it writes into the
	// running store, which is the documented behavior even after a reload
	// changed the startup-bound db_path.
	liveCfg.DBPath = nextPath
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?database=1&database_mode=merge", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("database-only merge status %d body %s", rr.Code, rr.Body.String())
	}
	rows, err = live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	merged := false
	for _, row := range rows {
		if row.ID == "backup-row" {
			merged = true
		}
	}
	if !merged {
		t.Fatal("database-only merge did not insert into the running store")
	}
	if pending, err := os.Stat(storage.PendingSnapshotPath(nextPath)); !os.IsNotExist(err) {
		t.Fatalf("database-only merge staged a replacement: %v", pending)
	}
	assertNoRestoreStaging(t, dir)
}
