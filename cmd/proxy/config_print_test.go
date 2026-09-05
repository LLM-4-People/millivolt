package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestPrintConfigOnly(t *testing.T) {
	if os.Getenv("MILLIVOLT_TEST_PRINT_CONFIG") == "1" {
		flag.CommandLine = flag.NewFlagSet("proxy", flag.ExitOnError)
		os.Args = []string{"proxy", "-print-config", "-config", "invalid.yaml",
			"-listen", "not-an-address", "-db-path", "missing/never.db", "-pid-file", "never.pid"}
		main()
		os.Exit(0)
	}
	dir := t.TempDir()
	invalid := []byte("unknown-setting: [\n")
	if err := os.WriteFile(filepath.Join(dir, "invalid.yaml"), invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrintConfigOnly$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MILLIVOLT_TEST_PRINT_CONFIG=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("print-only command: %v; stderr: %s", err, stderr.String())
	}
	var want bytes.Buffer
	if err := config.WriteYAML(&want, config.Default()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) || stderr.Len() != 0 {
		t.Fatalf("print-only output differs from defaults or contains diagnostics: %s", stderr.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "invalid.yaml" {
		t.Fatalf("print-only command changed its working directory: %v, %v", entries, err)
	}
	preserved, err := os.ReadFile(filepath.Join(dir, "invalid.yaml"))
	if err != nil || !bytes.Equal(preserved, invalid) {
		t.Fatalf("print-only command changed the selected config: %v", err)
	}
	roundtrip := filepath.Join(t.TempDir(), "printed.yaml")
	if err := os.WriteFile(roundtrip, got, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadFile(roundtrip)
	if err != nil || !reflect.DeepEqual(loaded.Map(), config.Default().Map()) {
		t.Fatalf("printed defaults do not roundtrip: %v", err)
	}
}
