package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/proxy"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func registerBackupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/backup", handleBackup)
	mux.HandleFunc("/admin/restore", handleRestore)
}

// The two mutating routes' consumed query-key sets: the duplicate gate and
// the unknown-key gate read the same list, so a future key cannot widen one
// gate without the other.
var (
	backupQueryKeys  = []string{"config", "database"}
	restoreQueryKeys = []string{"inspect", "config", "database", "config_mode", "database_mode"}
)

const (
	// restoreUploadPattern names one private upload staging file. The wildcard
	// keeps concurrent restores distinct; the handler removes it on every
	// path, including an upload read failure and a client disconnect.
	restoreUploadPattern = "millivolt-restore-upload-*.mvb"

	// restoreUploadBuffer bounds the streaming upload copy, so the request body
	// never becomes one heap-sized slice.
	restoreUploadBuffer = 32 << 10
)

func handleBackup(w http.ResponseWriter, r *http.Request) {
	if !rejectUnless(w, r, http.MethodGet) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, backupQueryKeys...); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// A mutating route rejects unknown keys outright: an ignored key would
	// silently select the default, broader action (?CONFIG=1 would broaden
	// the archive the same way). The gate order is parse error, duplicates,
	// unknown keys, then the value grammar.
	if err := adminjson.UnknownQueryKey(q, backupQueryKeys...); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	wantConfig, wantDB, err := parseBackupParts(q)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	arch, cleanup, err := buildBackup(r.Context(), wantConfig, wantDB)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// The database member is a private packed snapshot file. Remove it after
	// encoding, on every path: an encode error, an open/stream failure, or a
	// client disconnect. The encoded archive is independent of the source.
	defer cleanup()
	// A database member is staged for the self-check: capacity must follow
	// the database volume, not a generic /tmp sized independently of
	// backup_max_bytes. The artifact itself streams into that same staging
	// directory (the process temp directory for a config-only archive), so
	// the download never retains a second full archive copy in memory.
	stageDir, ok := stageDirOr400(w, wantDB)
	if !ok {
		return
	}
	path, size, err := backup.EncodeFile(stageDir, arch, backupCap())
	if err != nil {
		if errors.Is(err, backup.ErrTooLarge) {
			adminjson.WriteErrorJSON(w, http.StatusRequestEntityTooLarge, "backup exceeds backup_max_bytes")
			return
		}
		log.Printf("backup: encode failed: %v", err)
		adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// EncodeFile removed every failure path; the caller owns this file and
	// removes it after the response, including a client disconnect or a
	// write failure.
	defer os.Remove(path)
	f, err := os.Open(path)
	if err != nil {
		log.Printf("backup: open encoded archive: %v", err)
		adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	// The one artifact-header owner composes the name (millivolt-backup-<
	// UTC stamp>.mvb), disposition, content type and no-store.
	proxy.WriteArtifactHeaders(w, "backup", "mvb", "application/octet-stream", time.Now(), "")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("backup: download failed: %v", err)
	}
}

func handleRestore(w http.ResponseWriter, r *http.Request) {
	if !rejectUnless(w, r, http.MethodPost) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, restoreQueryKeys...); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// A mutating route rejects unknown keys outright: an ignored key would
	// silently select the default, broader action (?INSPECT=1 would run the
	// real restore where ?inspect=1 previews; ?CONFIG_MODE=merge would
	// silently become the replace default). The gate order is parse error,
	// duplicates, unknown keys, then the value grammar.
	if err := adminjson.UnknownQueryKey(q, restoreQueryKeys...); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	inspect, err := queryFlag(q, "inspect")
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "inspect: "+err.Error())
		return
	}
	wantConfig, wantDB, err := parseBackupParts(q)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	configMode, err := parseRestoreMode(q, "config_mode")
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	dbMode, err := parseRestoreMode(q, "database_mode")
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	cap := backupCap()
	// The upload is streamed into one private file, never read whole into heap.
	// A database archive stages that file on the live database's volume so the
	// compressed upload follows the same capacity contract as the decoded
	// member; a config-only restore without a live store may use the process
	// temp directory. The decoder owns the decoded database's staging
	// directory and removes it on every error.
	uploadDir := os.TempDir()
	decodeDir := ""
	if dir, err := dbStageDir(); err == nil {
		uploadDir, decodeDir = dir, dir
	}
	body := http.MaxBytesReader(w, r.Body, cap)
	defer body.Close()
	uploadPath, err := stageRestoreUpload(uploadDir, body, cap)
	if err != nil {
		// The endpoint's existing upload contract answers 413 for an over-cap
		// body and for a client that disappears mid-upload; local staging
		// failures stay server errors instead of being misreported as a cap.
		var readErr *restoreUploadReadError
		if errors.Is(err, backup.ErrTooLarge) || errors.As(err, &readErr) {
			adminjson.WriteErrorJSON(w, http.StatusRequestEntityTooLarge, "backup exceeds backup_max_bytes")
			return
		}
		log.Printf("restore: stage upload: %v", err)
		adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() {
		if err := os.Remove(uploadPath); err != nil && !os.IsNotExist(err) {
			log.Printf("restore: remove staged upload %s: %v", uploadPath, err)
		}
	}()
	arch, err := backup.DecodeFile(decodeDir, uploadPath)
	if err != nil {
		// Without a live store there is no volume for a decoded database
		// member; keep the endpoint's existing storage-disabled answer while a
		// config-only archive above still decodes and applies.
		if errors.Is(err, backup.ErrSnapshotStaging) {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, storage.ErrStorageDisabled.Error())
			return
		}
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// The decoded database member is one private staging directory. Remove it
	// after inspect or apply on every path, including a validation failure, an
	// apply failure, or a client disconnect while the response is written.
	if arch.DatabasePath != "" {
		defer func() {
			dir := filepath.Dir(arch.DatabasePath)
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("restore: remove decoded database staging %s: %v", dir, err)
			}
		}()
	}
	// A file-backed database member is integrity-checked in place, so the
	// legacy staging-directory argument is empty.
	if err := backup.Validate("", arch); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if inspect {
		writeRestoreInspect(w, r, arch)
		return
	}
	if wantConfig && len(arch.Config) == 0 {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "archive has no config member")
		return
	}
	if wantDB && len(arch.Database) == 0 && arch.DatabasePath == "" {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "archive has no database member")
		return
	}
	restart := []string{}
	if wantConfig {
		skipped, err := restoreConfig(arch.Config, configMode)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		restart = append(restart, skipped...)
	}
	if wantDB {
		needRestart, err := restoreDatabase(r.Context(), arch.DatabasePath, dbMode)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if needRestart {
			restart = append(restart, "db_path")
		}
	}
	// ok is the operator action plane's shared success marker (the dashboard
	// trusts the HTTP status; the restore tests pin it, tests/_go/cmd/proxy/backup.go).
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "restart_required": restart})
}

func parseBackupParts(q url.Values) (wantConfig, wantDB bool, err error) {
	_, hasC := q["config"]
	_, hasD := q["database"]
	if !hasC && !hasD {
		return true, true, nil
	}
	if hasC {
		wantConfig, err = queryBool(q.Get("config"))
		if err != nil {
			return false, false, fmt.Errorf("config: %w", err)
		}
	}
	if hasD {
		wantDB, err = queryBool(q.Get("database"))
		if err != nil {
			return false, false, fmt.Errorf("database: %w", err)
		}
	}
	if !wantConfig && !wantDB {
		return false, false, fmt.Errorf("select config, database, or both")
	}
	return wantConfig, wantDB, nil
}

func queryBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, fmt.Errorf("want 1 or 0")
	}
}

func queryFlag(q url.Values, key string) (bool, error) {
	if _, ok := q[key]; !ok {
		return false, nil
	}
	return queryBool(q.Get(key))
}

func parseRestoreMode(q url.Values, key string) (string, error) {
	if _, present := q[key]; !present {
		return "replace", nil
	}
	switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
	case "":
		// An explicit empty value is not the absent default: it names no mode,
		// so accepting it would silently select the broader replace action.
		return "", fmt.Errorf("%s: empty value; want merge or replace", key)
	case "replace":
		return "replace", nil
	case "merge":
		return "merge", nil
	default:
		return "", fmt.Errorf("%s: want merge or replace", key)
	}
}

func backupCap() int64 {
	liveMu.Lock()
	defer liveMu.Unlock()
	if liveCfg == nil {
		return int64(config.Default().BackupMaxBytes)
	}
	return int64(liveCfg.BackupMaxBytes)
}

// dbStageDir is where transient SQLite snapshot files are staged for operator
// backup and restore: the live database's own volume, contractually sized to
// hold the database and a packed copy of it. A generic /tmp is a small tmpfs
// in hardened deployments and must not cap the backup contract. The live
// database belongs to the store, not the reloadable config: db_path is
// startup-bound, so after a reload changes it the running store keeps its
// boot path, and the staging volume follows the store's own file. No store
// means no live database to size the contract against, so staging answers
// ErrStorageDisabled.
func dbStageDir() (string, error) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if liveStore == nil {
		return "", storage.ErrStorageDisabled
	}
	return filepath.Dir(liveStore.DBPath()), nil
}

// stageDirOr400 stages a database member for the self-check when one is
// requested, answering a staging failure with the endpoint's 400 JSON error.
// ok is false when the response is complete.
func stageDirOr400(w http.ResponseWriter, stage bool) (string, bool) {
	if !stage {
		return "", true
	}
	dir, err := dbStageDir()
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return dir, true
}

// restoreUploadReadError marks a failure while reading the request body, so
// the handler keeps the endpoint's existing 413 contract for a client that
// disconnects mid-upload while local staging write failures stay 500.
type restoreUploadReadError struct{ err error }

func (e *restoreUploadReadError) Error() string { return e.err.Error() }
func (e *restoreUploadReadError) Unwrap() error { return e.err }

// stageRestoreUpload copies the bounded request body into one private file and
// returns its path. The handler owns the file and removes it on every path. On
// error this owner removes its staging file and returns no path.
func stageRestoreUpload(dir string, body io.Reader, limit int64) (string, error) {
	if limit <= 0 {
		return "", fmt.Errorf("backup size limit is invalid")
	}
	f, err := os.CreateTemp(dir, restoreUploadPattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if err := copyRestoreUpload(f, body, limit); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	committed = true
	return path, nil
}

// copyRestoreUpload streams the body in bounded chunks and refuses the chunk
// that would cross the archive ceiling. http.MaxBytesReader owns the same cap
// at the transport; this count keeps a non-HTTP reader (tests and future
// callers) equally bounded.
func copyRestoreUpload(dst io.Writer, src io.Reader, limit int64) error {
	buf := make([]byte, restoreUploadBuffer)
	var n int64
	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			if n+int64(nr) > limit {
				return backup.ErrTooLarge
			}
			if err := writeRestoreChunk(dst, buf[:nr]); err != nil {
				return err
			}
			n += int64(nr)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(readErr, &maxErr) {
				return backup.ErrTooLarge
			}
			return &restoreUploadReadError{err: readErr}
		}
	}
}

func writeRestoreChunk(dst io.Writer, b []byte) error {
	n, err := dst.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// buildBackup assembles the requested members. A database member is staged as
// a private packed snapshot file (store.SnapshotFile) and returned by path, so
// backup.EncodeFile streams it in bounded chunks instead of the handler
// retaining a whole-database copy. The returned cleanup removes that snapshot
// directory and must run after encoding on every handler path; config-only
// returns a no-op. On error the snapshot owner has already cleaned up.
func buildBackup(ctx context.Context, wantConfig, wantDB bool) (backup.Archive, func(), error) {
	cleanup := func() {}
	var a backup.Archive
	if wantConfig {
		liveMu.Lock()
		path := liveConfigPath
		liveMu.Unlock()
		if path == "" {
			return backup.Archive{}, cleanup, fmt.Errorf("no config file; start with -config to back up settings")
		}
		cfg, err := config.LoadFile(path)
		if err != nil {
			return backup.Archive{}, cleanup, err
		}
		var buf bytes.Buffer
		if err := config.WriteYAML(&buf, cfg); err != nil {
			return backup.Archive{}, cleanup, err
		}
		a.Config = buf.Bytes()
	}
	if wantDB {
		liveMu.Lock()
		store := liveStore
		liveMu.Unlock()
		if store == nil {
			return backup.Archive{}, cleanup, storage.ErrStorageDisabled
		}
		path, remove, err := store.SnapshotFile(ctx)
		if err != nil {
			return backup.Archive{}, cleanup, err
		}
		cleanup = remove
		a.DatabasePath = path
	}
	return a, cleanup, nil
}

func restoreConfig(raw []byte, mode string) ([]string, error) {
	incoming, skipped, err := config.LoadBytes(raw)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("config backup dropped keys: %s", strings.Join(skipped, ", "))
	}
	if err := incoming.Validate(); err != nil {
		return nil, err
	}
	liveMu.Lock()
	path := liveConfigPath
	overrides := map[string]struct{}{}
	if liveListenOverride != "" {
		overrides["listen"] = struct{}{}
	}
	if liveDBOverride != "" {
		overrides["db_path"] = struct{}{}
	}
	liveMu.Unlock()
	if path == "" {
		return nil, fmt.Errorf("no config file; start with -config to restore settings")
	}
	cur, err := config.LoadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		cur = config.Default()
	}
	keep := map[string]any{}
	if len(overrides) > 0 {
		cm := cur.Map()
		for k := range overrides {
			keep[k] = cm[k]
		}
	}
	next := incoming.Clone()
	if mode == "merge" {
		if err := config.OverlayNonDefault(cur, incoming); err != nil {
			return nil, err
		}
		next = cur
	}
	if len(keep) > 0 {
		if err := next.Apply(keep); err != nil {
			return nil, err
		}
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := config.WriteFile(path, next); err != nil {
		return nil, err
	}
	return reloadConfig()
}

func restoreDatabase(ctx context.Context, path, mode string) (restart bool, err error) {
	liveMu.Lock()
	dbPath := ""
	if liveCfg != nil {
		dbPath = liveCfg.DBPath
	}
	store := liveStore
	liveMu.Unlock()
	if dbPath == "" {
		return false, storage.ErrStorageDisabled
	}
	if mode == "merge" {
		if store == nil {
			return false, storage.ErrStorageDisabled
		}
		_, _, err := store.MergeSnapshotFile(ctx, path)
		return false, err
	}
	// Replace stages next to the live CONFIG's db_path, not the running
	// store's boot file: the staged snapshot applies at the next restart,
	// which opens the persisted db_path (startup-bound), so the restore
	// targets the path that boot will actually open - a reload that changed
	// db_path stages the new volume while the store keeps the old one. The
	// decoded member streams into a destination-side temporary file, so the
	// database never returns to the heap.
	return true, storage.StageSnapshotFile(dbPath, path)
}

func writeRestoreInspect(w http.ResponseWriter, r *http.Request, arch backup.Archive) {
	// ok is the operator action plane's shared success marker (the dashboard
	// trusts the HTTP status; the inspect tests pin it, tests/_go/cmd/proxy/backup.go).
	out := map[string]any{
		"ok":      true,
		"created": arch.Created.UTC().Format(time.RFC3339),
	}
	if len(arch.Config) > 0 {
		cfg, skipped, err := config.LoadBytes(arch.Config)
		if err != nil || len(skipped) > 0 {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "backup config is invalid")
			return
		}
		liveMu.Lock()
		path := liveConfigPath
		liveMu.Unlock()
		live := config.Default()
		if path != "" {
			if cur, err := config.LoadFile(path); err == nil {
				live = cur
			}
		}
		out["config"] = map[string]any{
			"present":  true,
			"bytes":    len(arch.Config),
			"values":   cfg.Map(),
			"modified": config.DiffKeys(cfg, config.Default()),
			"vs_live":  config.DiffKeys(cfg, live),
		}
	} else {
		out["config"] = map[string]any{"present": false}
	}
	if arch.DatabasePath != "" {
		size, err := arch.DatabaseMemberSize()
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		info, err := backup.InspectDatabaseFile(arch.DatabasePath)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		db := map[string]any{
			"present":   true,
			"bytes":     size,
			"requests":  info.Requests,
			"debug":     info.Debug,
			"oldest_ms": info.OldestMs,
			"newest_ms": info.NewestMs,
		}
		liveMu.Lock()
		store := liveStore
		liveMu.Unlock()
		if store != nil {
			n, err := store.SnapshotOverlapFile(r.Context(), arch.DatabasePath)
			if err != nil {
				adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			db["overlap"] = n
		}
		out["database"] = db
	} else {
		out["database"] = map[string]any{"present": false}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func backupStatus() map[string]any {
	liveMu.Lock()
	path := liveConfigPath
	store := liveStore
	dbPath := ""
	if liveCfg != nil {
		dbPath = liveCfg.DBPath
	}
	liveMu.Unlock()
	pending := false
	if dbPath != "" {
		_, err := os.Stat(storage.PendingSnapshotPath(dbPath))
		pending = err == nil
	}
	var requests int64
	if store != nil {
		requests = store.Totals().Requests
	}
	return map[string]any{
		"config":           path != "",
		"database":         store != nil,
		"pending_database": pending,
		"requests":         requests,
	}
}
