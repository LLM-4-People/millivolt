package web

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestDashCfgSeedsMatchDefault locks the first-paint JS seeds to
// config.Default() so they cannot drift into a second table. Runtime still
// overlays GET /admin/config (applyDashValues).
func TestDashCfgSeedsMatchDefault(t *testing.T) {
	b, err := staticFS.ReadFile("static/js/core.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`const dashCfg = \{([^}]+)\}`).FindSubmatch(b)
	if block == nil {
		t.Fatal("core.js missing dashCfg object")
	}
	intField := func(key string) int {
		t.Helper()
		// Word-boundary anchored like boolField: a longer key ending in this
		// one (dash_poll_ms vs poll_ms) must not shadow it.
		m := regexp.MustCompile(`\b` + key + `\s*:\s*(\d+)`).FindSubmatch(block[1])
		if m == nil {
			t.Fatalf("dashCfg.%s missing", key)
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	boolField := func(key string) bool {
		t.Helper()
		// Word-boundary anchored: a longer key ending in this one
		// (dash_background_refresh vs background_refresh) must not shadow it.
		m := regexp.MustCompile(`\b` + key + `\s*:\s*(true|false)`).FindSubmatch(block[1])
		if m == nil {
			t.Fatalf("dashCfg.%s missing", key)
		}
		return string(m[1]) == "true"
	}
	d := config.Default()
	want := map[string]int{
		"history_size":      d.HistorySize,
		"log_rows":          d.DashLogRows,
		"poll_ms":           int(d.DashPollInterval.Milliseconds()),
		"chart_ms":          int(d.DashChartRefresh.Milliseconds()),
		"explorer_stale_ms": int(d.DashExplorerStale.Milliseconds()),
	}
	for key, w := range want {
		if got := intField(key); got != w {
			t.Errorf("dashCfg.%s = %d, want Default %d", key, got, w)
		}
	}
	if got := boolField("background_refresh"); got != d.DashBackgroundRefresh {
		t.Errorf("dashCfg.background_refresh = %v, want Default %v", got, d.DashBackgroundRefresh)
	}
}

// TestCanonicalFieldFallbacksMatchOwners locks the dashboard's settings
// fallback lists and the ui_check fixture to their single Go owners, so a
// field added or removed on either side fails the build:
//   - settings_providers.js USAGE_FIELDS_FALLBACK == metrics.CanonicalUsageFields
//     (the canonical token fields, served by GET /admin/config as usage_fields)
//   - settings_providers.js MODEL_FIELDS_FALLBACK == format.CanonicalModelFields
//     (the /v1/models enrichment vocabulary, served as model_fields)
//   - ui_harness/fixtures.js cfgDoc.usage_fields ==
//     metrics.CanonicalUsageFields (the harness stubs the /admin/config
//     payload and asserts its dropdown)
func TestCanonicalFieldFallbacksMatchOwners(t *testing.T) {
	js, err := staticFS.ReadFile("static/js/settings_providers.js")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsStringList(t, js, "USAGE_FIELDS_FALLBACK = "), metrics.CanonicalUsageFields; !slices.Equal(got, want) {
		t.Errorf("settings_providers.js USAGE_FIELDS_FALLBACK = %v, want metrics.CanonicalUsageFields %v", got, want)
	}
	if got, want := jsStringList(t, js, "MODEL_FIELDS_FALLBACK = "), format.CanonicalModelFields; !slices.Equal(got, want) {
		t.Errorf("settings_providers.js MODEL_FIELDS_FALLBACK = %v, want format.CanonicalModelFields %v", got, want)
	}
	root := moduleRoot(t)
	// cfgDoc lives in the harness fixtures owner, not the runner: the jsdom
	// suite was split into tests/ui_harness/ and this contract must follow the
	// literal rather than the entry-point filename.
	fixtures, err := os.ReadFile(filepath.Join(root, "tests", "ui_harness", "fixtures.js"))
	if err != nil {
		t.Fatalf("read ui_harness/fixtures.js: %v", err)
	}
	if got, want := jsStringList(t, fixtures, "usage_fields: "), metrics.CanonicalUsageFields; !slices.Equal(got, want) {
		t.Errorf("ui_harness/fixtures.js cfgDoc.usage_fields = %v, want metrics.CanonicalUsageFields %v", got, want)
	}
}

// jsStringList extracts a single-line string-array literal (decl + ['a',
// 'b', …]) from src, failing the test when the declaration is missing or
// empty. Only the canonical-field lists use this shape; a multi-line array
// would not match and the test fails loudly instead of guessing.
func jsStringList(t *testing.T, src []byte, decl string) []string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(decl) + `\[([^\]]*)\]`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%q array literal not found", decl)
	}
	quotes := regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1)
	out := make([]string, 0, len(quotes))
	for _, q := range quotes {
		out = append(out, string(q[1]))
	}
	if len(out) == 0 {
		t.Fatalf("%q array literal has no entries", decl)
	}
	return out
}

// moduleRoot locates the repo root from this file's compile-time source path
// (the same self-location rule cmd/proxy/restart.go applies). A stripped
// source view (for example a -trimpath build) cannot self-locate its source,
// so the JS harness contract fails with that reason instead of skipping or
// guessing a path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Fatalf("cannot locate own source path (source paths stripped, e.g. -trimpath): JS harness contract cannot be verified")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s: JS harness contract cannot be verified without the repository source view", dir)
		}
		dir = parent
	}
}
