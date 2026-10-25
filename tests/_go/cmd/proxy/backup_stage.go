package main

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestDBStageDirNamesLiveStoreVolumeAfterReloadPathChange pins the staging
// volume's owner. db_path is startup-bound, so a reload that changes it
// swaps the config snapshot while the running store keeps its boot path:
// dbStageDir must name the live database's volume (the store's), never the
// not-yet-active config value, while replace-mode restore keeps staging at
// the live config's path - the next restart opens the persisted db_path.
func TestDBStageDirNamesLiveStoreVolumeAfterReloadPathChange(t *testing.T) {
	store, _, dbPath, _ := openLiveStoreFixture(t)
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = store
	next := config.Default()
	next.DBPath = filepath.Join(t.TempDir(), "next.db")
	// The reload's snapshot publication (reloadConfig repoints liveCfg to
	// a fresh clone under liveMu) with a changed, startup-bound db_path:
	// the running store still has the boot path.
	liveCfg = next
	got, err := dbStageDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(dbPath); got != want {
		t.Fatalf("dbStageDir = %q, want the live store's volume %q", got, want)
	}
	data, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	decoded := filepath.Join(t.TempDir(), "decoded.db")
	if err := os.WriteFile(decoded, data, 0o600); err != nil {
		t.Fatal(err)
	}
	restart, err := restoreDatabase(t.Context(), decoded, "replace")
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("replace restore reported restart=false, want true")
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(next.DBPath)); err != nil {
		t.Fatalf("replace did not stage at the live config's db_path: %v", err)
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(dbPath)); !os.IsNotExist(err) {
		t.Fatal("replace staged at the store's boot path, want the config path")
	}
}

// assertNoPreservedPending pins the admission-slot guard cleanup: a preserved
// pre-existing pending snapshot is either restored to the slot on failure or
// removed after a successful restore, never left as a hidden sibling.
func assertNoPreservedPending(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".millivolt-restore-pending-") {
			t.Fatalf("preserved pending snapshot left behind: %s", e.Name())
		}
	}
}

// TestRestoreConfigDatabaseStageFailureLeavesStateUntouched pins the H3
// transaction order: the database stages before the config is written, so an
// archived db_path under a missing directory answers 400 with the config file,
// liveCfg, and pending admission slot exactly as they were. A later boot can
// only open the pre-restore path.
func TestRestoreConfigDatabaseStageFailureLeavesStateUntouched(t *testing.T) {
	liveReloadFixture(t)
	store, _, livePath, dir := openLiveStoreFixture(t)
	store.Record(&metrics.Record{ID: "stage-failure", Provider: "neutral.example", Model: "n", StatusCode: 200})
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

	archived := config.Default()
	archived.MaxRetries = 42
	archived.DBPath = filepath.Join(dir, "missing-subdir", "next.db")
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Config: yaml.Bytes(), Database: data})
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
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1&database=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s, want 400 from database staging", rr.Code, rr.Body.String())
	}
	afterConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatalf("failed restore rewrote the config file:\nbefore=%q\nafter=%q", beforeConfig, afterConfig)
	}
	if diff := config.DiffKeys(liveCfg, beforeLive); len(diff) != 0 {
		t.Fatalf("failed restore changed liveCfg keys %v", diff)
	}
	if liveCfg.MaxRetries != 1 || liveCfg.DBPath != livePath {
		t.Fatalf("liveCfg max_retries=%d db_path=%q, want the pre-restore values", liveCfg.MaxRetries, liveCfg.DBPath)
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(archived.DBPath)); !os.IsNotExist(err) {
		t.Fatal("failed restore staged a pending snapshot at the archived db_path")
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(livePath)); !os.IsNotExist(err) {
		t.Fatal("failed restore staged a pending snapshot at the live db_path")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing-subdir")); !os.IsNotExist(err) {
		t.Fatal("failed restore created the archived db_path directory")
	}
	assertNoPreservedPending(t, dir)
	assertNoRestoreStaging(t, dir)
}

// mutatePackedDatabase applies statements to a packed snapshot file and
// returns its new bytes. Integrity still passes; only the writer contract the
// staged preflight probes is broken.
func mutatePackedDatabase(t *testing.T, data []byte, statements ...string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mutated.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("mutate packed database: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
