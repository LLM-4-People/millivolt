package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestBackupRestoreConfigRoundTrip(t *testing.T) {
	liveReloadFixture(t)
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
	// The download is an artifact like the export and capture surfaces:
	// octet-stream, no-store, and a millivolt-backup-<UTC stamp>.mvb
	// attachment name - the one shared header owner composes all three.
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("backup Content-Type = %q, want application/octet-stream", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("backup Cache-Control = %q, want no-store", cc)
	}
	dispo := regexp.MustCompile(`^attachment; filename="millivolt-backup-(\d{8}-\d{6})\.mvb"$`).
		FindStringSubmatch(rr.Header().Get("Content-Disposition"))
	if dispo == nil {
		t.Fatalf("Content-Disposition = %q, want a millivolt-backup-<stamp>.mvb attachment",
			rr.Header().Get("Content-Disposition"))
	}
	stamp, err := time.ParseInLocation("20060102-150405", dispo[1], time.UTC)
	if err != nil {
		t.Fatalf("backup attachment stamp %q does not parse: %v", dispo[1], err)
	}
	if skew := time.Since(stamp); skew < -time.Minute || skew > time.Minute {
		t.Fatalf("backup attachment stamp %s sits %s from now UTC - the artifact filename clock must be UTC", dispo[1], skew)
	}
	raw := rr.Body.Bytes()
	arch, err := backup.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Validate("", arch); err != nil {
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

// TestBackupHandlerStreamsFileWithContentLength pins the download path: the
// response is served from the encoded staging file, Content-Length is that
// file's size, the artifact headers are unchanged, and the bytes equal what
// backup.Encode emits for the same archive and Created moment.
func TestBackupHandlerStreamsFileWithContentLength(t *testing.T) {
	liveReloadFixture(t)
	cfg := config.Default()
	cfg.MaxRetries = 7
	if err := config.WriteFile(liveConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	t.Setenv("TMPDIR", stage)
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?config=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("backup status %d body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.Bytes()
	if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(body))
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if dispo := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(dispo, `attachment; filename="millivolt-backup-`) {
		t.Fatalf("Content-Disposition = %q, want an attachment backup name", dispo)
	}
	arch, err := backup.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Validate("", arch); err != nil {
		t.Fatal(err)
	}
	want, err := backup.Encode("", backup.Archive{Created: arch.Created, Config: arch.Config})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatal("handler bytes differ from backup.Encode for the same archive")
	}
	assertNoBackupStaging(t, stage)
}

// TestBuildBackupDatabaseUsesSnapshotFile pins the handler's database member
// source: buildBackup stages a private packed file on the live database's
// volume and returns it by path rather than as heap bytes, and its cleanup
// removes the whole snapshot directory.
func TestBuildBackupDatabaseUsesSnapshotFile(t *testing.T) {
	store, _, dbPath, dir := openLiveStoreFixture(t)
	store.Record(&metrics.Record{ID: "file-backed", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = store

	arch, cleanup, err := buildBackup(t.Context(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if arch.DatabasePath == "" || len(arch.Database) != 0 {
		t.Fatalf("buildBackup source path=%q bytes=%d, want the file-backed source", arch.DatabasePath, len(arch.Database))
	}
	if got := filepath.Dir(filepath.Dir(arch.DatabasePath)); got != dir {
		t.Fatalf("snapshot staged under %q, want the live database volume %q", got, dir)
	}
	if err := backup.CheckDatabaseFile(arch.DatabasePath); err != nil {
		t.Fatalf("buildBackup did not return a valid packed database: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(arch.DatabasePath)); !os.IsNotExist(err) {
		t.Fatal("buildBackup cleanup left the snapshot directory")
	}
	assertNoBackupStaging(t, dir)
}

// TestBackupHandlerDatabaseUsesSnapshotFile pins the file-backed database
// download end to end: ?database=1 emits the same archive bytes as
// backup.Encode for the same members and Created time, Content-Length is the
// exact artifact size, and both the snapshot directory and the encoded
// archive staging file are removed after a normal response and after a client
// disconnect during the body.
func TestBackupHandlerDatabaseUsesSnapshotFile(t *testing.T) {
	store, _, dbPath, dir := openLiveStoreFixture(t)
	store.Record(&metrics.Record{ID: "http-file-backed", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = store
	mux := http.NewServeMux()
	registerBackupRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?database=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("backup status %d body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.Bytes()
	if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(body))
	}
	arch, err := backup.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(arch.Config) != 0 || len(arch.Database) == 0 {
		t.Fatal("database-only archive had the wrong members")
	}
	if err := backup.Validate(dir, arch); err != nil {
		t.Fatal(err)
	}
	want, err := backup.Encode(dir, backup.Archive{Created: arch.Created, Database: arch.Database})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatal("path-backed handler bytes differ from byte-backed backup.Encode for the same members")
	}
	assertNoBackupStaging(t, dir)

	mux.ServeHTTP(&failingWriter{}, httptest.NewRequest(http.MethodGet, "/admin/backup?database=1", nil))
	assertNoBackupStaging(t, dir)
}

// TestBackupHandlerCapFailsClosed proves a snapshot over backup_max_bytes is
// rejected during encode, before a partial artifact or success response can
// be served, and the staging file is removed.
func TestBackupHandlerCapFailsClosed(t *testing.T) {
	store, _, dbPath, dir := openLiveStoreFixture(t)
	// Store.Record owns the record-retention boundary, so a single 2 MiB user
	// agent is now correctly capped. Use the debug sidecar instead: it is a
	// legitimate durable payload with its own store cap, and the backup handler
	// snapshots the whole database including request_debug rows.
	store.Record(&metrics.Record{
		ID:         "oversize-backup",
		Provider:   "neutral.example",
		Model:      "n",
		StatusCode: 200,
		Debug:      true,
	})
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.SaveDebugCapture(t.Context(), "oversize-backup", "cap-test", now.UnixMilli(), now.Add(time.Hour).UnixMilli(), incompressibleBytes("backup-handler-cap", 2<<20)); err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveCfg.BackupMaxBytes = config.BackupMaxBytesMin
	liveStore = store
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/backup?database=1", nil))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d body %s, want 413", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "backup exceeds backup_max_bytes") {
		t.Fatalf("body %q does not name the cap", rr.Body.String())
	}
	assertNoBackupStaging(t, dir)
}

// TestBackupHandlerRemovesStagingOnWriteFailure pins the download cleanup
// owner: a client that disappears mid-body still leaves no staging artifact.
func TestBackupHandlerRemovesStagingOnWriteFailure(t *testing.T) {
	liveReloadFixture(t)
	if err := config.WriteFile(liveConfigPath, config.Default()); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	t.Setenv("TMPDIR", stage)
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	mux.ServeHTTP(&failingWriter{}, httptest.NewRequest(http.MethodGet, "/admin/backup?config=1", nil))
	assertNoBackupStaging(t, stage)
}

type failingWriter struct{ header http.Header }

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingWriter) WriteHeader(int) {}

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func assertNoBackupStaging(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if backup.IsStagingName(e.Name()) {
			t.Fatalf("backup staging file left behind: %s", e.Name())
		}
		if strings.HasPrefix(e.Name(), "millivolt-snapshot-") {
			t.Fatalf("temporary snapshot directory left behind: %s", e.Name())
		}
	}
}

// assertNoRestoreStaging pins the restore cleanup owner: the private upload
// file and the decoded database directory must not outlive the handler on any
// success or failure path.
func assertNoRestoreStaging(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "millivolt-restore-") {
			t.Fatalf("restore staging left behind: %s", e.Name())
		}
	}
}

// incompressibleText builds deterministic high-entropy text so a zstd frame
// keeps a database snapshot over the 1 MiB backup ceiling.
func incompressibleText(n int) string {
	var b strings.Builder
	sum := sha256.Sum256([]byte("backup-handler-cap"))
	for b.Len() < n {
		sum = sha256.Sum256(sum[:])
		b.WriteString(hex.EncodeToString(sum[:]))
	}
	return b.String()[:n]
}

func incompressibleBytes(seed string, n int) []byte {
	out := make([]byte, 0, n)
	sum := sha256.Sum256([]byte(seed))
	for len(out) < n {
		sum = sha256.Sum256(sum[:])
		out = append(out, sum[:]...)
	}
	return out[:n]
}

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
