package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
