package main

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestApplyCLIOverridesValidatesTheEffectiveConfig pins H27: the flag
// overrides are applied before validation, and the one applying owner
// validates the result, so an invalid --listen or --db-path cannot bypass the
// loader's checks. A valid override still lands.
func TestApplyCLIOverridesValidatesTheEffectiveConfig(t *testing.T) {
	oldListen, oldDB := liveListenOverride, liveDBOverride
	t.Cleanup(func() { liveListenOverride, liveDBOverride = oldListen, oldDB })
	liveDBOverride = ""

	liveListenOverride = "not-an-address"
	if err := applyCLIOverrides(config.Default()); err == nil || !strings.Contains(err.Error(), "listen:") {
		t.Fatalf("invalid listen override = %v, want the listen validation", err)
	}

	liveListenOverride = "127.0.0.1:18081"
	cfg := config.Default()
	if err := applyCLIOverrides(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:18081" {
		t.Fatalf("listen = %q, want the override", cfg.Listen)
	}
}
