package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func recordForTest() *metrics.Record {
	now := time.Now()
	return &metrics.Record{ID: "req-status", Start: now, End: now.Add(time.Millisecond)}
}

func TestStatusAndDescriptors(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	writePlugin(t, dir, "fixture", code, pluginHash(code))
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	r.PublishRecord(context.Background(), recordForTest())

	status := r.Status()
	if !status.Enabled || len(status.Modules) != 1 {
		t.Fatalf("status = %+v", status)
	}
	module := status.Modules[0]
	if module.ID != "fixture" || module.ABI != "v1" || len(module.Capabilities) != 1 {
		t.Fatalf("module status = %+v", module)
	}
	if len(status.Descriptors) != 0 {
		t.Fatalf("fixture declared no descriptors, got %+v", status.Descriptors)
	}
}

func TestVerifyReportsDigestDrift(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	writePlugin(t, dir, "fixture", code, pluginHash(code))
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())

	report, err := r.Verify(dir)
	if err != nil || !report.OK || len(report.Modules) != 1 || !report.Modules[0].OK {
		t.Fatalf("verify = %+v, %v", report, err)
	}

	manifestPath := filepath.Join(dir, "fixture.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), pluginHash(code), strings.Repeat("0", 64), 1)
	if err := os.WriteFile(manifestPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = r.Verify(dir)
	if err != nil || report.OK || len(report.Modules) != 1 || !strings.Contains(report.Modules[0].Detail, "digest") {
		t.Fatalf("verify after drift = %+v, %v", report, err)
	}
}

func TestStatusReportsArchiveFilename(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	writeArchiveForTest(t, dir, "fixture", code, "", nil)
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	r.PublishRecord(context.Background(), recordForTest())
	status := r.Status()
	if len(status.Modules) != 1 || status.Modules[0].Module != "fixture.mvplugin" {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Descriptors) != 0 {
		t.Fatalf("fixture declared no descriptors, got %+v", status.Descriptors)
	}
}

func TestVerifyAcceptsArchive(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	writeArchiveForTest(t, dir, "fixture", code, "", nil)
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	report, err := r.Verify(dir)
	if err != nil || !report.OK || len(report.Modules) != 1 {
		t.Fatalf("verify = %+v, %v", report, err)
	}
	if report.Modules[0].File != "fixture.mvplugin" || report.Modules[0].ID != "fixture" {
		t.Fatalf("verify entry = %+v", report.Modules[0])
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.mvplugin"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = r.Verify(dir)
	if err != nil || report.OK || len(report.Modules) != 1 || report.Modules[0].OK {
		t.Fatalf("verify after tamper = %+v, %v", report, err)
	}
}

func TestPanelDataIsBoundedForCoreRendering(t *testing.T) {
	raw := map[string]string{"provider": "opencode-go", "requests": "12"}
	raw[strings.Repeat("k", maxPanelDataKeyBytes+1)] = "long key"
	raw["long_value"] = strings.Repeat("v", maxPanelDataValueBytes+1)
	for i := 0; i < maxPanelDataEntries+4; i++ {
		raw["extra_"+strings.Repeat("x", 8)+strconv.Itoa(i)] = "x"
	}
	clean := sanitizePanelData(raw)
	if clean["provider"] != "opencode-go" || clean["requests"] != "12" {
		t.Fatalf("clean data = %+v", clean)
	}
	if _, ok := clean["long_value"]; ok {
		t.Fatal("oversized value survived")
	}
	if len(clean) != maxPanelDataEntries {
		t.Fatalf("entries = %d, want %d", len(clean), maxPanelDataEntries)
	}
	for key, value := range clean {
		if len(key) > maxPanelDataKeyBytes || len(value) > maxPanelDataValueBytes {
			t.Fatalf("unbounded entry %q = %q", key, value)
		}
	}
}

func TestPinArchivePreservesInstallState(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	_, raw := writeArchiveForTest(t, dir, "fixture", code, "", nil)
	pkg, err := ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "plugins.lock.json")
	locks := &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}
	if err := locks.SetInstall("fixture", InstallState{
		Version:         "0.1.0",
		Archive:         "fixture.mvplugin",
		Source:          "local",
		ResolvedVersion: "0.1.0",
		ArchiveSHA256:   pkg.ArchiveSHA256,
		InstalledAt:     "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := locks.Save(lockPath); err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDirWithPolicy(context.Background(), dir, nil, locks); err != nil {
		t.Fatal(err)
	}
	pinned, err := r.Pin(dir, lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) != 1 || pinned[0] != "fixture.mvplugin" {
		t.Fatalf("pinned = %q", pinned)
	}
	loaded, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Check("fixture.mvplugin", pkg.ArchiveSHA256); err != nil {
		t.Fatalf("pin missing: %v", err)
	}
	if _, ok := loaded.Install("fixture"); !ok {
		t.Fatal("install metadata lost by Pin")
	}
}

func TestPinPreservesStateInstalledAfterBoot(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	_, raw := writeArchiveForTest(t, dir, "fixture", code, "", nil)
	pkg, err := ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "plugins.lock.json")
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	bootLocks := &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}
	if err := bootLocks.Save(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := r.LoadDirWithPolicy(context.Background(), dir, nil, bootLocks); err != nil {
		t.Fatal(err)
	}
	// The installer commits its own state to disk and does not hot-swap the
	// boot-loaded runtime. PIN must read that authoritative state, not the
	// stale snapshot captured before the install.
	installed := &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}
	if err := installed.SetInstall("fixture", InstallState{
		Version:         "0.1.0",
		Archive:         "fixture.mvplugin",
		Source:          "local",
		ResolvedVersion: "0.1.0",
		ArchiveSHA256:   pkg.ArchiveSHA256,
		InstalledAt:     "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := installed.Save(lockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pin(dir, lockPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLocks(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Install("fixture"); !ok {
		t.Fatal("install metadata committed after boot was lost by Pin")
	}
}

func TestLoadDirWithPolicyRejectsArchivePinMismatch(t *testing.T) {
	dir := t.TempDir()
	code := fixtureModule()
	writeArchiveForTest(t, dir, "fixture", code, "", nil)
	locks := &Locks{
		Pins:     map[string]string{"fixture.mvplugin": strings.Repeat("0", 64)},
		Installs: map[string]InstallState{},
	}
	r, err := NewRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err := r.LoadDirWithPolicy(context.Background(), dir, nil, locks); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("pin mismatch accepted: %v", err)
	}
}
