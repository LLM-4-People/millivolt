package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Handler is GET/POST /admin/config. GET returns the schema (the single
// registry the dashboard renders) plus file values, defaults, and the
// process-effective snapshot. POST applies a values object onto the file
// config, validates, writes YAML, then hot-reloads via Persist.
//
// Lowest choke point: the same Schema / Apply / Validate / WriteFile path
// every caller uses. Unknown keys 400. A failed validate never writes.
type Handler struct {
	// Serialize the file read/compare/write/reload transaction. Revision checks
	// also reject stale full-form saves from another tab instead of losing edits.
	mu sync.Mutex
	// Path is the config file. Empty means in-memory defaults only; POST
	// is rejected because there is nowhere to persist.
	Path string
	// Effective returns the running snapshot (CLI overrides applied).
	Effective func() *Config
	// Startup is the immutable process-start snapshot, including CLI overrides.
	// It owns restart-required comparisons even after multiple saves/reloads.
	Startup *Config
	// Overrides are yaml-key → value for CLI flags that must not be written
	// back into the file (dev instance -listen / -db-path).
	Overrides map[string]string
	// Persist is called AFTER a successful WriteFile to hot-apply. Production
	// uses LoadFileRepair + CLI overrides + Server.Reload (a clean WriteFile
	// is a no-op repair). Nil is write-only (tests).
	Persist func() (restartRequired []string, err error)
	// Backup reports whether Settings can download/restore config and
	// database. Nil omits the object (tests).
	Backup func() map[string]any
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		h.serveGet(w)
	case http.MethodPost:
		h.servePost(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, `{"error":"GET or POST only"}`, http.StatusMethodNotAllowed)
	}
}

func (h *Handler) serveGet(w http.ResponseWriter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	file, err := h.fileConfig()
	if err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusInternalServerError)
		return
	}
	body := h.state(file)
	body["fields"] = Schema()
	body["categories"] = Categories()
	body["defaults"] = Default().Map()
	body["overrides"] = h.Overrides
	body["writable"] = h.Path != ""
	body["path"] = h.Path
	body["usage_fields"] = metrics.CanonicalUsageFields
	body["model_fields"] = format.CanonicalModelFields
	if h.Backup != nil {
		b := h.Backup()
		if b == nil {
			b = map[string]any{}
		}
		b["modified"] = DiffKeys(file, Default())
		body["backup"] = b
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(body); err != nil {
		return
	}
}

func (h *Handler) servePost(w http.ResponseWriter, r *http.Request) {
	if h.Path == "" {
		http.Error(w, `{"error":"no config file; start with -config to persist settings"}`, http.StatusBadRequest)
		return
	}
	var body struct {
		Values   map[string]any `json:"values"`
		Revision string         `json:"revision"`
	}
	if err := adminjson.Decode(w, r, &body); err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
		return
	}
	if body.Values == nil || body.Revision == "" {
		http.Error(w, `{"error":"values object and revision required"}`, http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, err := h.fileConfig()
	if err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusInternalServerError)
		return
	}
	if body.Revision != configRevision(cur) {
		http.Error(w, `{"error":"settings changed since they were loaded; revert to reload before applying"}`, http.StatusConflict)
		return
	}
	// CLI-overridden keys are never taken from the form - writing the
	// effective listen (dev :8081) into proxy.yaml would retarget prod.
	for k := range h.Overrides {
		delete(body.Values, k)
	}
	if err := cur.Apply(body.Values); err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
		return
	}
	if err := cur.Validate(); err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
		return
	}
	if err := WriteFile(h.Path, cur); err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusInternalServerError)
		return
	}
	var reloadErr error
	if h.Persist != nil {
		_, reloadErr = h.Persist()
	}
	file, err := h.fileConfig()
	if err != nil {
		http.Error(w, `{"error":`+strconv.Quote("settings saved, but reading them back failed: "+err.Error())+`}`, http.StatusInternalServerError)
		return
	}
	result := h.state(file)
	result["ok"] = reloadErr == nil
	result["saved"] = true
	if reloadErr != nil {
		result["error"] = "settings saved, but reload failed: " + reloadErr.Error()
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = json.NewEncoder(w).Encode(result)
}

// GET and POST share the same persisted/effective/restart state builder.
func (h *Handler) state(file *Config) map[string]any {
	effective := file
	if h.Effective != nil {
		if e := h.Effective(); e != nil {
			effective = e
		}
	}
	skipped := StartupBoundChanges(h.Startup, effective)
	if skipped == nil {
		skipped = []string{}
	}
	return map[string]any{
		"restart_required": skipped,
		"values":           file.Map(),
		"revision":         configRevision(file),
		"effective":        effective.Map(),
	}
}

// Map contains only schema-owned JSON values; encoding/json sorts map keys,
// making this identity independent of YAML comments or map iteration order.
func configRevision(c *Config) string {
	b, _ := json.Marshal(c.Map())
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (h *Handler) fileConfig() (*Config, error) {
	if h.Path == "" {
		return Default(), nil
	}
	c, err := LoadFile(h.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, fmt.Errorf("load config: %w", err)
	}
	return c, nil
}
