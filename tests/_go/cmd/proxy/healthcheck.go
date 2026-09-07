package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthcheckDoesNotRewriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("max_retriez: 3\nmax_retries: 1\n---\nextra: true\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath, oldListen := liveConfigPath, liveListenOverride
	t.Cleanup(func() {
		liveConfigPath, liveListenOverride = oldPath, oldListen
	})
	liveConfigPath = path
	liveListenOverride = "127.0.0.1:1"
	_ = runHealthcheck()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("healthcheck rewrote config: %v\n%s", err, got)
	}
}
