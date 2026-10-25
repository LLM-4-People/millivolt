package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

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

// TestStorelessInspectDocsMatchTheWire pins the user-visible store-less
// preview contract to the route and restore prose: presence and decoded size
// are reported, the member is not inspectable and has no request span, and a
// database-selecting apply fails closed before staging.
func TestStorelessInspectDocsMatchTheWire(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(doc)), " ")
	for _, want := range []string{
		"A store-less preview still reports the member's presence and decoded size with `inspectable:false` and the reason.",
		"a restore that selects the database member without a store fails closed before staging",
		"Without a live store it reports `present:true`, the decoded `bytes`, `inspectable:false` and the reason, and produces no request span.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/operations.md is missing the store-less contract %q", want)
		}
	}
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
