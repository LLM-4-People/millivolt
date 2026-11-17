package plugins

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTrust(t *testing.T, dir, signer string, pub ed25519.PublicKey) string {
	t.Helper()
	path := filepath.Join(dir, "trust.json")
	body := map[string]any{
		"keys": []map[string]string{{
			"id":             signer,
			"ed25519_public": hex.EncodeToString(pub),
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSignedPlugin(t *testing.T, dir, id string, code []byte, signer string, key ed25519.PrivateKey) {
	t.Helper()
	sum := sha256.Sum256(code)
	m := &PluginManifest{
		ID:           id,
		Version:      "0.1.0",
		ABI:          "v1",
		MinMillivolt: "0.3.26",
		Capabilities: []string{"emit_metric"},
		Module:       id + ".wasm",
		ModuleSHA256: hex.EncodeToString(sum[:]),
		License:      "MIT",
		Signer:       signer,
	}
	m.Signature = hex.EncodeToString(ed25519.Sign(key, m.SigningPayload()))
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".wasm"), code, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTrustVerifiesSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trust, err := LoadTrust(writeTrust(t, dir, "release", pub))
	if err != nil || !trust.Configured() {
		t.Fatalf("LoadTrust = %v, %v", trust, err)
	}
	code := fixtureModule()
	writeSignedPlugin(t, dir, "signed", code, "release", priv)
	manifest, err := LoadPluginManifest(filepath.Join(dir, "signed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Verify(manifest); err != nil {
		t.Fatalf("Verify signed manifest: %v", err)
	}

	manifest.Signature = strings.Repeat("0", 128)
	if err := trust.Verify(manifest); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("tampered signature accepted: %v", err)
	}
}

func TestTrustRejectsUnsignedAndUnknownSigner(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trust, err := LoadTrust(writeTrust(t, dir, "release", pub))
	if err != nil {
		t.Fatal(err)
	}
	unsigned := &PluginManifest{
		ID: "unsigned", Version: "0.1.0", ABI: "v1", MinMillivolt: "0.3.26",
		Capabilities: []string{"log"}, Module: "unsigned.wasm",
		ModuleSHA256: strings.Repeat("0", 64), License: "MIT",
	}
	if err := trust.Verify(unsigned); err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned manifest accepted: %v", err)
	}
	unsigned.Signer = "other"
	unsigned.Signature = strings.Repeat("0", 128)
	if err := trust.Verify(unsigned); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("unknown signer accepted: %v", err)
	}
}

func TestLocksPinDigests(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	sum := sha256.Sum256(code)
	digest := hex.EncodeToString(sum[:])
	lockPath := filepath.Join(dir, "plugins.lock.json")

	locks := &Locks{Pins: map[string]string{}}
	locks.Set("fixture.wasm", digest)
	if err := locks.Save(lockPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Check("fixture.wasm", digest); err != nil {
		t.Fatalf("matching digest refused: %v", err)
	}
	if err := loaded.Check("fixture.wasm", strings.Repeat("1", 64)); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("changed digest accepted: %v", err)
	}
}

func TestLoadDirWithPolicyRequiresSignature(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	code := fixtureModule()
	writePlugin(t, dir, "unsigned", code, pluginHash(code))
	trust, err := LoadTrust(writeTrust(t, dir, "release", pub))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDirWithPolicy(context.Background(), dir, trust, nil); err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned manifest loaded under trust: %v", err)
	}
}

func writeLockDocument(t *testing.T, body any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plugins.lock.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocksLoadLegacyPinsAndArchiveInstallState(t *testing.T) {
	digest := strings.Repeat("a", 64)
	previousDigest := strings.Repeat("b", 64)
	path := writeLockDocument(t, map[string]any{
		"pins": map[string]string{
			"legacy.wasm":      strings.Repeat("1", 64),
			"fixture.mvplugin": digest,
		},
		"installs": map[string]any{
			"fixture": map[string]any{
				"version":          "0.2.0",
				"archive":          "fixture.mvplugin",
				"source":           "github:owner/repo@v0.2.0",
				"resolved_version": "v0.2.0",
				"archive_sha256":   digest,
				"installed_at":     "2026-01-01T00:00:00Z",
				"previous": map[string]any{
					"version":        "0.1.0",
					"archive":        "fixture.mvplugin.prev",
					"archive_sha256": previousDigest,
					"source":         "local",
				},
			},
		},
	})

	locks, err := LoadLocks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := locks.Check("legacy.wasm", strings.Repeat("1", 64)); err != nil {
		t.Fatalf("legacy pin rejected: %v", err)
	}
	if err := locks.Check("fixture.mvplugin", digest); err != nil {
		t.Fatalf("archive pin rejected: %v", err)
	}
	state, ok := locks.Install("fixture")
	if !ok || state.Version != "0.2.0" || state.ResolvedVersion != "v0.2.0" {
		t.Fatalf("install state = %+v, %v", state, ok)
	}
	if state.Previous == nil || state.Previous.Version != "0.1.0" || state.Previous.ArchiveSHA256 != previousDigest {
		t.Fatalf("previous state = %+v", state.Previous)
	}

	roundTrip := filepath.Join(t.TempDir(), "round-trip.json")
	if err := locks.Save(roundTrip); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLocks(roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Install("fixture"); !ok {
		t.Fatal("install metadata lost on save/load")
	}
}

func TestLocksRejectInvalidInstallState(t *testing.T) {
	valid := map[string]any{
		"version":        "0.1.0",
		"archive":        "fixture.mvplugin",
		"source":         "local",
		"archive_sha256": strings.Repeat("a", 64),
		"installed_at":   "2026-01-01T00:00:00Z",
	}
	tests := []struct {
		name string
		id   string
		body map[string]any
	}{
		{"bad id", "Bad", valid},
		{"bad version", "fixture", map[string]any{
			"version": "old", "archive": "fixture.mvplugin", "source": "local",
			"archive_sha256": strings.Repeat("a", 64), "installed_at": "2026-01-01T00:00:00Z",
		}},
		{"bad archive", "fixture", map[string]any{
			"version": "0.1.0", "archive": "fixture.wasm", "source": "local",
			"archive_sha256": strings.Repeat("a", 64), "installed_at": "2026-01-01T00:00:00Z",
		}},
		{"empty source", "fixture", map[string]any{
			"version": "0.1.0", "archive": "fixture.mvplugin", "source": "",
			"archive_sha256": strings.Repeat("a", 64), "installed_at": "2026-01-01T00:00:00Z",
		}},
		{"bad digest", "fixture", map[string]any{
			"version": "0.1.0", "archive": "fixture.mvplugin", "source": "local",
			"archive_sha256": strings.Repeat("A", 64), "installed_at": "2026-01-01T00:00:00Z",
		}},
		{"bad timestamp", "fixture", map[string]any{
			"version": "0.1.0", "archive": "fixture.mvplugin", "source": "local",
			"archive_sha256": strings.Repeat("a", 64), "installed_at": "yesterday",
		}},
		{"bad previous archive", "fixture", map[string]any{
			"version": "0.1.0", "archive": "fixture.mvplugin", "source": "local",
			"archive_sha256": strings.Repeat("a", 64), "installed_at": "2026-01-01T00:00:00Z",
			"previous": map[string]any{
				"version": "0.0.9", "archive": "other.mvplugin.prev",
				"archive_sha256": strings.Repeat("b", 64), "source": "local",
			},
		}},
		{"unknown field", "fixture", map[string]any{
			"version": "0.1.0", "archive": "fixture.mvplugin", "source": "local",
			"archive_sha256": strings.Repeat("a", 64), "installed_at": "2026-01-01T00:00:00Z",
			"mystery": true,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeLockDocument(t, map[string]any{
				"pins":     map[string]string{},
				"installs": map[string]any{test.id: test.body},
			})
			if _, err := LoadLocks(path); err == nil {
				t.Fatal("invalid install state accepted")
			}
		})
	}
}

func TestSetPinRejectsInvalidArchiveName(t *testing.T) {
	locks := &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}
	if err := locks.SetPin("../escape.mvplugin", strings.Repeat("a", 64)); err == nil {
		t.Fatal("path traversal pin accepted")
	}
	if err := locks.SetPin("fixture.mvplugin", strings.Repeat("a", 64)); err != nil {
		t.Fatalf("valid archive pin rejected: %v", err)
	}
}
