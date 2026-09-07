package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
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
	wantConfig, wantDB, err := parseBackupParts(r.URL.Query())
	if err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	arch, err := buildBackup(r.Context(), wantConfig, wantDB)
	if err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := backup.Encode(arch)
	if err != nil {
		logHTTPError(w, "backup failed", http.StatusInternalServerError)
		return
	}
	if int64(len(raw)) > backupCap() {
		logHTTPError(w, "backup exceeds backup_max_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	name := "millivolt-backup-" + time.Now().UTC().Format("20060102-150405") + ".mvb"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
	_, _ = w.Write(raw)
}

func handleRestore(w http.ResponseWriter, r *http.Request) {
	if !rejectUnless(w, r, http.MethodPost) {
		return
	}
	q := r.URL.Query()
	inspect, err := queryFlag(q, "inspect")
	if err != nil {
		logHTTPError(w, "inspect: "+err.Error(), http.StatusBadRequest)
		return
	}
	wantConfig, wantDB, err := parseBackupParts(q)
	if err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	configMode, err := parseRestoreMode(q, "config_mode")
	if err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	dbMode, err := parseRestoreMode(q, "database_mode")
	if err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cap := backupCap()
	body := http.MaxBytesReader(w, r.Body, cap)
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		logHTTPError(w, "backup exceeds backup_max_bytes", http.StatusRequestEntityTooLarge)
		return
	}
	arch, err := backup.Decode(raw)
	if err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := backup.Validate(arch); err != nil {
		logHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if inspect {
		writeRestoreInspect(w, r, arch)
		return
	}
	if wantConfig && len(arch.Config) == 0 {
		logHTTPError(w, "archive has no config member", http.StatusBadRequest)
		return
	}
	if wantDB && len(arch.Database) == 0 {
		logHTTPError(w, "archive has no database member", http.StatusBadRequest)
		return
	}
	restart := []string{}
	if wantConfig {
		skipped, err := restoreConfig(arch.Config, configMode)
		if err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
		restart = append(restart, skipped...)
	}
	if wantDB {
		needRestart, err := restoreDatabase(r.Context(), arch.Database, dbMode)
		if err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if needRestart {
			restart = append(restart, "db_path")
		}
	}
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
			return backup.Archive{}, fmt.Errorf("durable storage is disabled")
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
		return false, fmt.Errorf("durable storage is disabled")
	}
	if mode == "merge" {
		if store == nil {
			return false, fmt.Errorf("durable storage is disabled")
		}
		_, _, err := store.MergeSnapshot(ctx, raw)
		return false, err
	}
	return true, storage.StageSnapshot(cfg.DBPath, raw)
}

func writeRestoreInspect(w http.ResponseWriter, r *http.Request, arch backup.Archive) {
	out := map[string]any{
		"ok":      true,
		"inspect": true,
		"created": arch.Created.UTC().Format(time.RFC3339),
	}
	if len(arch.Config) > 0 {
		cfg, skipped, err := config.LoadBytes(arch.Config)
		if err != nil || len(skipped) > 0 {
			logHTTPError(w, "backup config is invalid", http.StatusBadRequest)
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
		info, err := backup.InspectDatabase(arch.Database)
		if err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
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
				logHTTPError(w, err.Error(), http.StatusBadRequest)
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
		"config":               path != "",
		"database":             store != nil,
		"pending_database":     pending,
		"restart_for_database": true,
		"requests":             requests,
	}
}
