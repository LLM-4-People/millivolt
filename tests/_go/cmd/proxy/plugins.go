package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/plugins"
)

func pluginHandlerConfig() *config.Config {
	cfg := config.Default()
	cfg.PluginsEnabled = false
	cfg.PluginDir = ""
	return cfg
}

func pluginArchiveBytes(t *testing.T, id, version string, signer string, key ed25519.PrivateKey) []byte {
	t.Helper()
	module := []byte("plugin module bytes")
	sum := sha256.Sum256(module)
	m := plugins.PluginManifest{
		ID:           id,
		Version:      version,
		ABI:          "v1",
		MinMillivolt: "0.3.26",
		Capabilities: []string{"emit_metric"},
		Module:       "module.wasm",
		ModuleSHA256: hex.EncodeToString(sum[:]),
		License:      "MIT",
		Signer:       signer,
	}
	if key != nil {
		m.Signature = hex.EncodeToString(ed25519.Sign(key, m.SigningPayload()))
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifest},
		{"module.wasm", module},
	} {
		w, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writePluginArchive(t *testing.T, dir, id string, raw []byte) string {
	t.Helper()
	path := filepath.Join(dir, id+".mvplugin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePluginTrust(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"id":             "release",
			"ed25519_public": hex.EncodeToString(pub),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type pluginFakeSource struct {
	fetched plugins.FetchedPlugin
}

func (f pluginFakeSource) Fetch(context.Context, string) (plugins.FetchedPlugin, error) {
	return f.fetched, nil
}

func multipartPluginRequest(t *testing.T, raw []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "fixture.mvplugin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/plugins/install", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestAdminPluginsGetWithoutRuntime(t *testing.T) {
	rec := httptest.NewRecorder()
	handlePlugins(nil, pluginHandlerConfig()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/plugins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var doc pluginStatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status.Enabled || len(doc.Status.Modules) != 0 || doc.Config.Enabled {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestAdminPluginsVerify(t *testing.T) {
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"action":"verify"}`)
	handlePlugins(nil, pluginHandlerConfig()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/plugins", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var report plugins.VerifyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK || len(report.Modules) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestAdminPluginsRejectsUnknownActionAndMethod(t *testing.T) {
	h := handlePlugins(nil, pluginHandlerConfig())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/plugins", strings.NewReader(`{"action":"enable"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown action status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/admin/plugins", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestAdminPluginInstallUploadAndUninstall(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")

	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, nil).ServeHTTP(rec, multipartPluginRequest(t, pluginArchiveBytes(t, "fixture", "0.1.0", "", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("install status = %d: %s", rec.Code, rec.Body.String())
	}
	var report plugins.InstallReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ID != "fixture" || report.Version != "0.1.0" {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(cfg.PluginDir, "fixture.mvplugin")); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	handlePluginUninstall(cfg).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/plugins/uninstall", strings.NewReader(`{"id":"fixture"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall status = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.PluginDir, "fixture.mvplugin")); !os.IsNotExist(err) {
		t.Fatalf("archive survived uninstall: %v", err)
	}
}

func TestAdminPluginInstallRequiresDownloadsForSource(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")
	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/plugins/install", strings.NewReader(`{"source":"github:owner/repo@v0.1.0"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminPluginSourceInstallRequiresTrust(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")
	cfg.PluginDownloadsEnabled = true
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := pluginArchiveBytes(t, "fixture", "0.1.0", "release", key)
	pkg, err := plugins.ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	source := pluginFakeSource{fetched: plugins.FetchedPlugin{
		Raw: raw, Package: pkg, Source: "github:owner/repo@v0.1.0", ResolvedVersion: "0.1.0",
	}}
	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, source).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/plugins/install", strings.NewReader(`{"source":"github:owner/repo@v0.1.0"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "trust") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg.PluginTrustFile = writePluginTrust(t, key.Public().(ed25519.PublicKey))
	rec = httptest.NewRecorder()
	handlePluginInstall(cfg, source).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/plugins/install", strings.NewReader(`{"source":"github:owner/repo@v0.1.0"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted source install status = %d: %s", rec.Code, rec.Body.String())
	}
}

func inboxRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/admin/plugins/install", strings.NewReader(`{"action":"install_inbox"}`))
}

func TestAdminPluginInboxScanInstallsValidFileAndKeepsInvalid(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")
	cfg.PluginInboxDir = t.TempDir()

	writePluginArchive(t, cfg.PluginInboxDir, "inbox-fixture", pluginArchiveBytes(t, "inbox-fixture", "0.1.0", "", nil))
	invalid := filepath.Join(cfg.PluginInboxDir, "broken.mvplugin")
	if err := os.WriteFile(invalid, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, nil).ServeHTTP(rec, inboxRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var report inboxInstallReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 2 || len(report.Installed) != 1 || len(report.Failed) != 1 || !report.RestartRequired {
		t.Fatalf("report = %+v", report)
	}
	if report.Installed[0].ID != "inbox-fixture" {
		t.Fatalf("installed = %+v", report.Installed[0])
	}
	if _, err := os.Stat(filepath.Join(cfg.PluginDir, "inbox-fixture.mvplugin")); err != nil {
		t.Fatalf("active archive missing: %v", err)
	}
	if raw, err := os.ReadFile(invalid); err != nil || string(raw) != "not an archive" {
		t.Fatalf("invalid inbox file changed or vanished: %q, %v", raw, err)
	}
}

func TestAdminPluginInboxScanRejectsSymlink(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")
	cfg.PluginInboxDir = t.TempDir()

	target := writePluginArchive(t, cfg.PluginInboxDir, "target", pluginArchiveBytes(t, "target", "0.1.0", "", nil))
	link := filepath.Join(cfg.PluginInboxDir, "linked.mvplugin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, nil).ServeHTTP(rec, inboxRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var report inboxInstallReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) != 1 || report.Failed[0].File != "linked.mvplugin" || report.Failed[0].Error != "not a regular file" {
		t.Fatalf("symlink not rejected: %+v", report.Failed)
	}
}

func TestAdminPluginInboxScanBounding(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")
	cfg.PluginInboxDir = t.TempDir()
	for i := 0; i <= maxInboxEntries; i++ {
		name := fmt.Sprintf("entry-%02d.mvplugin", i)
		if err := os.WriteFile(filepath.Join(cfg.PluginInboxDir, name), []byte("bad"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, nil).ServeHTTP(rec, inboxRequest(t))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "more than") {
		t.Fatalf("bounded scan status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminPluginInboxRequiresConfiguration(t *testing.T) {
	cfg := pluginHandlerConfig()
	cfg.PluginDir = t.TempDir()
	cfg.PluginLockFile = filepath.Join(cfg.PluginDir, "plugins.lock.json")
	rec := httptest.NewRecorder()
	handlePluginInstall(cfg, nil).ServeHTTP(rec, inboxRequest(t))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "plugin_inbox_dir") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}
