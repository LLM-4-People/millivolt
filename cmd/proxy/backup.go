package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

func handleBackup(w http.ResponseWriter, r *http.Request) {
	if !rejectUnless(w, r, http.MethodGet) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "config", "database"); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	wantConfig, wantDB, err := parseBackupParts(q)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	arch, err := buildBackup(r.Context(), wantConfig, wantDB)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// A database member is staged for the self-check: capacity must follow
	// the database volume, not a generic /tmp sized independently of
	// backup_max_bytes.
	stageDir, ok := stageDirOr400(w, wantDB)
	if !ok {
		return
	}
	raw, err := backup.Encode(stageDir, arch)
	if err != nil {
		log.Printf("backup: encode failed: %v", err)
		adminjson.WriteErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if int64(len(raw)) > backupCap() {
		adminjson.WriteErrorJSON(w, http.StatusRequestEntityTooLarge, "backup exceeds backup_max_bytes")
		return
	}
	// The one artifact-header owner composes the name (millivolt-backup-<
	// UTC stamp>.mvb), disposition, content type and no-store.
	proxy.WriteArtifactHeaders(w, "backup", "mvb", "application/octet-stream", time.Now(), "")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
	_, _ = w.Write(raw)
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
	if err := adminjson.DuplicateQueryKey(q, "inspect", "config", "database", "config_mode", "database_mode"); err != nil {
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
	body := http.MaxBytesReader(w, r.Body, cap)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusRequestEntityTooLarge, "backup exceeds backup_max_bytes")
		return
	}
	arch, err := backup.Decode(raw)
	if err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// Inspect and validation stage a database member next to the live
	// database; a config-only archive never touches the database volume.
	stageDir, ok := stageDirOr400(w, len(arch.Database) > 0)
	if !ok {
		return
	}
	if err := backup.Validate(stageDir, arch); err != nil {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if inspect {
		writeRestoreInspect(w, r, arch, stageDir)
		return
	}
	if wantConfig && len(arch.Config) == 0 {
		adminjson.WriteErrorJSON(w, http.StatusBadRequest, "archive has no config member")
		return
	}
	if wantDB && len(arch.Database) == 0 {
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
		needRestart, err := restoreDatabase(r.Context(), arch.Database, dbMode)
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
	if _, ok := q[key]; !ok {
		return "replace", nil
	}
	switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
	case "", "replace":
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
// in hardened deployments and must not cap the backup contract.
func dbStageDir() (string, error) {
	liveMu.Lock()
	cfg := liveCfg
	liveMu.Unlock()
	if cfg == nil || cfg.DBPath == "" {
		return "", storage.ErrStorageDisabled
	}
	return filepath.Dir(cfg.DBPath), nil
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

func buildBackup(ctx context.Context, wantConfig, wantDB bool) (backup.Archive, error) {
	var a backup.Archive
	if wantConfig {
		liveMu.Lock()
		path := liveConfigPath
		liveMu.Unlock()
		if path == "" {
			return backup.Archive{}, fmt.Errorf("no config file; start with -config to back up settings")
		}
		cfg, err := config.LoadFile(path)
		if err != nil {
			return backup.Archive{}, err
		}
		var buf bytes.Buffer
		if err := config.WriteYAML(&buf, cfg); err != nil {
			return backup.Archive{}, err
		}
		a.Config = buf.Bytes()
	}
	if wantDB {
		liveMu.Lock()
		store := liveStore
		liveMu.Unlock()
		if store == nil {
			return backup.Archive{}, storage.ErrStorageDisabled
		}
		data, err := store.Snapshot(ctx)
		if err != nil {
			return backup.Archive{}, err
		}
		a.Database = data
	}
	return a, nil
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

func restoreDatabase(ctx context.Context, raw []byte, mode string) (restart bool, err error) {
	liveMu.Lock()
	cfg := liveCfg
	store := liveStore
	liveMu.Unlock()
	if cfg == nil || cfg.DBPath == "" {
		return false, storage.ErrStorageDisabled
	}
	if mode == "merge" {
		if store == nil {
			return false, storage.ErrStorageDisabled
		}
		_, _, err := store.MergeSnapshot(ctx, raw)
		return false, err
	}
	return true, storage.StageSnapshot(cfg.DBPath, raw)
}

func writeRestoreInspect(w http.ResponseWriter, r *http.Request, arch backup.Archive, stageDir string) {
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
	if len(arch.Database) > 0 {
		info, err := backup.InspectDatabase(stageDir, arch.Database)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		db := map[string]any{
			"present":   true,
			"bytes":     len(arch.Database),
			"requests":  info.Requests,
			"debug":     info.Debug,
			"oldest_ms": info.OldestMs,
			"newest_ms": info.NewestMs,
		}
		liveMu.Lock()
		store := liveStore
		liveMu.Unlock()
		if store != nil {
			n, err := store.SnapshotOverlap(r.Context(), arch.Database)
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
