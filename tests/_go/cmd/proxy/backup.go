package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
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

// openLiveStoreFixture is the shared live-store prologue of the four
// restore tests below: it saves liveCfg/liveStore for cleanup restore,
// makes the temp dir, derives the storage options from config.Default(),
// opens the store at <dir>/live.db, and owns its Close cleanup. It returns
// the opened store, the options (a site's second store reuses them), the
// store path, and the temp dir; every per-site extra stays at its site.
func openLiveStoreFixture(t *testing.T) (store *storage.Store, opts storage.Options, path, dir string) {
	t.Helper()
	oldCfg, oldStore := liveCfg, liveStore
	t.Cleanup(func() { liveCfg, liveStore = oldCfg, oldStore })
	dir = t.TempDir()
	opts = storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  config.Default().StorageQueryMaxRows,
	}
	path = filepath.Join(dir, "live.db")
	store, err := storage.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, opts, path, dir
}

// TestRestoreFixtureRestoresGlobals pins openLiveStoreFixture's save-restore
// contract: it saves liveCfg/liveStore before opening the store, and its
// cleanup must return the globals to those pre-fixture values when the test
// ends. The verification cleanup registers BEFORE the fixture call, so LIFO
// runs it after the fixture's own restore - dropping the restore reddens
// here, and so would a cleanup registered in the wrong order.
func TestRestoreFixtureRestoresGlobals(t *testing.T) {
	preCfg, preStore := liveCfg, liveStore
	t.Cleanup(func() {
		if liveCfg != preCfg {
			t.Error("openLiveStoreFixture left liveCfg assigned: the save-restore cleanup is missing or out of order")
		}
		if liveStore != preStore {
			t.Error("openLiveStoreFixture left liveStore assigned: the save-restore cleanup is missing or out of order")
		}
	})
	store, _, dbPath, _ := openLiveStoreFixture(t)
	// Every real site assigns the globals mid-test (the restore handlers read
	// them); the row does the same so the restore has something to undo.
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = store
}

// TestRestoreFixtureOptsDeriveFromDefault pins the helper's Options
// derivation: the four Default-derived fields must track config.Default()
// (the channel and batch caps are the helper's own literals), so switching
// the helper to literal opts reddens here.
func TestRestoreFixtureOptsDeriveFromDefault(t *testing.T) {
	_, opts, _, _ := openLiveStoreFixture(t)
	d := config.Default()
	if opts.FlushInterval != d.StorageFlushInterval {
		t.Errorf("FlushInterval = %v, want config.Default().StorageFlushInterval (%v)", opts.FlushInterval, d.StorageFlushInterval)
	}
	if opts.QueryTimeout != d.StorageQueryTimeout {
		t.Errorf("QueryTimeout = %v, want config.Default().StorageQueryTimeout (%v)", opts.QueryTimeout, d.StorageQueryTimeout)
	}
	if opts.QueryMaxBytes != int(d.StorageQueryMaxBytes) {
		t.Errorf("QueryMaxBytes = %d, want config.Default().StorageQueryMaxBytes (%d)", opts.QueryMaxBytes, d.StorageQueryMaxBytes)
	}
	if opts.QueryMaxRows != d.StorageQueryMaxRows {
		t.Errorf("QueryMaxRows = %d, want config.Default().StorageQueryMaxRows (%d)", opts.QueryMaxRows, d.StorageQueryMaxRows)
	}
}

func TestRestoreValidatesWholeArchiveBeforeDatabaseApply(t *testing.T) {
	src, _, dbPath, dir := openLiveStoreFixture(t)
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw := decodeableArchive(t, 1|2, []byte{1, 2}, []byte("listen: 8080\n"), data)
	decoded, err := backup.Decode(raw)
	if err != nil {
		t.Fatalf("fixture must decode: %v", err)
	}
	if err := backup.Validate(dir, decoded); err == nil {
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
	assertNoRestoreStaging(t, dir)
}

func TestRestoreStagesDatabaseFromEncodedArchive(t *testing.T) {
	src, _, dbPath, dir := openLiveStoreFixture(t)
	src.Record(&metrics.Record{ID: "via-http", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Database: data})
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
	assertNoRestoreStaging(t, dir)
}

// TestRestoreOverCapUploadLeavesNoStaging pins the upload ceiling end to end:
// an over-cap body answers the existing 413 message and the private upload
// file is removed.
func TestRestoreOverCapUploadLeavesNoStaging(t *testing.T) {
	store, _, dbPath, dir := openLiveStoreFixture(t)
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveCfg.BackupMaxBytes = config.BackupMaxBytesMin
	liveStore = store
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	body := bytes.Repeat([]byte{0x41}, int(config.BackupMaxBytesMin)+1)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d body %s, want 413", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "backup exceeds backup_max_bytes") {
		t.Fatalf("body %q does not name the cap", rr.Body.String())
	}
	assertNoRestoreStaging(t, dir)
}

// TestRestoreUploadReadFailureLeavesNoStaging pins the client-disconnect path:
// a body that fails mid-read answers the endpoint's upload error and leaves no
// private upload file behind.
func TestRestoreUploadReadFailureLeavesNoStaging(t *testing.T) {
	store, _, dbPath, dir := openLiveStoreFixture(t)
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = store
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1",
		io.MultiReader(bytes.NewReader([]byte("MVB1")), failingReader{}))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d body %s, want the upload 413", rr.Code, rr.Body.String())
	}
	assertNoRestoreStaging(t, dir)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("client disconnected") }

// TestStageRestoreUploadEnforcesCap pins the staging owner directly: the
// bounded copy refuses an over-cap reader, returns no path, and removes its
// private file.
func TestStageRestoreUploadEnforcesCap(t *testing.T) {
	dir := t.TempDir()
	path, err := stageRestoreUpload(dir, bytes.NewReader(bytes.Repeat([]byte("x"), 1025)), 1024)
	if !errors.Is(err, backup.ErrTooLarge) {
		t.Fatalf("over-cap staging = %v, want ErrTooLarge", err)
	}
	if path != "" {
		t.Fatalf("over-cap staging returned path %q", path)
	}
	assertNoRestoreStaging(t, dir)

	path, err = stageRestoreUpload(dir, bytes.NewReader([]byte("MVB1")), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertNoRestoreStaging(t, dir)
}

// TestRestoreResponseWriteFailureLeavesNoStaging pins the client-disconnect
// path after a successful decode and inspect: the response write fails, and
// the upload file plus decoded database directory are still removed.
func TestRestoreResponseWriteFailureLeavesNoStaging(t *testing.T) {
	store, _, dbPath, dir := openLiveStoreFixture(t)
	store.Record(&metrics.Record{ID: "write-failure", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Database: data})
	if err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = dbPath
	liveStore = store
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	mux.ServeHTTP(&failingWriter{}, httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1", bytes.NewReader(raw)))
	assertNoRestoreStaging(t, dir)
}

// TestRestoreConfigOnlyWithoutStoreUsesTempAndCleans pins a config-only
// restore without a live database: it stages the upload in the process temp
// directory and removes it after the config applies.
func TestRestoreConfigOnlyWithoutStoreUsesTempAndCleans(t *testing.T) {
	liveReloadFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	raw, err := backup.Encode("", backup.Archive{Config: defaultConfigYAMLForRestore(t)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("restore status %d body %s", rr.Code, rr.Body.String())
	}
	assertNoRestoreStaging(t, temp)
}

// TestRestoreDatabaseWithoutStoreFailsBeforeStaging pins the missing-volume
// refusal for a database archive and its cleanup: no live database means no
// decoding volume, and the temp upload is removed.
func TestRestoreDatabaseWithoutStoreFailsBeforeStaging(t *testing.T) {
	liveReloadFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	dir := t.TempDir()
	src, err := storage.Open(filepath.Join(dir, "src.db"), storage.Options{
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
	src.Record(&metrics.Record{ID: "no-volume", Provider: "neutral.example", Model: "n", StatusCode: 200})
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
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?database=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), storage.ErrStorageDisabled.Error()) {
		t.Fatalf("body %q does not report the missing storage volume", rr.Body.String())
	}
	assertNoRestoreStaging(t, temp)
}

// restoreDatabaseSnapshot builds one packed database member with a single
// request, for mixed archives that must be decoded without a live store.
func restoreDatabaseSnapshot(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	src, err := storage.Open(filepath.Join(dir, "src.db"), storage.Options{
		WriteChanCap:  8,
		BatchCap:      4,
		FlushInterval: config.Default().StorageFlushInterval,
		QueryTimeout:  config.Default().StorageQueryTimeout,
		QueryMaxBytes: int(config.Default().StorageQueryMaxBytes),
		QueryMaxRows:  int(config.Default().StorageQueryMaxRows),
	})
	if err != nil {
		t.Fatal(err)
	}
	src.Record(&metrics.Record{ID: "mixed-member", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRestoreMixedArchiveConfigOnlyWithoutStore pins G5: a mixed archive can
// apply or preview its config member when durable storage is disabled, while a
// database request still fails closed.
func TestRestoreMixedArchiveConfigOnlyWithoutStore(t *testing.T) {
	liveReloadFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	dir := t.TempDir()
	bak := config.Default()
	bak.CaptureBodyPreview = true
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, bak); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Config: yaml.Bytes(), Database: restoreDatabaseSnapshot(t)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?config=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("config-only restore status %d body %s", rr.Code, rr.Body.String())
	}
	got, err := config.LoadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CaptureBodyPreview {
		t.Fatalf("config-only restore did not write the archive config: %+v", got)
	}
	assertNoRestoreStaging(t, temp)

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?database=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), storage.ErrStorageDisabled.Error()) {
		t.Fatalf("database-only status %d body %s, want 400 storage disabled", rr.Code, rr.Body.String())
	}
	assertNoRestoreStaging(t, temp)
}

// TestRestoreInspectMixedArchiveWithoutStore pins the honest store-less
// preview: config keys and diff are available, the database member reports its
// decoded size but is not inspectable and has no request span.
func TestRestoreInspectMixedArchiveWithoutStore(t *testing.T) {
	liveReloadFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	dir := t.TempDir()
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
	db := restoreDatabaseSnapshot(t)
	raw, err := backup.Encode(dir, backup.Archive{Config: yaml.Bytes(), Database: db})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/restore?inspect=1", bytes.NewReader(raw)))
	if rr.Code != http.StatusOK {
		t.Fatalf("inspect status %d body %s", rr.Code, rr.Body.String())
	}
	ins := decodeRestoreInspectStrict(t, rr.Body.Bytes())
	if !ins.OK || !ins.Config.Present || ins.Config.Values["capture_body_preview"] != true {
		t.Fatalf("config preview = %+v", ins.Config)
	}
	vsLive := map[string]bool{}
	for _, key := range ins.Config.VsLive {
		vsLive[key] = true
	}
	if !vsLive["capture_body_preview"] || !vsLive["max_retries"] {
		t.Fatalf("vs_live = %v, want capture_body_preview and max_retries", ins.Config.VsLive)
	}
	if !ins.Database.Present || ins.Database.Bytes != len(db) {
		t.Fatalf("database preview = %+v, want present with %d bytes", ins.Database, len(db))
	}
	if ins.Database.Inspectable == nil || *ins.Database.Inspectable {
		t.Fatalf("database inspectable = %v, want false", ins.Database.Inspectable)
	}
	if ins.Database.Reason == "" || ins.Database.Requests != 0 || ins.Database.Overlap != 0 ||
		ins.Database.OldestMs != 0 || ins.Database.NewestMs != 0 {
		t.Fatalf("store-less database preview = %+v", ins.Database)
	}
	assertNoRestoreStaging(t, temp)
}

func defaultConfigYAMLForRestore(t *testing.T) []byte {
	t.Helper()
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	return yaml.Bytes()
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

// TestParseRestoreModePresence pins that an absent key keeps the documented
// replace default while an explicit empty or whitespace value is a malformed
// mode, never a silent replace.
func TestParseRestoreModePresence(t *testing.T) {
	for _, key := range []string{"config_mode", "database_mode"} {
		for _, tc := range []struct {
			name    string
			q       url.Values
			want    string
			wantErr bool
		}{
			{"absent", url.Values{}, "replace", false},
			{"empty", url.Values{key: {""}}, "", true},
			{"whitespace", url.Values{key: {" \t "}}, "", true},
			{"merge", url.Values{key: {"merge"}}, "merge", false},
			{"replace", url.Values{key: {"replace"}}, "replace", false},
			{"uppercase", url.Values{key: {" MERGE "}}, "merge", false},
			{"invalid", url.Values{key: {"bogus"}}, "", true},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				got, err := parseRestoreMode(tc.q, key)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), key) {
						t.Fatalf("parseRestoreMode(%q) = %q, %v, want a %s error", tc.name, got, err, key)
					}
					return
				}
				if err != nil || got != tc.want {
					t.Fatalf("parseRestoreMode(%q) = %q, %v, want %q", tc.name, got, err, tc.want)
				}
			})
		}
	}
}

// TestBackupRestoreAdoptStrictQuery pins the destructive plane's strict-query
// adoption: both routes parse the raw query string, so a malformed pair can
// never be silently dropped into a broader action (inspect=%zz must not turn
// a would-be 400 into a real restore; config=1&database=%zz must not
// silently skip the database member), every repeated consumed flag is
// denied, never first-wins, and every key outside the consumed set is
// rejected outright - the ignore class belongs to idempotent fetches, while
// on a mutating route an unknown or case-variant key silently selects the
// default, broader action (?INSPECT=1 would run the real restore where
// ?inspect=1 previews). The fixture archive carries both members so each
// dropped or ignored pair would otherwise change what the restore really
// does. Each row also pins the 400's wording through the routes' JSON error
// transport, and the boundary rows pin the gate order: parse error,
// duplicates, unknown keys, then the value grammar - whose tail rows pin
// the member and mode 400s exactly, an invalid mode spelling answering the
// 400 rather than silently selecting the broader replace default.
func TestBackupRestoreAdoptStrictQuery(t *testing.T) {
	liveReloadFixture(t)
	// A non-default spelling so the closing invariant can see a silent
	// restore: the fixture archive carries the default config, so a crafted
	// query that slips past the gates rewrites these bytes.
	start := liveCfg.Clone()
	start.MaxRetries = 9
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	live, _, livePath, dir := openLiveStoreFixture(t)
	live.Record(&metrics.Record{ID: "strict-row", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := live.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, config.Default()); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode(dir, backup.Archive{Config: yamlBuf.Bytes(), Database: data})
	if err != nil {
		t.Fatal(err)
	}
	liveCfg = config.Default()
	liveCfg.DBPath = livePath
	liveStore = live
	beforeConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)

	for _, tc := range []struct {
		name, target, method, want string
	}{
		{"restore inspect=%zz must not become a real restore", "/admin/restore?inspect=%zz", http.MethodPost, "invalid query"},
		{"restore config=1&database=%zz must not skip the database member", "/admin/restore?config=1&database=%zz", http.MethodPost, "invalid query"},
		{"repeated inspect flag is denied, never first-wins", "/admin/restore?inspect=1&inspect=0", http.MethodPost, "duplicate inspect"},
		{"repeated config_mode is denied, never first-wins", "/admin/restore?config_mode=merge&config_mode=replace", http.MethodPost, "duplicate config_mode"},
		{"repeated database_mode is denied, never first-wins", "/admin/restore?database_mode=merge&database_mode=replace", http.MethodPost, "duplicate database_mode"},
		{"backup config=1&database=%zz must not silently become config-only", "/admin/backup?config=1&database=%zz", http.MethodGet, "invalid query"},
		{"repeated backup config flag is denied, never first-wins", "/admin/backup?config=1&config=0", http.MethodGet, "duplicate config"},
		{"restore INSPECT=1 must not run the real restore as the default action", "/admin/restore?INSPECT=1", http.MethodPost, "unknown key INSPECT"},
		{"restore CONFIG_MODE=merge must not silently become the replace default", "/admin/restore?CONFIG_MODE=merge", http.MethodPost, "unknown key CONFIG_MODE"},
		{"backup CONFIG=1 must not silently broaden the archive", "/admin/backup?CONFIG=1", http.MethodGet, "unknown key CONFIG"},
		{"restore bogus=1 is an unknown key, not an ignored one", "/admin/restore?bogus=1", http.MethodPost, "unknown key bogus"},
		{"backup bogus=1 is an unknown key, not an ignored one", "/admin/backup?bogus=1", http.MethodGet, "unknown key bogus"},
		{"a parse error outranks the unknown key", "/admin/restore?bogus=1&bad=%zz", http.MethodPost, "invalid query"},
		{"a restore parse error outranks the duplicate key", "/admin/restore?config=1&config=0&bad=%zz", http.MethodPost, "invalid query"},
		{"a duplicate outranks the unknown key", "/admin/restore?config=1&config=0&bogus=1", http.MethodPost, "duplicate config"},
		{"an unknown key outranks the value grammar", "/admin/restore?inspect=x&bogus=1", http.MethodPost, "unknown key bogus"},
		{"the value grammar answers last", "/admin/restore?inspect=x", http.MethodPost, "inspect: want 1 or 0"},
		{"an invalid restore config member answers the member grammar's 400", "/admin/restore?config=x", http.MethodPost, "config: want 1 or 0"},
		{"an invalid restore database member answers the member grammar's 400", "/admin/restore?database=x", http.MethodPost, "database: want 1 or 0"},
		{"an invalid config_mode answers the 400, never a silent replace", "/admin/restore?config_mode=bogus", http.MethodPost, "config_mode: want merge or replace"},
		{"an invalid database_mode answers the 400, never a silent replace", "/admin/restore?database_mode=bogus", http.MethodPost, "database_mode: want merge or replace"},
		{"an explicit empty config_mode is denied, never the replace default", "/admin/restore?config_mode=", http.MethodPost, "config_mode: empty value; want merge or replace"},
		{"an explicit whitespace database_mode is denied, never the replace default", "/admin/restore?database_mode=%20", http.MethodPost, "database_mode: empty value; want merge or replace"},
		{"a backup parse error outranks the duplicate key", "/admin/backup?config=1&config=0&bad=%zz", http.MethodGet, "invalid query"},
		{"a backup duplicate outranks the unknown key", "/admin/backup?config=1&config=0&bogus=1", http.MethodGet, "duplicate config"},
		{"a backup unknown key outranks the value grammar", "/admin/backup?config=x&bogus=1", http.MethodGet, "unknown key bogus"},
		{"an invalid backup config member answers the member grammar's 400", "/admin/backup?config=x", http.MethodGet, "config: want 1 or 0"},
		{"an invalid backup database member answers the member grammar's 400", "/admin/backup?database=x", http.MethodGet, "database: want 1 or 0"},
	} {
		w := httptest.NewRecorder()
		var body io.Reader
		if tc.method == http.MethodPost {
			body = bytes.NewReader(raw)
		}
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.target, body))
		var errBody struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
			t.Errorf("%s: body is not the flat error shape: %v (%q)", tc.name, err, w.Body.String())
			continue
		}
		if w.Code != http.StatusBadRequest || errBody.Error != tc.want {
			t.Errorf("%s: status=%d error=%q, want 400 %q", tc.name, w.Code, errBody.Error, tc.want)
		}
	}
	// No crafted query may reach a destructive side effect: the config file
	// stays byte-identical and no database snapshot is staged.
	afterConfig, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatal("a crafted query rewrote the live config file")
	}
	if _, err := os.Stat(storage.PendingSnapshotPath(livePath)); !os.IsNotExist(err) {
		t.Fatal("a crafted query staged a database snapshot")
	}
}

// TestRestorePercentDecodedConsumedKeyIsConsumed is the percent-decoding freeze
// row: a percent-decoded spelling of a consumed key (?%63onfig=1) IS the
// consumed key, never an unknown one, so the restore runs exactly as the
// canonical ?config=1 does. The canonical spelling's behavior is pinned
// collectively: the route-level round-trip tests pin the restore 200s and
// the applied config, while the ok-marker pins live in the other-spelling
// rows (the database-stage and merge restores). A strict parse that stopped
// decoding keys would answer the unknown-key 400 instead of restoring.
func TestRestorePercentDecodedConsumedKeyIsConsumed(t *testing.T) {
	liveReloadFixture(t)
	start := config.Default()
	start.MaxRetries = 9
	if err := config.WriteFile(liveConfigPath, start); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	arch := config.Default()
	arch.MaxRetries = 2
	var yamlBuf bytes.Buffer
	if err := config.WriteYAML(&yamlBuf, arch); err != nil {
		t.Fatal(err)
	}
	raw, err := backup.Encode("", backup.Archive{Config: yamlBuf.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerBackupRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/restore?%63onfig=1", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("decoded spelling status %d body %s, want the 200 the canonical ?config=1 answers (the decoded key is consumed, not unknown)", rr.Code, rr.Body.String())
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || !body.OK {
		t.Fatalf("restore body %s is not the ok marker the canonical spelling answers", rr.Body.String())
	}
	if liveCfg.MaxRetries != 2 {
		t.Fatalf("the decoded spelling did not run the real restore: max_retries=%d, want the archive's 2", liveCfg.MaxRetries)
	}
}

// TestBackupRestoreConsumedQueryKeysPinned is the consumed-set source pin:
// nothing bounded the two lists' contents, so widening restoreQueryKeys by
// one key would silently re-admit the ignore class wave 24 removed. The pin
// reads cmd/proxy/backup.go's source text (the package dir is the test
// binary's working directory, the same relative-read convention
// dev_config.go uses for scripts/dev.sh), asserts the exact list literals,
// confirms the parsed literals against the live package variables so a
// stale parse can never pass, and requires each route's two gates
// (DuplicateQueryKey and UnknownQueryKey) to be called exactly once with
// the same list variable, so the set cannot drift between the gates.
func TestBackupRestoreConsumedQueryKeysPinned(t *testing.T) {
	raw, err := os.ReadFile("backup.go")
	if err != nil {
		t.Fatalf("read cmd/proxy/backup.go: %v", err)
	}
	src := string(raw)
	region := func(decl string) string {
		t.Helper()
		start := strings.Index(src, decl)
		if start < 0 {
			t.Fatalf("cmd/proxy/backup.go is missing %q", decl)
		}
		rest := src[start:]
		end := strings.Index(rest, "\n}")
		if end < 0 {
			t.Fatalf("%q is not terminated by a line-anchored closing brace", decl)
		}
		return rest[:end]
	}
	listLiteral := func(name string) []string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^\t` + name + `\s*=\s*\[\]string\{([^}]*)\}`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s is not a top-level []string literal in cmd/proxy/backup.go", name)
		}
		quotes := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1)
		if len(quotes) == 0 {
			t.Fatalf("%s declares no keys", name)
		}
		keys := make([]string, 0, len(quotes))
		for _, q := range quotes {
			keys = append(keys, q[1])
		}
		return keys
	}
	wantBackup := []string{"config", "database"}
	wantRestore := []string{"inspect", "config", "database", "config_mode", "database_mode"}
	if got := listLiteral("backupQueryKeys"); !slices.Equal(got, wantBackup) {
		t.Errorf("backupQueryKeys source literal = %v, want exactly %v", got, wantBackup)
	}
	if !slices.Equal(backupQueryKeys, wantBackup) {
		t.Errorf("live backupQueryKeys = %v, want exactly %v (the source pin and the variable disagree)", backupQueryKeys, wantBackup)
	}
	if got := listLiteral("restoreQueryKeys"); !slices.Equal(got, wantRestore) {
		t.Errorf("restoreQueryKeys source literal = %v, want exactly %v", got, wantRestore)
	}
	if !slices.Equal(restoreQueryKeys, wantRestore) {
		t.Errorf("live restoreQueryKeys = %v, want exactly %v (the source pin and the variable disagree)", restoreQueryKeys, wantRestore)
	}
	for _, route := range []struct{ decl, list string }{
		{"func handleBackup(", "backupQueryKeys"},
		{"func handleRestore(", "restoreQueryKeys"},
	} {
		body := region(route.decl)
		calls := map[string]string{}
		for _, gate := range []string{"adminjson.DuplicateQueryKey", "adminjson.UnknownQueryKey"} {
			matches := regexp.MustCompile(regexp.QuoteMeta(gate)+`\(([^)]*)\)`).FindAllStringSubmatch(body, -1)
			if len(matches) != 1 {
				t.Errorf("%s calls %s %d times, want exactly one consumed-set gate call", route.decl, gate, len(matches))
				continue
			}
			args := strings.Split(matches[0][1], ",")
			if len(args) != 2 {
				t.Errorf("%s's %s call takes %d arguments, want the parsed query and one list", route.decl, gate, len(args))
				continue
			}
			calls[gate] = strings.TrimSpace(args[1])
		}
		want := route.list + "..."
		if calls["adminjson.DuplicateQueryKey"] != want || calls["adminjson.UnknownQueryKey"] != want {
			t.Errorf("%s gates read %q and %q, want both to pass %s", route.decl,
				calls["adminjson.DuplicateQueryKey"], calls["adminjson.UnknownQueryKey"], want)
		}
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
