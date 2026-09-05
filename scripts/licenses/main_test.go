package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt"
)

func TestLicenseNames(t *testing.T) {
	for _, name := range []string{"LICENSE", "LICENSE-3RD-PARTY.md", "LICENSE.txt", "COPYING", "NOTICE.md", "PATENTS", "patents.txt", "DEPENDENCY-LICENSE", "asset.LICENSE"} {
		if !licenseName(name) {
			t.Errorf("missed %s", name)
		}
	}
	for _, name := range []string{"license_check.go", "license.go", "licenseplate.txt"} {
		if licenseName(name) {
			t.Errorf("false match %s", name)
		}
	}
}
func TestNoticeAncestorsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "package")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	s := &source{root: root, dirs: map[string]bool{}}
	if err := addAncestors(s, dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE", "nested/NOTICE", "nested/package/PATENTS"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := collect(s)
	if err != nil || len(files) != 3 {
		t.Fatalf("package ancestor notices: %v %v", files, err)
	}
	if err := addAncestors(s, filepath.Dir(root)); err == nil {
		t.Fatal("accepted notice outside module")
	}
	if err := os.Symlink(filepath.Join(root, "LICENSE"), filepath.Join(dir, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	if _, err := collect(s); err == nil {
		t.Fatal("silently omitted symlinked notice")
	}
}
func TestMissingLicenseFailsBeforeOutput(t *testing.T) {
	root := t.TempDir()
	_, err := collect(&source{root: root, dirs: map[string]bool{root: true}, extra: map[string]string{}})
	if err == nil {
		t.Fatal("missing notice accepted")
	}
}
func TestExactInstalledDependencyNotices(t *testing.T) {
	// Run from the module root, like the Docker build and shared check script.
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(old, "../.."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("test requires module checkout")
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	packages, err := dependencies("linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "notices")
	if err := bundle(out, packages); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []entry
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range manifest {
		seen[e.Name] = true
		if e.Name == "millivolt" && e.Version != millivolt.Version() {
			t.Fatal("application notice version differs from VERSION")
		}
		if len(e.Files) == 0 {
			t.Fatal("empty module notices")
		}
	}
	for _, p := range packages {
		if p.Module == nil || p.Module.Main {
			continue
		}
		name := "modules/" + p.Module.Path
		if !seen[name] {
			t.Fatalf("missing actual dependency %s", name)
		}
		for _, file := range []string{"LICENSE", "LICENSE-3RD-PARTY.md", "NOTICE", "PATENTS"} {
			original, err := os.ReadFile(filepath.Join(p.Module.Dir, file))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(filepath.Join(out, name, file))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, copied) {
				t.Fatalf("changed upstream notice %s/%s", name, file)
			}
		}
	}
	for _, required := range []string{"go", "millivolt"} {
		if !seen[required] {
			t.Fatalf("missing %s", required)
		}
	}
	if strings.Contains(string(data), root) {
		t.Fatal("manifest leaked checkout path")
	}
	for _, name := range []string{"LICENSE", "PATENTS"} {
		original, err := os.ReadFile(filepath.Join(runtime.GOROOT(), name))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.ReadFile(filepath.Join(out, "go", name))
		if err != nil || !bytes.Equal(original, copied) {
			t.Fatalf("Go toolchain %s not preserved: %v", name, err)
		}
	}
	outAgain := filepath.Join(t.TempDir(), "notices")
	if err := bundle(outAgain, packages); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(outAgain, "manifest.json"))
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("notice manifest is not deterministic: %v", err)
	}
	embedded := filepath.Join(out, "millivolt", "internal", "web", "static", "vendor", "uplot.LICENSE")
	if _, err := os.Stat(embedded); err != nil {
		t.Fatalf("embedded asset notice: %v", err)
	}
}
