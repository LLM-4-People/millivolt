package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestReloadReportsDroppedKeysWithoutRewriting(t *testing.T) {
	liveReloadFixture(t)
	raw := []byte("max_retries: 3\nmax_retriez: 9\n")
	if err := os.WriteFile(liveConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pending, err := reloadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if liveCfg.MaxRetries != 3 {
		t.Fatalf("reload dropped the valid override: max_retries=%d", liveCfg.MaxRetries)
	}
	if len(pending) != 0 {
		t.Fatalf("reload reported startup-bound changes: %v", pending)
	}
	body, err := os.ReadFile(liveConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("max_retriez: 9")) {
		t.Fatalf("reload rewrote the source instead of preserving it:\n%s", body)
	}
	if !bytes.Contains(body, []byte("max_retries: 3\n")) {
		t.Fatalf("reload lost the valid override:\n%s", body)
	}
}

func TestReloadKeepsStartupWarningUntilBootSettingsRestored(t *testing.T) {
	liveReloadFixture(t)
	changed := liveCfg.Clone()
	changed.HistorySize++
	for _, retries := range []int{2, 3} {
		changed.MaxRetries = retries
		if err := config.WriteFile(liveConfigPath, changed); err != nil {
			t.Fatal(err)
		}
		pending, err := reloadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(pending, "history_size") || liveCfg.MaxRetries != retries {
			t.Fatalf("later saves must retain the startup warning: pending=%v retries=%d", pending, liveCfg.MaxRetries)
		}
	}
	changed.HistorySize = bootCfg.HistorySize
	if err := config.WriteFile(liveConfigPath, changed); err != nil {
		t.Fatal(err)
	}
	pending, err := reloadConfig()
	if err != nil || len(pending) != 0 {
		t.Fatalf("restoring boot settings should clear warning: %v %v", pending, err)
	}
}

// Reload must never treat an unparseable document (including a nested
// duplicate key) as boot debris: the running config and the file stay exactly
// as they were, and no .invalid copy is created.
func TestReloadParseErrorPreservesRunningConfigAndFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"plain syntax error", "listen: [\n"},
		{"nested duplicate", "providers:\n  alpha:\n    models_path: /v1\n    models_path: /v2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveReloadFixture(t)
			good := []byte("max_retries: 3\n")
			if err := os.WriteFile(liveConfigPath, good, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := reloadConfig(); err != nil {
				t.Fatal(err)
			}
			raw := []byte(tc.body)
			if err := os.WriteFile(liveConfigPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := reloadConfig(); err == nil {
				t.Fatalf("reload accepted %s", tc.name)
			}
			if liveCfg.MaxRetries != 3 {
				t.Fatalf("failed reload changed the running config: max_retries=%d", liveCfg.MaxRetries)
			}
			got, err := os.ReadFile(liveConfigPath)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("failed reload rewrote the active config: %v %q", err, got)
			}
			if _, err := os.Stat(liveConfigPath + ".invalid"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed reload moved the active config aside: %v", err)
			}
		})
	}
}

// A reload with an invalid security-sensitive value fails and keeps the
// running allowlist; a mixed list never applies its valid prefix.
func TestReloadInvalidAllowlistKeepsRunningPolicy(t *testing.T) {
	liveReloadFixture(t)
	good := []byte("allowed_base_urls:\n  - https://api.example.com\n")
	if err := os.WriteFile(liveConfigPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	bad := []byte("allowed_base_urls:\n  - https://api.example.com\n  - api.openai.com\n")
	if err := os.WriteFile(liveConfigPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err == nil {
		t.Fatal("reload accepted a mixed valid/invalid allowlist")
	}
	if !slices.Equal(liveCfg.AllowedBaseURLs, []string{"https://api.example.com"}) {
		t.Fatalf("failed reload changed the running allowlist: %v", liveCfg.AllowedBaseURLs)
	}
	got, err := os.ReadFile(liveConfigPath)
	if err != nil || !bytes.Equal(got, bad) {
		t.Fatalf("failed reload rewrote the active config: %v %q", err, got)
	}
}

// A boot-time repair is not a reload behavior: unknown debris is reported and
// applied around it, but the reload never rewrites the file.
func TestReloadUnknownDebrisDoesNotRewrite(t *testing.T) {
	liveReloadFixture(t)
	raw := []byte("max_retries: 4\nmax_retriez: 9\n")
	if err := os.WriteFile(liveConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	if liveCfg.MaxRetries != 4 {
		t.Fatalf("reload lost the valid key: max_retries=%d", liveCfg.MaxRetries)
	}
	got, err := os.ReadFile(liveConfigPath)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("reload rewrote the source: %v %q", err, got)
	}
}
