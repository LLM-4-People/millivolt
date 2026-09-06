package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A failing build stub proves source selection/preflight without building,
// launching, stopping, or touching the database of any proxy process.
func TestDevConfigSourcePreflight(t *testing.T) {
	script, err := os.ReadFile("../../scripts/dev.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, source string
		example      bool
		wantBuild    bool
	}{
		{"public example", "", true, true},
		{"never implicitly local", "", false, false},
		{"explicit operator source", "proxy.yaml", false, true},
		{"missing source", "absent.yaml", true, false},
		{"directory source", "scripts", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "bin"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string][]byte{
				"scripts/dev.sh": script,
				"proxy.yaml":     []byte("operator-only: true\n"),
				"bin/go":         []byte("#!/usr/bin/env bash\necho GO_BUILD_BLOCKED >&2\nexit 42\n"),
			}
			if tc.example {
				files["proxy.example.yaml"] = []byte("example-only: true\n")
			}
			for name, contents := range files {
				if err := os.WriteFile(filepath.Join(root, name), contents, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join(root, "scripts/dev.sh"), "up")
			cmd.Env = append(os.Environ(),
				"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
				"DEV_CONFIG="+tc.source, "DEV_PORT=18081", "DEV_HOST=127.0.0.1", "DEV_DB=none", "DEV_COPY_DB=0")
			out, err := cmd.CombinedOutput()
			if err == nil || strings.Contains(string(out), "GO_BUILD_BLOCKED") != tc.wantBuild {
				t.Fatalf("source preflight: err=%v output=%s", err, out)
			}
			if !tc.wantBuild && !strings.Contains(string(out), "DEV_CONFIG must name a readable regular file") {
				t.Fatalf("source was not refused before build: %s", out)
			}
		})
	}
}

func TestDevStatusDoesNotRequireSourceConfig(t *testing.T) {
	cmd := exec.Command("bash", "../../scripts/dev.sh", "status")
	cmd.Env = append(os.Environ(), "DEV_CONFIG="+filepath.Join(t.TempDir(), "absent.yaml"),
		"DEV_PORT=18081", "DEV_HOST=127.0.0.1", "DEV_DB=none")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("status must not need a config source: %v: %s", err, out)
	}
}
