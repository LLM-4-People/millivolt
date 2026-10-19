package main

import (
	"bytes"
	"context"
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

// TestStandaloneRestoreDatabaseSerializesWithCombinedRestore pins the lock the
// standalone database-only path shares with the HTTP combined restore. A
// combined restore paused before its config write stages a pending snapshot;
// the standalone restore must wait, not publish a newer snapshot that the
// combined rollback then deletes. Both write the same admission slot, so the
// surviving file is the standalone restore's snapshot.
func TestStandaloneRestoreDatabaseSerializesWithCombinedRestore(t *testing.T) {
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
		data, err := src.Snapshot(context.Background())
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	combinedData := source("combined-src.db", "combined-row")
	laterData := source("later-src.db", "later-row")
	laterPath := filepath.Join(dir, "later-snapshot.db")
	if err := os.WriteFile(laterPath, laterData, 0o600); err != nil {
		t.Fatal(err)
	}

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

	standaloneResult := make(chan error, 1)
	go func() {
		_, err := restoreDatabase(context.Background(), laterPath, "replace")
		standaloneResult <- err
	}()
	// With the shared serializer the standalone restore cannot finish while
	// the combined restore holds reloadMu. Without it, the standalone snapshot
	// is published and then deleted by the combined rollback below.
	var standaloneErr error
	completedDuringPause := false
	select {
	case standaloneErr = <-standaloneResult:
		completedDuringPause = true
	case <-time.After(2 * time.Second):
	}
	close(release)
	rr := <-combinedResult
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("combined restore status = %d, want 400 from the injected config failure (body %s)", rr.Code, rr.Body.String())
	}
	if !completedDuringPause {
		standaloneErr = <-standaloneResult
	}
	if standaloneErr != nil {
		t.Fatalf("standalone restoreDatabase: %v", standaloneErr)
	}
	if completedDuringPause {
		t.Fatal("standalone restore completed while the combined restore held the shared admission slot")
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(livePath)); err != nil {
		t.Fatalf("shared pending slot was deleted by the combined rollback: %v", err)
	}
}
