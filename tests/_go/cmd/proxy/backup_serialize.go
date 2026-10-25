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
	"syscall"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestSettingsSaveSerializedWithRestore pins the Settings/restore serializer:
// a save that starts while a restore holds the config transaction must not
// write its stale view over the restore and report saved:true for an edit the
// final file does not contain. The stale revision is rejected after the
// restore completes; the restore's config remains final.
func TestSettingsSaveSerializedWithRestore(t *testing.T) {
	liveReloadFixture(t)
	start := config.Default()
	start.MaxRetries = 1
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	liveCfg = start.Clone()

	h := adminConfigHandler()
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d %s", get.Code, get.Body.String())
	}
	var doc struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &doc); err != nil || doc.Revision == "" {
		t.Fatalf("GET revision = %q, err %v", doc.Revision, err)
	}

	persist := h.Persist
	if persist == nil {
		t.Fatal("production Settings handler has no Persist")
	}
	persistCalled := make(chan struct{}, 1)
	h.Persist = func() ([]string, error) {
		select {
		case persistCalled <- struct{}{}:
		default:
		}
		return persist()
	}

	archived := config.Default()
	archived.MaxRetries = 42
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode("", backup.Archive{Config: yamlBuf.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restoreConfigWriteHook = nil
		restoreConfigReloadHook = nil
	})
	paused, release := make(chan struct{}), make(chan struct{})
	restoreConfigWriteHook = func(path string, cfg *config.Config) error {
		close(paused)
		<-release
		return config.WriteFile(path, cfg)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)

	restoreResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1", bytes.NewReader(raw)))
		restoreResult <- rr
	}()
	<-paused

	settingsResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/config",
			strings.NewReader(`{"revision":"`+doc.Revision+`","values":{"max_retries":7}}`)))
		settingsResult <- rr
	}()
	// Before the serializer, the save reaches WriteFile and Persist while
	// the restore is paused (write first, then Persist); after it, the save
	// waits for the restore. Either way the release below is deterministic.
	select {
	case <-persistCalled:
	case <-time.After(250 * time.Millisecond):
	}
	close(release)

	if rr := <-restoreResult; rr.Code != http.StatusOK {
		t.Fatalf("restore status %d body %s", rr.Code, rr.Body.String())
	}
	settings := <-settingsResult
	final, err := config.LoadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	switch settings.Code {
	case http.StatusConflict:
		if final.MaxRetries != 42 {
			t.Fatalf("stale save rejected but final max_retries=%d, want the restore's 42", final.MaxRetries)
		}
	case http.StatusOK:
		var saved struct {
			Saved     bool           `json:"saved"`
			Values    map[string]any `json:"values"`
			Effective map[string]any `json:"effective"`
		}
		if err := json.Unmarshal(settings.Body.Bytes(), &saved); err != nil {
			t.Fatalf("save body %q: %v", settings.Body.String(), err)
		}
		if saved.Saved {
			if final.MaxRetries != 7 {
				t.Fatalf("saved:true for max_retries=7 but the final config is %d", final.MaxRetries)
			}
			if saved.Values["max_retries"] != float64(7) || saved.Effective["max_retries"] != float64(7) {
				t.Fatalf("saved response values/effective = %v/%v, want 7", saved.Values["max_retries"], saved.Effective["max_retries"])
			}
		} else if final.MaxRetries != 42 {
			t.Fatalf("unsaved settings left final max_retries=%d, want the restore's 42", final.MaxRetries)
		}
	default:
		t.Fatalf("settings status %d body %s, want 200 or the stale-save 409", settings.Code, settings.Body.String())
	}
}

// TestRestoreInspectLiveConfigFailsClosed pins C7: a configured but unreadable
// or missing live config makes inspect fail closed instead of comparing the
// archive against fabricated Default() values. An instance started without
// -config still documents its Default comparison in the success row.
func TestRestoreInspectLiveConfigFailsClosed(t *testing.T) {
	liveReloadFixture(t)
	archived := config.Default()
	archived.MaxRetries = 2
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, archived); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode("", backup.Archive{Config: yaml.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	inspect := func() *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1", bytes.NewReader(raw)))
		return rr
	}
	assertUnavailable := func(rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("inspect status %d body %s, want 400", rr.Code, rr.Body.String())
		}
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("inspect error body %q: %v", rr.Body.String(), err)
		}
		if !strings.Contains(body.Error, "live config unavailable") {
			t.Fatalf("inspect error %q does not report the unavailable live config", body.Error)
		}
		if strings.Contains(rr.Body.String(), "vs_live") || strings.Contains(rr.Body.String(), `"ok":true`) {
			t.Fatalf("inspect fabricated a success diff: %s", rr.Body.String())
		}
	}

	if err := os.WriteFile(liveConfigPath, []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(inspect())

	// Repair semantics would omit an unknown nested field or an extra YAML
	// document; inspect must fail closed like the strict apply path.
	for _, strictFailure := range []string{
		"providers:\n  example:\n    models_path: /v1/models\n    bogus_field: true\n",
		"max_retries: 2\n---\nmax_retries: 3\n",
	} {
		if err := os.WriteFile(liveConfigPath, []byte(strictFailure), 0o600); err != nil {
			t.Fatal(err)
		}
		assertUnavailable(inspect())
	}

	// The bounded read owner rejects an overgrown document before a whole-file
	// read, so inspect reports the size limit rather than allocating it.
	oversized := bytes.Repeat([]byte("a"), config.ConfigDocumentMaxBytes+1)
	if err := os.WriteFile(liveConfigPath, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(inspect())

	if err := os.Remove(liveConfigPath); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(inspect())

	// A directory is not a regular file; the read owner fails closed instead of
	// attempting a document read.
	if err := os.Mkdir(liveConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(inspect())
	if err := os.Remove(liveConfigPath); err != nil {
		t.Fatal(err)
	}

	// A FIFO must be rejected from its stat before os.Open, which would block
	// forever waiting for a writer. Run inspect off the test goroutine so a
	// reordering regression fails on the timeout instead of hanging the suite.
	if err := syscall.Mkfifo(liveConfigPath, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	fifoDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { fifoDone <- inspect() }()
	select {
	case fifoRR := <-fifoDone:
		assertUnavailable(fifoRR)
	case <-time.After(2 * time.Second):
		t.Fatal("inspect blocked on a FIFO instead of rejecting the non-regular path")
	}
	if err := os.Remove(liveConfigPath); err != nil {
		t.Fatal(err)
	}

	liveConfigPath = ""
	rr := inspect()
	if rr.Code != http.StatusOK {
		t.Fatalf("no-path inspect status %d body %s", rr.Code, rr.Body.String())
	}
	ins := decodeRestoreInspectStrict(t, rr.Body.Bytes())
	if !ins.OK || !slices.Contains(ins.Config.VsLive, "max_retries") {
		t.Fatalf("no-path inspect vs_live=%v, want the documented Default comparison", ins.Config.VsLive)
	}
}

// TestBackupRejectsInvalidBaseConfig pins TC7-1: a backup capture must not
// silently render built-in defaults from an invalid base file.
func TestBackupRejectsInvalidBaseConfig(t *testing.T) {
	liveReloadFixture(t)
	raw := []byte("max_retries: not-an-int\n")
	if err := os.WriteFile(liveConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?config=1", nil))
	if rr.Code == http.StatusOK {
		t.Fatalf("backup accepted an invalid base config: %s", rr.Body.String())
	}
	got, err := os.ReadFile(liveConfigPath)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("backup rewrote the invalid base file: %v\n%s", err, got)
	}
}

// TestRestoreMergeRejectsInvalidBaseConfig pins TC7-1: a merge restore must
// fail before writing when the current base file is invalid.
func TestRestoreMergeRejectsInvalidBaseConfig(t *testing.T) {
	liveReloadFixture(t)
	raw := []byte("max_retries: not-an-int\n")
	if err := os.WriteFile(liveConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var incoming bytes.Buffer
	if err := config.WriteYAML(&incoming, config.Default()); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRestoreConfig(incoming.Bytes(), "merge"); err == nil {
		t.Fatal("merge restore accepted an invalid base config")
	}
	got, err := os.ReadFile(liveConfigPath)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("merge restore rewrote the invalid base file: %v\n%s", err, got)
	}
}

// TestRestoreConfigCaptureFailsClosed pins the rollback-capture read bound:
// prepareRestoreConfig must reject a FIFO or an oversized live config before
// any write, using the same bounded config.ReadDocument owner as inspect. A
// plain os.ReadFile would block on the FIFO or allocate the oversized file.
func TestRestoreConfigCaptureFailsClosed(t *testing.T) {
	liveReloadFixture(t)
	raw, err := backup.Encode("", backup.Archive{Config: defaultConfigYAMLForRestore(t)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	restore := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1", bytes.NewReader(raw)))
		return rr
	}
	assertRejected := func(rr *httptest.ResponseRecorder, want string) {
		t.Helper()
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("restore status %d body %s, want 400", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("restore rejection %q does not contain %q", rr.Body.String(), want)
		}
	}

	if err := syscall.Mkfifo(liveConfigPath, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- restore() }()
	select {
	case rr := <-done:
		assertRejected(rr, "not a regular file")
	case <-time.After(2 * time.Second):
		t.Fatal("restore blocked on a FIFO instead of rejecting the non-regular live config")
	}
	if err := os.Remove(liveConfigPath); err != nil {
		t.Fatal(err)
	}

	oversized := bytes.Repeat([]byte("a"), config.ConfigDocumentMaxBytes+1)
	if err := os.WriteFile(liveConfigPath, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	assertRejected(restore(), "config document limit")
}

// TestRestoreConfigKeepsCLIOverrides pins the preview's premise: a replace and
// a merge both keep the process-start listen and db_path overrides, so the
// settings panel must not render them as reset by the backup.
func TestRestoreConfigKeepsCLIOverrides(t *testing.T) {
	liveReloadFixture(t)
	liveListenOverride = "127.0.0.1:19090"
	liveDBOverride = filepath.Join(t.TempDir(), "live.db")
	liveCfg.Listen = liveListenOverride
	liveCfg.DBPath = liveDBOverride
	bootCfg = liveCfg.Clone()
	if err := config.WriteFile(liveConfigPath, liveCfg); err != nil {
		t.Fatal(err)
	}

	var incoming bytes.Buffer
	if err := config.WriteYAML(&incoming, config.Default()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"replace", "merge"} {
		t.Run(mode, func(t *testing.T) {
			plan, err := prepareRestoreConfig(incoming.Bytes(), mode)
			if err != nil {
				t.Fatal(err)
			}
			if plan.next.Listen != liveListenOverride {
				t.Fatalf("restored listen = %q, want the CLI override %q", plan.next.Listen, liveListenOverride)
			}
			if plan.dbPath != liveDBOverride {
				t.Fatalf("staged db path = %q, want the CLI override %q", plan.dbPath, liveDBOverride)
			}
		})
	}
}

// TestBackupCaptureMissingConfigFailsClosed pins the one consumer that does
// not substitute defaults: a config backup with no file has nothing to archive
// and must report the missing document.
func TestBackupCaptureMissingConfigFailsClosed(t *testing.T) {
	liveReloadFixture(t)
	liveConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	_, cleanup, err := buildBackup(t.Context(), true, false)
	defer cleanup()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-config backup error = %v, want the missing-file error", err)
	}
}

// TestRestoreMergeMissingConfigUsesDefaults pins the restore merge exception:
// a missing live file merges the archive over the built-in defaults and does
// not create the missing file during preparation.
func TestRestoreMergeMissingConfigUsesDefaults(t *testing.T) {
	liveReloadFixture(t)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	liveConfigPath = missing
	plan, err := prepareRestoreConfig([]byte("max_retries: 9\n"), "merge")
	if err != nil {
		t.Fatalf("prepareRestoreConfig with a missing file = %v", err)
	}
	if plan.next.MaxRetries != 9 {
		t.Fatalf("merged max_retries = %d, want 9", plan.next.MaxRetries)
	}
	if plan.previousExists {
		t.Fatal("missing file was reported as an existing previous document")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore preparation created the missing config file: %v", err)
	}
}
