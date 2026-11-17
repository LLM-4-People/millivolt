package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/plugins"
)

const (
	pluginActionBodyCap = 4096
	pluginUploadBodyCap = (32 << 20) + (1 << 20)
	pluginFormFileCap   = 32 << 20
	maxInboxEntries     = 16
)

var errInvalidUpload = errors.New("invalid plugin upload")

type inboxFailure struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

type inboxInstallReport struct {
	Scanned         int                     `json:"scanned"`
	Installed       []plugins.InstallReport `json:"installed"`
	Failed          []inboxFailure          `json:"failed,omitempty"`
	RestartRequired bool                    `json:"restart_required"`
}

// pluginStatusDoc is the management view. It carries no credential material:
// the credential file path is configuration, never a token.
type pluginStatusDoc struct {
	Status plugins.Status `json:"status"`
	Config pluginConfig   `json:"config"`
}

type pluginConfig struct {
	Enabled         bool   `json:"enabled"`
	Dir             string `json:"dir"`
	CredentialsFile string `json:"credentials_file"`
	TrustFile       string `json:"trust_file"`
	LockFile        string `json:"lock_file"`
	InboxDir        string `json:"inbox_dir"`
	Downloads       bool   `json:"downloads_enabled"`
}

// handlePlugins serves GET /admin/plugins and POST /admin/plugins with
// action=verify or action=pin. Enabling and disabling remain boot-time config
// changes; there is no route that pretends to hot-swap a WASM module.
func handlePlugins(runtime *plugins.Runtime, cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodGet:
			writePluginsJSON(w, pluginStatusDoc{
				Status: runtime.Status(),
				Config: pluginConfigFrom(cfg),
			})
		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, pluginActionBodyCap)
			var request struct {
				Action string `json:"action"`
			}
			if err := adminjson.Decode(w, r, &request); err != nil {
				adminjson.WriteError(w, http.StatusBadRequest, "invalid plugin request")
				return
			}
			switch request.Action {
			case "verify":
				report, err := runtime.Verify(cfg.PluginDir)
				if err != nil {
					adminjson.WriteError(w, http.StatusInternalServerError, err.Error())
					return
				}
				writePluginsJSON(w, report)
			case "pin":
				if cfg.PluginLockFile == "" {
					adminjson.WriteError(w, http.StatusBadRequest, "plugin_lock_file is not configured")
					return
				}
				pinned, err := runtime.Pin(cfg.PluginDir, cfg.PluginLockFile)
				if err != nil {
					adminjson.WriteError(w, http.StatusInternalServerError, err.Error())
					return
				}
				writePluginsJSON(w, struct {
					Pinned []string `json:"pinned"`
				}{Pinned: pinned})
			default:
				adminjson.WriteError(w, http.StatusBadRequest, "action must be verify or pin")
			}
		default:
			w.Header().Set("Allow", "GET, POST")
			adminjson.WriteError(w, http.StatusMethodNotAllowed, "GET or POST")
		}
	})
}

// handlePluginInstall accepts one multipart upload, one bounded JSON source
// reference, or action=install_inbox. All three use the core installer.
func handlePluginInstall(cfg *config.Config, source plugins.SourceFetcher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			adminjson.WriteErrorJSON(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		if cfg.PluginDir == "" || cfg.PluginLockFile == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "plugin_dir and plugin_lock_file must be configured")
			return
		}
		trust, err := plugins.LoadTrust(cfg.PluginTrustFile)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			r.Body = http.MaxBytesReader(w, r.Body, pluginUploadBodyCap)
			report, err := installUploadedPlugin(r, cfg, trust)
			if err != nil {
				adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			writePluginsJSON(w, report)
			return
		}

		var request struct {
			Source string `json:"source"`
			Action string `json:"action"`
		}
		if err := adminjson.Decode(w, r, &request); err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "invalid plugin request")
			return
		}
		if request.Action == "install_inbox" {
			if request.Source != "" {
				adminjson.WriteErrorJSON(w, http.StatusBadRequest, "source and action are mutually exclusive")
				return
			}
			report, err := scanPluginInbox(cfg, trust)
			if err != nil {
				adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
				return
			}
			writePluginsJSON(w, report)
			return
		}
		if request.Action != "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "unknown plugin action")
			return
		}
		if request.Source == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "source is required")
			return
		}
		if !cfg.PluginDownloadsEnabled {
			adminjson.WriteErrorJSON(w, http.StatusForbidden, "plugin downloads are disabled")
			return
		}
		report, err := plugins.InstallSource(r.Context(), request.Source, cfg.PluginDir, cfg.PluginLockFile, trust, source)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writePluginsJSON(w, report)
	})
}

// scanPluginInbox installs regular .mvplugin files from the configured inbox.
// Invalid files remain in place and are reported; symlinks are never followed.
func scanPluginInbox(cfg *config.Config, trust *plugins.Trust) (inboxInstallReport, error) {
	report := inboxInstallReport{Installed: []plugins.InstallReport{}}
	if cfg.PluginInboxDir == "" {
		return report, fmt.Errorf("plugin_inbox_dir is not configured")
	}
	if err := requirePluginPlainDir(cfg.PluginInboxDir); err != nil {
		return report, err
	}
	dir, err := os.Open(cfg.PluginInboxDir)
	if err != nil {
		return report, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(maxInboxEntries + 1)
	if err != nil && err != io.EOF {
		return report, err
	}
	if len(names) > maxInboxEntries {
		return report, fmt.Errorf("plugin inbox has more than %d entries", maxInboxEntries)
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".mvplugin") {
			continue
		}
		report.Scanned++
		if name != filepath.Base(name) || strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
			report.Failed = append(report.Failed, inboxFailure{File: name, Error: "invalid filename"})
			continue
		}
		path := filepath.Join(cfg.PluginInboxDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			report.Failed = append(report.Failed, inboxFailure{File: name, Error: err.Error()})
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			report.Failed = append(report.Failed, inboxFailure{File: name, Error: "not a regular file"})
			continue
		}
		installed, err := plugins.InstallFile(path, cfg.PluginDir, cfg.PluginLockFile, trust, true)
		if err != nil {
			report.Failed = append(report.Failed, inboxFailure{File: name, Error: err.Error()})
			continue
		}
		report.Installed = append(report.Installed, installed)
	}
	report.RestartRequired = len(report.Installed) > 0
	return report, nil
}

// handlePluginUninstall removes one installed archive and its state.
func handlePluginUninstall(cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			adminjson.WriteErrorJSON(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := adminjson.Decode(w, r, &request); err != nil || request.ID == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "id is required")
			return
		}
		if cfg.PluginDir == "" || cfg.PluginLockFile == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "plugin_dir and plugin_lock_file must be configured")
			return
		}
		if err := plugins.Uninstall(cfg.PluginDir, cfg.PluginLockFile, request.ID); err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writePluginsJSON(w, struct {
			ID              string `json:"id"`
			RestartRequired bool   `json:"restart_required"`
		}{ID: request.ID, RestartRequired: true})
	})
}

// handlePluginUpdatesCheck reports newer candidates without mutating files.
func handlePluginUpdatesCheck(cfg *config.Config, source plugins.SourceFetcher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			adminjson.WriteErrorJSON(w, http.StatusMethodNotAllowed, "GET")
			return
		}
		if !cfg.PluginDownloadsEnabled {
			adminjson.WriteErrorJSON(w, http.StatusForbidden, "plugin downloads are disabled")
			return
		}
		trust, err := plugins.LoadTrust(cfg.PluginTrustFile)
		if err != nil || trust == nil || !trust.Configured() {
			adminjson.WriteErrorJSON(w, http.StatusForbidden, "plugin_trust_file is required for remote plugin checks")
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "id is required")
			return
		}
		locks, err := plugins.LoadLocks(cfg.PluginLockFile)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		candidates, err := plugins.CheckUpdates(r.Context(), id, locks, source)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writePluginsJSON(w, struct {
			Candidates []plugins.UpdateCandidate `json:"candidates"`
		}{Candidates: candidates})
	})
}

// handlePluginUpdate fetches, validates and stages one signed plugin update.
func handlePluginUpdate(cfg *config.Config, source plugins.SourceFetcher) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			adminjson.WriteErrorJSON(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		if !cfg.PluginDownloadsEnabled {
			adminjson.WriteErrorJSON(w, http.StatusForbidden, "plugin downloads are disabled")
			return
		}
		trust, err := plugins.LoadTrust(cfg.PluginTrustFile)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		var request plugins.UpdateRequest
		if err := adminjson.Decode(w, r, &request); err != nil || request.ID == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "id is required")
			return
		}
		locks, err := plugins.LoadLocks(cfg.PluginLockFile)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		report, err := plugins.Update(r.Context(), request, cfg.PluginDir, cfg.PluginLockFile, locks, trust, source)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writePluginsJSON(w, report)
	})
}

// handlePluginRollback restores the recorded previous archive.
func handlePluginRollback(cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			adminjson.WriteErrorJSON(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		trust, err := plugins.LoadTrust(cfg.PluginTrustFile)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := adminjson.Decode(w, r, &request); err != nil || request.ID == "" {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, "id is required")
			return
		}
		locks, err := plugins.LoadLocks(cfg.PluginLockFile)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		report, err := plugins.Rollback(cfg.PluginDir, cfg.PluginLockFile, locks, request.ID, trust)
		if err != nil {
			adminjson.WriteErrorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		writePluginsJSON(w, report)
	})
}

func installUploadedPlugin(r *http.Request, cfg *config.Config, trust *plugins.Trust) (plugins.InstallReport, error) {
	var report plugins.InstallReport
	reader, err := r.MultipartReader()
	if err != nil {
		return report, err
	}
	var tempPath string
	fileCount := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return report, err
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		fileCount++
		if fileCount > 1 || !strings.HasSuffix(part.FileName(), ".mvplugin") {
			part.Close()
			return report, errInvalidUpload
		}
		tmp, err := os.CreateTemp("", "millivolt-plugin-upload-*.mvplugin")
		if err != nil {
			part.Close()
			return report, err
		}
		tempPath = tmp.Name()
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			part.Close()
			return report, err
		}
		written, err := io.Copy(tmp, io.LimitReader(part, pluginFormFileCap+1))
		closeErr := tmp.Close()
		part.Close()
		if err != nil {
			return report, err
		}
		if closeErr != nil {
			return report, closeErr
		}
		if written > pluginFormFileCap {
			return report, errInvalidUpload
		}
	}
	if tempPath == "" {
		return report, errInvalidUpload
	}
	defer os.Remove(tempPath)
	return plugins.InstallFile(tempPath, cfg.PluginDir, cfg.PluginLockFile, trust, true)
}

func requirePluginPlainDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a plain directory", path)
	}
	return nil
}

func pluginConfigFrom(cfg *config.Config) pluginConfig {
	return pluginConfig{
		Enabled:         cfg.PluginsEnabled,
		Dir:             cfg.PluginDir,
		CredentialsFile: cfg.PluginCredentialsFile,
		TrustFile:       cfg.PluginTrustFile,
		LockFile:        cfg.PluginLockFile,
		InboxDir:        cfg.PluginInboxDir,
		Downloads:       cfg.PluginDownloadsEnabled,
	}
}

func writePluginsJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("plugins: encode admin response: %v", err)
	}
}
