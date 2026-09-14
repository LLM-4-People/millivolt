package web

// Cross-language mirror pins for the dashboard's static JS sources. Each
// test reads the real embedded source (staticFS, like
// TestDashCfgSeedsMatchDefault) and locks a JS literal to its Go owner so
// drift reddens on the Go side without executing any JS. The companion
// jsdom pins in tests/ui_check.js cover the JS behavioral half of the pairs
// that need execution.

import (
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// jsObjectBlock extracts the source text of a `const NAME = { ... }`
// literal (through the first line-anchored closing "};"), failing the test
// when the declaration is missing. Only flat object literals with
// single-line members use this shape; a nested multi-line member would end
// the block early and fail loudly instead of guessing.
func jsObjectBlock(t *testing.T, src []byte, name string) string {
	t.Helper()
	decl := "const " + name + " = "
	start := strings.Index(string(src), decl)
	if start < 0 {
		t.Fatalf("static source missing const %s", name)
	}
	rest := string(src[start:])
	end := strings.Index(rest, "\n};")
	if end < 0 {
		t.Fatalf("const %s object literal is not terminated by a line-anchored };", name)
	}
	return rest[:end]
}

func jsObjectKeys(block string) []string {
	var keys []string
	for _, m := range regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*):`).FindAllStringSubmatch(block, -1) {
		keys = append(keys, m[1])
	}
	return keys
}

// TestExplorerTimeBucketLabelsMatchGoVocabulary pins explorer.js
// TIME_BUCKET_LABELS to the id vocabulary metrics.TimeBucket produces: the
// dashboard may relabel a bucket, but it can never drop, add, or rename an
// id without the Go owner changing first (the explorer renders an unknown
// bucket as its raw id).
func TestExplorerTimeBucketLabelsMatchGoVocabulary(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/explorer.js")
	if err != nil {
		t.Fatal(err)
	}
	keys := jsObjectKeys(jsObjectBlock(t, src, "TIME_BUCKET_LABELS"))
	if len(keys) == 0 {
		t.Fatal("TIME_BUCKET_LABELS declares no keys")
	}
	// The Go-derived vocabulary: every hour of one full week in the server's
	// local zone covers the weekend split and all three hour bands, so the
	// collected set is exactly the complete range of TimeBucket outputs.
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.Local) // a Monday
	seen := map[string]bool{}
	for i := range 7 * 24 {
		seen[metrics.TimeBucket(start.Add(time.Duration(i)*time.Hour))] = true
	}
	for _, id := range []string{"night", "work", "evening", "weekend"} {
		if !seen[id] {
			t.Fatalf("the week fixture never produced bucket %q; the vocabulary derivation is broken", id)
		}
	}
	sortedKeys := slices.Clone(keys)
	slices.Sort(sortedKeys)
	want := make([]string, 0, len(seen))
	for id := range seen {
		want = append(want, id)
	}
	slices.Sort(want)
	if !slices.Equal(sortedKeys, want) {
		t.Fatalf("TIME_BUCKET_LABELS keys = %v, want the Go TimeBucket vocabulary %v", sortedKeys, want)
	}
}

// TestConversationParentDimsMatchGoPivotOrder pins explorer.js
// CONVERSATION_PARENT_DIMS to the pivot dimension triple and order the
// lineage owner serves (conversation_lineage builds ParentScope in this
// order), so the dashboard's validity check can never accept a scope the
// server does not build.
func TestConversationParentDimsMatchGoPivotOrder(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/explorer.js")
	if err != nil {
		t.Fatal(err)
	}
	dims := jsStringList(t, src, "const CONVERSATION_PARENT_DIMS = ")
	want := []string{dimensionNames[dimClient], dimensionNames[dimKey], dimensionNames[dimConversation]}
	if !slices.Equal(dims, want) {
		t.Fatalf("CONVERSATION_PARENT_DIMS = %v, want the Go parent-scope order %v", dims, want)
	}
}

// TestSettingsCategoryRefKeysMatchGoCategories pins the chrome.js
// SETTINGS_CAT_REF registry to config.Categories(): the same category ids,
// in the same order, with no orphan on either side. settingsCatVisual falls
// back to the server visual for an unknown id, so a dropped or added entry
// would otherwise degrade silently.
func TestSettingsCategoryRefKeysMatchGoCategories(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	got := jsObjectKeys(jsObjectBlock(t, src, "SETTINGS_CAT_REF"))
	want := make([]string, 0, 16)
	for _, c := range config.Categories() {
		want = append(want, c.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("SETTINGS_CAT_REF keys = %v, want config.Categories() ids in order %v", got, want)
	}
}

// TestShellAssetsEqualBrandPaths pins the sw.js SHELL_ASSETS precache list
// to the exact Go brand-path set minus the service worker itself (the worker
// updates independently and never precaches its own URL). The previous
// coverage was presence-only; this equality pin also reddens when the list
// carries a path the mux never registers, or drops one it does.
func TestShellAssetsEqualBrandPaths(t *testing.T) {
	src, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	got := jsStringList(t, src, "const SHELL_ASSETS = ")
	want := make([]string, 0, len(brandByURL))
	for _, p := range BrandPaths() {
		if p != "/sw.js" {
			want = append(want, p)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("sw.js SHELL_ASSETS = %v, want the brand-path set %v", got, want)
	}
}

// staticJSLiteralCount counts how often a quoted JS string literal appears
// across every embedded static JS source (dashboard scripts plus the service
// worker; vendored code included, which has never carried these keys).
func staticJSLiteralCount(t *testing.T, literal string) int {
	t.Helper()
	count := 0
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".js" {
			return nil
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		count += strings.Count(string(b), literal)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// TestLiveEmptyKPIKeysMatchBootstrapWireKeys pins live.js's EMPTY_KPI (the
// kpiNow fallback document rendered before any aggregate has arrived) to
// kpiWireKeys, the same test-side list TestBootstrapWireKeysStrict asserts
// against the served bootstrap kpi section. The fallback was previously
// hand-spelled with zero test references, so a dropped, added, or renamed
// key drifted silently in both directions.
func TestLiveEmptyKPIKeysMatchBootstrapWireKeys(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/live.js")
	if err != nil {
		t.Fatal(err)
	}
	got := jsObjectKeys(jsObjectBlock(t, src, "EMPTY_KPI"))
	if len(got) == 0 {
		t.Fatal("EMPTY_KPI declares no keys")
	}
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	want := slices.Clone(kpiWireKeys)
	slices.Sort(want)
	if !slices.Equal(sorted, want) {
		t.Fatalf("live.js EMPTY_KPI keys = %v, want the bootstrap kpi wire set %v", sorted, want)
	}
}

// TestDashFiltersStorageKeySingleton: core.js FILTERS_STORAGE_KEY owns the
// saved-views storage key. A raw 'dash.filters' literal appearing anywhere
// else in the static JS would be a second authority (a key spelled by hand
// drifts silently from the constant), so the literal must occur exactly
// once across every static JS source.
func TestDashFiltersStorageKeySingleton(t *testing.T) {
	if got := staticJSLiteralCount(t, "'dash.filters'"); got != 1 {
		t.Errorf("'dash.filters' appears %d times across the static JS, want exactly 1 (core.js FILTERS_STORAGE_KEY is the single owner)", got)
	}
}

// TestShellCachePrefixSingleton: sw.js CACHE_PREFIX owns the shell cache
// name prefix. A raw 'millivolt-shell-' literal elsewhere in the static JS
// would bypass the const and could drift from the cache identity the
// version marker completes, so the literal must occur exactly once.
func TestShellCachePrefixSingleton(t *testing.T) {
	if got := staticJSLiteralCount(t, "'millivolt-shell-'"); got != 1 {
		t.Errorf("'millivolt-shell-' appears %d times across the static JS, want exactly 1 (sw.js CACHE_PREFIX is the single owner)", got)
	}
}
