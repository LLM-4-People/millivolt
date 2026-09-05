package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt"
)

func TestVersionOnly(t *testing.T) {
	if mode := os.Getenv("MILLIVOLT_TEST_VERSION"); mode != "" {
		flag.CommandLine = flag.NewFlagSet("proxy", flag.ExitOnError)
		os.Args = []string{"proxy", "-version", "-config", "invalid.yaml",
			"-listen", "not-an-address", "-db-path", "missing/never.db", "-pid-file", "never.pid"}
		if mode == "conflict" {
			os.Args = append(os.Args, "-print-config")
		}
		main()
		os.Exit(0)
	}
	for _, mode := range []string{"version", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			invalid := []byte("unknown-setting: [\n")
			if err := os.WriteFile(filepath.Join(dir, "invalid.yaml"), invalid, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVersionOnly$")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "MILLIVOLT_TEST_VERSION="+mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if mode == "conflict" {
				if err == nil || len(output) != 0 || !bytes.Contains(stderr.Bytes(), []byte("mutually exclusive")) {
					t.Fatalf("conflicting flags: output %q, stderr %q, err %v", output, &stderr, err)
				}
			} else {
				if err != nil || stderr.Len() != 0 {
					t.Fatalf("version command: %v, stderr %q", err, &stderr)
				}
				var got millivolt.BuildInfo
				if err := json.Unmarshal(output, &got); err != nil || got.Version != millivolt.Version() || got.Revision == "" || got.GoVersion == "" {
					t.Fatalf("version JSON: %q, %v", output, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "invalid.yaml" {
				t.Fatalf("version command wrote runtime state: %v, %v", entries, err)
			}
			preserved, err := os.ReadFile(filepath.Join(dir, "invalid.yaml"))
			if err != nil || !bytes.Equal(preserved, invalid) {
				t.Fatalf("version command changed configuration: %v", err)
			}
		})
	}
}
