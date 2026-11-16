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

// TestHealthcheckMissingConfigFailsClosed pins the fail-closed exception: a
// named config that does not exist fails the probe instead of silently
// probing the built-in default listener. The empty -config case (built-in
// defaults by definition) is covered by config.LoadFileStrict and stays a 0.
// An invalid document is covered above and also exits 1.
func TestHealthcheckMissingConfigFailsClosed(t *testing.T) {
	oldPath, oldListen := liveConfigPath, liveListenOverride
	t.Cleanup(func() { liveConfigPath, liveListenOverride = oldPath, oldListen })
	probe := httptest.NewServer(http.HandlerFunc(handleHealthz))
	defer probe.Close()
	// The override points at a reachable probe, so a 0 could only come from
	// the default fallback this test removes.
	liveListenOverride = strings.TrimPrefix(probe.URL, "http://")
	liveConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	if code := runHealthcheck(); code != 1 {
		t.Fatalf("healthcheck with a missing named config = %d, want 1", code)
	}
}

// TestHealthcheckEmptyConfigUsesBuiltInDefaults pins the one remaining default
// path: -config "" means the built-in defaults, so the probe runs instead of
// failing for a file that was never named.
func TestHealthcheckEmptyConfigUsesBuiltInDefaults(t *testing.T) {
	oldPath, oldListen := liveConfigPath, liveListenOverride
	t.Cleanup(func() { liveConfigPath, liveListenOverride = oldPath, oldListen })
	probe := httptest.NewServer(http.HandlerFunc(handleHealthz))
	defer probe.Close()
	liveConfigPath = ""
	liveListenOverride = strings.TrimPrefix(probe.URL, "http://")
	if code := runHealthcheck(); code != 0 {
		t.Fatalf("healthcheck with built-in defaults = %d, want 0", code)
	}
}
