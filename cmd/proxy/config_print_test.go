package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestPrintConfigOnly(t *testing.T) {
	if mode := os.Getenv("MILLIVOLT_TEST_PRINT_CONFIG"); mode != "" {
		flag.CommandLine = flag.NewFlagSet("proxy", flag.ExitOnError)
		os.Args = []string{"proxy", "-config", "invalid.yaml",
			"-listen", "not-an-address", "-db-path", "missing/never.db", "-pid-file", "never.pid"}
		os.Args = append(os.Args, strings.Split(mode, ",")...)
		main()
		os.Exit(0)
	}
	for _, tc := range []struct {
		flags         string
		configuration func() *config.Config
	}{
		{"-print-config", config.Default},
		{"-print-example-config", config.Example},
		{"-print-config,-print-example-config", nil},
		{"-print-example-config,-print-config", nil},
	} {
		t.Run(tc.flags, func(t *testing.T) {
			dir := t.TempDir()
			invalid := []byte("unknown-setting: [\n")
			if err := os.WriteFile(filepath.Join(dir, "invalid.yaml"), invalid, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrintConfigOnly$")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "MILLIVOLT_TEST_PRINT_CONFIG="+tc.flags)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			got, err := cmd.Output()
			if tc.configuration == nil {
				if err == nil || len(got) != 0 || !bytes.Contains(stderr.Bytes(), []byte("mutually exclusive")) {
					t.Fatalf("conflicting flags: output %q, stderr %q, err %v", got, &stderr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("print-only command: %v; stderr: %s", err, stderr.String())
				}
				var want bytes.Buffer
				if err := config.WriteYAML(&want, tc.configuration()); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want.Bytes()) || stderr.Len() != 0 {
					t.Fatalf("print-only output differs from configuration or contains diagnostics: %s", stderr.String())
				}
				roundtrip := filepath.Join(t.TempDir(), "printed.yaml")
				if err := os.WriteFile(roundtrip, got, 0o600); err != nil {
					t.Fatal(err)
				}
				loaded, err := config.LoadFile(roundtrip)
				if err != nil || !reflect.DeepEqual(loaded.Map(), tc.configuration().Map()) {
					t.Fatalf("printed configuration does not roundtrip: %v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "invalid.yaml" {
				t.Fatalf("print-only command changed its working directory: %v, %v", entries, err)
			}
			preserved, err := os.ReadFile(filepath.Join(dir, "invalid.yaml"))
			if err != nil || !bytes.Equal(preserved, invalid) {
				t.Fatalf("print-only command changed the selected config: %v", err)
			}
		})
	}
}
