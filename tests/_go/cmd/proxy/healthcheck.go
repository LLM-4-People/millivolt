package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthcheckDoesNotRewriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("max_retriez: 3\nmax_retries: 1\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// runHealthcheck derives its probe target from cfg.Listen, so the
	// override points it at the real liveness handler on a fixture
	// listener. The old 127.0.0.1:1 target made every run burn the 3s probe
	// timeout on hosts where loopback connects to closed ports hang; the
	// no-rewrite invariant below holds for any probe outcome.
	probe := httptest.NewServer(http.HandlerFunc(handleHealthz))
	defer probe.Close()
	oldPath, oldListen := liveConfigPath, liveListenOverride
	t.Cleanup(func() {
		liveConfigPath, liveListenOverride = oldPath, oldListen
	})
	liveConfigPath = path
	liveListenOverride = strings.TrimPrefix(probe.URL, "http://")
	_ = runHealthcheck()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("healthcheck rewrote config: %v\n%s", err, got)
	}
}

// TestHealthcheckRejectsInvalidBaseConfig pins TC7-1: an invalid recognized
// value makes healthcheck exit 1 without rewriting the file.
func TestHealthcheckRejectsInvalidBaseConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("max_retries: not-an-int\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath := liveConfigPath
	t.Cleanup(func() { liveConfigPath = oldPath })
	liveConfigPath = path
	if code := runHealthcheck(); code != 1 {
		t.Fatalf("healthcheck exit = %d, want 1", code)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("healthcheck rewrote the invalid base file: %v\n%s", err, got)
	}
}
