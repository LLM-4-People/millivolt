package plugins

import (
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

func archiveManifestForTest(t *testing.T, id string, module []byte, signer string, key ed25519.PrivateKey) []byte {
	t.Helper()
	sum := sha256.Sum256(module)
	m := &PluginManifest{
		ID:           id,
		Version:      "0.1.0",
		ABI:          "v1",
		MinMillivolt: packageTestMinMillivolt,
		Capabilities: []string{"emit_metric"},
		Module:       packageModuleName,
		ModuleSHA256: hex.EncodeToString(sum[:]),
		License:      "MIT",
		Signer:       signer,
	}
	if key != nil {
		m.Signature = hex.EncodeToString(ed25519.Sign(key, m.SigningPayload()))
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeArchiveForTest(t *testing.T, dir, id string, code []byte, signer string, key ed25519.PrivateKey) (string, []byte) {
	t.Helper()
	raw := makePackageZip(t,
		packageZipEntry{name: packageManifestName, data: archiveManifestForTest(t, id, code, signer, key)},
		packageZipEntry{name: packageModuleName, data: code},
	)
	name := id + ".mvplugin"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func TestInstallFileWritesArchivePinAndState(t *testing.T) {
	dir := t.TempDir()
	srcDir := t.TempDir()
	code := fixtureModule()
	src, raw := writeArchiveForTest(t, srcDir, "fixture", code, "", nil)
	lockPath := filepath.Join(dir, "plugins.lock.json")

	report, err := InstallFile(src, dir, lockPath, &Trust{keys: map[string]ed25519.PublicKey{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.ID != "fixture" || report.Version != "0.1.0" || report.Archive != "fixture.mvplugin" {
		t.Fatalf("report = %+v", report)
	}
	if report.Signature != "unsigned" || report.Pin != "updated" || !report.RestartRequired {
		t.Fatalf("report = %+v", report)
	}
	active, err := os.ReadFile(filepath.Join(dir, "fixture.mvplugin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != string(raw) {
		t.Fatal("active archive bytes changed")
	}
	locks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := locks.Check("fixture.mvplugin", pkg.ArchiveSHA256); err != nil {
		t.Fatalf("archive pin missing: %v", err)
	}
	state, ok := locks.Install("fixture")
	if !ok || state.Version != "0.1.0" || state.Archive != "fixture.mvplugin" || state.Source != "local" {
		t.Fatalf("install state = %+v, %v", state, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "fixture.mvplugin.prev")); !os.IsNotExist(err) {
		t.Fatalf("unexpected previous archive: %v", err)
	}
}

func TestInstallFileRejectsUnsignedWithoutAllow(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeArchiveForTest(t, t.TempDir(), "fixture", fixtureModule(), "", nil)
	_, err := InstallFile(src, dir, filepath.Join(dir, "plugins.lock.json"), &Trust{keys: map[string]ed25519.PublicKey{}}, false)
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned archive accepted: %v", err)
	}
}

func TestInstallFileRequiresTrustedSignature(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trustDir := t.TempDir()
	trust, err := LoadTrust(writeTrust(t, trustDir, "release", pub))
	if err != nil {
		t.Fatal(err)
	}
	src, _ := writeArchiveForTest(t, t.TempDir(), "fixture", fixtureModule(), "", nil)
	_, err = InstallFile(src, dir, filepath.Join(dir, "plugins.lock.json"), trust, true)
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned archive accepted under trust: %v", err)
	}
}

func TestInstallFileRejectsDuplicateAndPinMismatch(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	src, _ := writeArchiveForTest(t, t.TempDir(), "fixture", code, "", nil)
	lockPath := filepath.Join(dir, "plugins.lock.json")
	trust := &Trust{keys: map[string]ed25519.PublicKey{}}
	if _, err := InstallFile(src, dir, lockPath, trust, true); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFile(src, dir, lockPath, trust, true); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("duplicate accepted: %v", err)
	}

	otherDir := t.TempDir()
	otherLock := filepath.Join(otherDir, "plugins.lock.json")
	if err := os.WriteFile(otherLock, []byte(`{"pins":{"fixture.mvplugin":"`+strings.Repeat("1", 64)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFile(src, otherDir, otherLock, trust, true); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("pin mismatch accepted: %v", err)
	}
}

func TestInstallFileRollsBackWhenLockCommitFails(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "lock-dir")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	src, _ := writeArchiveForTest(t, t.TempDir(), "fixture", fixtureModule(), "", nil)
	trust := &Trust{keys: map[string]ed25519.PublicKey{}}
	if _, err := InstallFile(src, dir, lockPath, trust, true); err == nil {
		t.Fatal("expected lock commit failure")
	}
	if _, err := os.Lstat(filepath.Join(dir, "fixture.mvplugin")); !os.IsNotExist(err) {
		t.Fatalf("archive left behind after failed commit: %v", err)
	}
}

func TestUninstallRemovesArchiveAndMetadata(t *testing.T) {
	dir := t.TempDir()
	src, _ := writeArchiveForTest(t, t.TempDir(), "fixture", fixtureModule(), "", nil)
	lockPath := filepath.Join(dir, "plugins.lock.json")
	trust := &Trust{keys: map[string]ed25519.PublicKey{}}
	if _, err := InstallFile(src, dir, lockPath, trust, true); err != nil {
		t.Fatal(err)
	}
	prev := filepath.Join(dir, "fixture.mvplugin.prev")
	if err := os.WriteFile(prev, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(dir, lockPath, "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "fixture.mvplugin"), prev} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("file not removed %q: %v", path, err)
		}
	}
	locks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := locks.Install("fixture"); ok {
		t.Fatal("install metadata survived uninstall")
	}
	if _, ok := locks.Pins["fixture.mvplugin"]; ok {
		t.Fatal("pin survived uninstall")
	}
}
