package main

import (
	"bytes"
	"os"
	"slices"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestReloadRepairsDroppedKeys(t *testing.T) {
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
	if bytes.Contains(body, []byte("max_retriez")) {
		t.Fatalf("reload did not repair dropped keys:\n%s", body)
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
