package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Status is the read-only management view of the boot-loaded plugin set.
// It contains no credential material and no guest-supplied markup.
type Status struct {
	Enabled     bool           `json:"enabled"`
	Modules     []ModuleStatus `json:"modules"`
	Descriptors []ModuleUI     `json:"descriptors,omitempty"`
}

type ModuleStatus struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	ABI          string   `json:"abi"`
	Module       string   `json:"module"`
	License      string   `json:"license"`
	Capabilities []string `json:"capabilities"`
	Failures     uint64   `json:"failures"`
}

type ModuleUI struct {
	PluginID string    `json:"plugin_id"`
	Tools    []ToolUI  `json:"tools,omitempty"`
	Panels   []PanelUI `json:"panels,omitempty"`
}

type ToolUI struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
}

type PanelUI struct {
	ID    string            `json:"id"`
	Title string            `json:"title"`
	Data  map[string]string `json:"data,omitempty"`
}

type guestOutput struct {
	Tools  []ToolUI  `json:"tools"`
	Panels []PanelUI `json:"panels"`
}

const (
	maxDescriptorTools     = 16
	maxDescriptorPanels    = 16
	maxPanelDataEntries    = 24
	maxPanelDataKeyBytes   = 64
	maxPanelDataValueBytes = 256
)

// sanitizePanelData copies bounded string data for core rendering. Keys and
func sanitizePanelData(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	preferred := []string{
		"provider", "requests", "models", "input_tokens", "output_tokens", "cost",
		"rate_limited", "remaining", "last_model", "last_status", "resets",
		"model_list", "config_schema", "config_values", "capability_set",
	}
	keys := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	add := func(key string) {
		value, exists := raw[key]
		if !exists || seen[key] || len(key) == 0 || len(key) > maxPanelDataKeyBytes ||
			len(value) > maxPanelDataValueBytes {
			return
		}
		seen[key] = true
		keys = append(keys, key)
	}
	for _, key := range preferred {
		add(key)
	}
	remaining := make([]string, 0, len(raw))
	for key := range raw {
		if !seen[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	for _, key := range remaining {
		add(key)
	}
	if len(keys) > maxPanelDataEntries {
		keys = keys[:maxPanelDataEntries]
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = raw[key]
	}
	return out
}

// Status returns a snapshot of loaded modules and the descriptors they have
// emitted. A descriptor is data for the core UI to render, never HTML or JS.
func (r *Runtime) Status() Status {
	status := Status{Modules: []ModuleStatus{}}
	if r == nil {
		return status
	}
	r.mu.Lock()
	instances := append([]*instance(nil), r.instances...)
	r.mu.Unlock()
	status.Enabled = len(instances) > 0
	for _, inst := range instances {
		capabilities := append([]string(nil), inst.manifest.Capabilities...)
		moduleName := inst.manifest.Module
		if inst.archive != "" {
			moduleName = inst.archive
		}
		status.Modules = append(status.Modules, ModuleStatus{
			ID:           inst.id,
			Version:      inst.manifest.Version,
			ABI:          inst.manifest.ABI,
			Module:       moduleName,
			License:      inst.manifest.License,
			Capabilities: capabilities,
			Failures:     inst.failures.Load(),
		})
		ui := inst.descriptors()
		if len(ui.Tools) > 0 || len(ui.Panels) > 0 {
			status.Descriptors = append(status.Descriptors, ui)
		}
	}
	return status
}

func (i *instance) descriptors() ModuleUI {
	ui := ModuleUI{PluginID: i.id}
	value := i.lastOutput.Load()
	if value == nil {
		return ui
	}
	var payload guestOutput
	if err := json.Unmarshal(value.([]byte), &payload); err != nil {
		return ui
	}
	if len(payload.Tools) > 0 {
		ui.Tools = append([]ToolUI(nil), payload.Tools...)
		if len(ui.Tools) > maxDescriptorTools {
			ui.Tools = ui.Tools[:maxDescriptorTools]
		}
	}
	if len(payload.Panels) > 0 {
		panels := append([]PanelUI(nil), payload.Panels...)
		if len(panels) > maxDescriptorPanels {
			panels = panels[:maxDescriptorPanels]
		}
		for i := range panels {
			panels[i].Data = sanitizePanelData(panels[i].Data)
		}
		ui.Panels = panels
	}
	return ui
}

// VerifyReport is the result of re-reading plugin files without loading them.
type VerifyReport struct {
	OK      bool          `json:"ok"`
	Modules []VerifyEntry `json:"modules"`
}

type VerifyEntry struct {
	File   string `json:"file"`
	ID     string `json:"id,omitempty"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Verify re-checks manifests and digests on disk. It never instantiates code.
func (r *Runtime) Verify(dir string) (VerifyReport, error) {
	report := VerifyReport{OK: true, Modules: []VerifyEntry{}}
	if dir == "" {
		return report, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return report, err
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		isModule := strings.HasSuffix(name, ".wasm")
		isArchive := strings.HasSuffix(name, ".mvplugin")
		if !isModule && !isArchive {
			continue
		}
		count++
		if count > MaxPluginFiles {
			return report, fmt.Errorf("plugin dir: more than %d modules", MaxPluginFiles)
		}
		item := VerifyEntry{File: name}
		info, err := entry.Info()
		limit := maxModuleBytes
		if isArchive {
			limit = maxPackageArchiveBytes
		}
		switch {
		case err != nil:
			item.Detail = err.Error()
		case !info.Mode().IsRegular() || info.Size() > int64(limit):
			item.Detail = "not a regular file under the byte cap"
		default:
			raw, readErr := readBoundedFile(filepath.Join(dir, name), limit)
			if readErr != nil {
				item.Detail = readErr.Error()
			} else if isArchive {
				pkg, pkgErr := ReadPackage(raw)
				switch {
				case pkgErr != nil:
					item.Detail = pkgErr.Error()
				case name != pkg.Manifest.ID+".mvplugin":
					item.Detail = "archive name does not match manifest id"
				default:
					item.ID = pkg.Manifest.ID
					item.OK = true
				}
			} else {
				manifest, manifestErr := LoadPluginManifest(filepath.Join(dir, strings.TrimSuffix(name, ".wasm")+".json"))
				if manifestErr != nil {
					item.Detail = manifestErr.Error()
				} else {
					item.ID = manifest.ID
					sum := sha256.Sum256(raw)
					switch {
					case manifest.Module != name:
						item.Detail = "manifest names a different module"
					case hex.EncodeToString(sum[:]) != manifest.ModuleSHA256:
						item.Detail = "digest mismatch"
					default:
						item.OK = true
					}
				}
			}
		}
		if !item.OK && item.Detail == "" {
			item.Detail = "verification failed"
		}
		report.OK = report.OK && item.OK
		report.Modules = append(report.Modules, item)
	}
	return report, nil
}
