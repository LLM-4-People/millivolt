package plugins

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSource struct {
	fetched FetchedPlugin
	err     error
}

func (f fakeSource) Fetch(context.Context, string) (FetchedPlugin, error) {
	return f.fetched, f.err
}

func writeTrustedArchiveForUpdate(t *testing.T, sourceDir, id, version string, key ed25519.PrivateKey) (string, []byte, FetchedPlugin) {
	t.Helper()
	raw := signedArchiveBytes(t, id, version, key, "release")
	path := filepath.Join(sourceDir, id+".mvplugin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	fetched := FetchedPlugin{
		Raw:             raw,
		Package:         pkg,
		Source:          "github:owner/repo@" + version,
		ResolvedVersion: version,
	}
	return path, raw, fetched
}

func trustedTestTrust(t *testing.T, key ed25519.PrivateKey) *Trust {
	t.Helper()
	pub := key.Public().(ed25519.PublicKey)
	path := writeTrust(t, t.TempDir(), "release", pub)
	trust, err := LoadTrust(path)
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

func TestCompareVersions(t *testing.T) {
	if compareVersions("v0.1.0", "0.1.0") != 0 {
		t.Fatal("v prefix not normalized")
	}
	if compareVersions("0.2.0", "0.1.9") <= 0 {
		t.Fatal("higher patch not detected")
	}
	if compareVersions("0.2.0-rc.1", "0.2.0") >= 0 {
		t.Fatal("prerelease ordering is wrong")
	}
}

func TestUpdateStagesNewVersionAndRollback(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "plugins.lock.json")
	sourceDir := t.TempDir()
	_, _, initial := writeTrustedArchiveForUpdate(t, sourceDir, "fixture", "0.1.0", key)
	report, err := InstallFile(filepath.Join(sourceDir, "fixture.mvplugin"), dir, lockPath, trustedTestTrust(t, key), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != "0.1.0" {
		t.Fatalf("install report = %+v", report)
	}
	trust := trustedTestTrust(t, key)
	locks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, next := writeTrustedArchiveForUpdate(t, t.TempDir(), "fixture", "0.2.0", key)
	next.Source = "github:owner/repo@v0.2.0"
	updateReport, err := Update(context.Background(), UpdateRequest{ID: "fixture", Source: "github:owner/repo@latest"}, dir, lockPath, locks, trust, fakeSource{fetched: next})
	if err != nil {
		t.Fatal(err)
	}
	if updateReport.FromVersion != "0.1.0" || updateReport.ToVersion != "0.2.0" || !updateReport.RestartRequired {
		t.Fatalf("update report = %+v", updateReport)
	}
	updatedLocks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := updatedLocks.Install("fixture")
	if !ok || state.Version != "0.2.0" || state.Previous == nil || state.Previous.Version != "0.1.0" {
		t.Fatalf("state = %+v, %v", state, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "fixture.mvplugin.prev")); err != nil {
		t.Fatalf("previous archive missing: %v", err)
	}
	if initial.ResolvedVersion != "0.1.0" {
		t.Fatalf("unexpected initial fetched package %+v", initial)
	}

	rollback, err := Rollback(dir, lockPath, updatedLocks, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Version != "0.1.0" || !rollback.RestartRequired {
		t.Fatalf("rollback report = %+v", rollback)
	}
	after, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	state, ok = after.Install("fixture")
	if !ok || state.Version != "0.1.0" || state.Previous != nil {
		t.Fatalf("state after rollback = %+v, %v", state, ok)
	}
}

func TestUpdateRequiresCapabilityConfirmation(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "plugins.lock.json")
	sourceDir := t.TempDir()
	writeTrustedArchiveForUpdate(t, sourceDir, "fixture", "0.1.0", key)
	trust := trustedTestTrust(t, key)
	if _, err := InstallFile(filepath.Join(sourceDir, "fixture.mvplugin"), dir, lockPath, trust, true); err != nil {
		t.Fatal(err)
	}
	locks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := signedArchiveBytes(t, "fixture", "0.2.0", key, "release", "emit_metric", "read:records")
	pkg, err := ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	fetched := FetchedPlugin{Raw: raw, Package: pkg, Source: "github:owner/repo@v0.2.0", ResolvedVersion: "0.2.0"}
	if _, err := Update(context.Background(), UpdateRequest{ID: "fixture", Source: "github:owner/repo@latest"}, dir, lockPath, locks, trust, fakeSource{fetched: fetched}); err == nil || !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("capability growth accepted: %v", err)
	}
}

func TestBootRecoveryPromotesPreviousArchive(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "plugins.lock.json")
	sourceDir := t.TempDir()
	writeTrustedArchiveForUpdate(t, sourceDir, "fixture", "0.1.0", key)
	trust := trustedTestTrust(t, key)
	if _, err := InstallFile(filepath.Join(sourceDir, "fixture.mvplugin"), dir, lockPath, trust, true); err != nil {
		t.Fatal(err)
	}
	locks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, next := writeTrustedArchiveForUpdate(t, t.TempDir(), "fixture", "0.2.0", key)
	next.Source = "github:owner/repo@v0.2.0"
	if _, err := Update(context.Background(), UpdateRequest{ID: "fixture", Source: "github:owner/repo@latest"}, dir, lockPath, locks, trust, fakeSource{fetched: next}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.mvplugin"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveredLocks, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	r.SetLockPath(lockPath)
	if err := r.LoadDirWithPolicy(context.Background(), dir, trust, recoveredLocks); err != nil {
		t.Fatalf("boot recovery: %v", err)
	}
	state, ok := recoveredLocks.Install("fixture")
	if !ok || state.Version != "0.1.0" {
		t.Fatalf("state after recovery = %+v, %v", state, ok)
	}
}
