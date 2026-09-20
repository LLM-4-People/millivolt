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
	"regexp"
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
// duplicates, unknown keys, then the value grammar.
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
		{"a duplicate outranks the unknown key", "/admin/restore?config=1&config=0&bogus=1", http.MethodPost, "duplicate config"},
		{"an unknown key outranks the value grammar", "/admin/restore?inspect=x&bogus=1", http.MethodPost, "unknown key bogus"},
		{"the value grammar answers last", "/admin/restore?inspect=x", http.MethodPost, "inspect: want 1 or 0"},
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
		Present  bool  `json:"present"`
		Bytes    int   `json:"bytes"`
		Requests int64 `json:"requests"`
		Debug    int64 `json:"debug"`
		OldestMs int64 `json:"oldest_ms"`
		NewestMs int64 `json:"newest_ms"`
		Overlap  int64 `json:"overlap"`
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
	if _, err := buildBackup(t.Context(), false, true); !errors.Is(err, storage.ErrStorageDisabled) {
		t.Errorf("buildBackup database member: %v, want ErrStorageDisabled", err)
	}
	if _, err := restoreDatabase(t.Context(), nil, "replace"); !errors.Is(err, storage.ErrStorageDisabled) {
		t.Errorf("restoreDatabase without db_path: %v, want ErrStorageDisabled", err)
	}

	// A configured db_path with no live store still refuses a merge with
	// the same identity.
	liveCfg = config.Default()
	liveCfg.DBPath = filepath.Join(t.TempDir(), "live.db")
	if _, err := restoreDatabase(t.Context(), nil, "merge"); !errors.Is(err, storage.ErrStorageDisabled) {
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
