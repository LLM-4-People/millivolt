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
	wantConfig, wantDB, err := parseBackupParts(r.URL.Query())
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
		if err := restoreConfig(arch.Config); err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if wantDB {
		if err := restoreDatabase(arch.Database); err != nil {
			logHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
		restart = append(restart, "db_path")
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

func restoreConfig(raw []byte) error {
	cfg, skipped, err := config.LoadBytes(raw)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		return fmt.Errorf("config backup dropped keys: %s", strings.Join(skipped, ", "))
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	liveMu.Lock()
	path := liveConfigPath
	liveMu.Unlock()
	if path == "" {
		return fmt.Errorf("no config file; start with -config to restore settings")
	}
	if err := config.WriteFile(path, cfg); err != nil {
		return err
	}
	_, err = reloadConfig()
	return err
}

func restoreDatabase(raw []byte) error {
	liveMu.Lock()
	cfg := liveCfg
	liveMu.Unlock()
	if cfg == nil || cfg.DBPath == "" {
		return fmt.Errorf("durable storage is disabled")
	}
	return storage.StageSnapshot(cfg.DBPath, raw)
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
	return map[string]any{
		"config":               path != "",
		"database":             store != nil,
		"pending_database":     pending,
		"restart_for_database": true,
	}
}
