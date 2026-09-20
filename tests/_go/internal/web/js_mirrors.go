package web

// Cross-language mirror pins for the dashboard's static JS sources. Each
// test reads the real embedded source (staticFS, like
// TestDashCfgSeedsMatchDefault) and locks a JS literal to its Go owner so
// drift reddens on the Go side without executing any JS. The companion
// jsdom pins in tests/ui_check.js cover the JS behavioral half of the pairs
// that need execution.

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
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

// TestModelRuleModesMatchGoVocabulary pins chrome.js MODEL_RULE_MODES to
// config's exported rule-mode constants (ModelRuleExact/Pattern/Lower - the
// literals ValidateModelRules switches on, so they are the complete Go
// vocabulary). The editor's mode dropdown and the draft fallback both read
// this table; previously only a ui_check hand list guarded it, so a Go-side
// mode rename or removal drifted silently. The label half of each pair is
// not presentation-only: mrModeSelectHTML renders it as the option's value
// attribute and collectModelRules sends that select value as the wire mode,
// so ui_check pins the rendered option values (the labels-as-values order)
// while this Go pin owns the declared value half's sequence.
func TestModelRuleModesMatchGoVocabulary(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	decl := "const MODEL_RULE_MODES = "
	start := strings.Index(string(src), decl)
	if start < 0 {
		t.Fatal("static source missing const MODEL_RULE_MODES")
	}
	line := string(src[start:])
	if end := strings.Index(line, ";"); end >= 0 {
		line = line[:end]
	}
	var got []string
	for _, m := range regexp.MustCompile(`\['([^']+)'(?:\s*,\s*'[^']*')?\]`).FindAllStringSubmatch(line, -1) {
		got = append(got, m[1])
	}
	if len(got) == 0 {
		t.Fatal("MODEL_RULE_MODES declares no [value, label] pairs")
	}
	want := []string{config.ModelRuleExact, config.ModelRulePattern, config.ModelRuleLower}
	if !slices.Equal(got, want) {
		t.Fatalf("chrome.js MODEL_RULE_MODES values = %v, want the config rule-mode vocabulary %v", got, want)
	}
}

// configOverrideSource reads internal/config/config.go from the repository
// root (moduleRoot's self-location rule, the same one the ui_check.js
// contract uses). Config's forbidden-override-header set is package-private,
// so its source text is this pin's only enumerable view: the exported
// ForbiddenOverrideHeader oracle can confirm members but cannot list them.
// A trimpath build skips with the reason instead of guessing a path.
func configOverrideSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal", "config", "config.go"))
	if err != nil {
		t.Fatalf("read internal/config/config.go: %v", err)
	}
	return string(b)
}

// sourceRegion returns src from decl through the first line-anchored closing
// brace. Literal extractions are scoped to their owning declaration this way,
// so a same-looking fragment elsewhere in the file can never satisfy a pin
// silently, and a missing or unterminated region fails loudly.
func sourceRegion(t *testing.T, src, decl string) string {
	t.Helper()
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("source missing %q", decl)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("%q is not terminated by a line-anchored closing brace", decl)
	}
	return rest[:end]
}

// firstSubmatch returns the first capture of re in src, failing the test
// when the pattern is absent.
func firstSubmatch(t *testing.T, src, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("pattern %s not found", re)
	}
	return m[1]
}

// jsLiteralPairs extracts the name-to-value entries of a flat JS object
// literal block: quoted ('name') and bare-identifier (name) keys with
// single-quoted values, one per line. A duplicate key fails loudly rather
// than letting map order decide which entry the pin compares.
func jsLiteralPairs(t *testing.T, block string) map[string]string {
	t.Helper()
	pairs := make(map[string]string)
	for _, m := range regexp.
		MustCompile(`(?m)^\s*(?:'([^']+)'|([A-Za-z][A-Za-z0-9_-]*))\s*:\s*'([^']+)'\s*,?$`).
		FindAllStringSubmatch(block, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if _, dup := pairs[name]; dup {
			t.Fatalf("duplicate object entry %q", name)
		}
		pairs[name] = m[3]
	}
	return pairs
}

// goMapStringStringPairs extracts the entries of a map[string]string literal
// block ("name": "value", one per line). A duplicate key fails loudly.
func goMapStringStringPairs(t *testing.T, block string) map[string]string {
	t.Helper()
	pairs := make(map[string]string)
	for _, m := range regexp.
		MustCompile(`(?m)^\s*"([^"]+)"\s*:\s*"([^"]+)"\s*,?$`).
		FindAllStringSubmatch(block, -1) {
		if _, dup := pairs[m[1]]; dup {
			t.Fatalf("duplicate map entry %q", m[1])
		}
		pairs[m[1]] = m[2]
	}
	return pairs
}

// TestRequestOverrideForbiddenHeadersMatchGoVocabulary pins chrome.js
// RO_FORBIDDEN_HEADERS, the request-overrides editor's live validation
// vocabulary, to config's forbidden-header owner, name by name and reason by
// reason, plus the control-prefix rule both sides share. The JS half is read
// from the embedded chrome.js literal; the config half is derived from the
// forbiddenOverrideHeaders map source and confirmed against the exported
// ForbiddenOverrideHeader oracle, so a stale parse can never pass. Either
// side dropping, adding or relabeling an entry reddens here before the
// editor and the server can disagree about what an operator may override.
func TestRequestOverrideForbiddenHeadersMatchGoVocabulary(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	jsPairs := jsLiteralPairs(t, jsObjectBlock(t, src, "RO_FORBIDDEN_HEADERS"))
	if len(jsPairs) == 0 {
		t.Fatal("RO_FORBIDDEN_HEADERS declares no entries")
	}
	cfgSrc := configOverrideSource(t)
	goPairs := goMapStringStringPairs(t, sourceRegion(t, cfgSrc, "forbiddenOverrideHeaders = map[string]string{"))
	if len(goPairs) == 0 {
		t.Fatal("config forbiddenOverrideHeaders declares no entries")
	}
	jsNames := slices.Sorted(maps.Keys(jsPairs))
	goNames := slices.Sorted(maps.Keys(goPairs))
	if !slices.Equal(jsNames, goNames) {
		t.Fatalf("forbidden override header names differ:\nchrome.js RO_FORBIDDEN_HEADERS = %v\nconfig forbiddenOverrideHeaders = %v", jsNames, goNames)
	}
	for _, name := range goNames {
		if jsPairs[name] != goPairs[name] {
			t.Errorf("forbidden header %q: chrome.js reason %q, config reason %q", name, jsPairs[name], goPairs[name])
		}
		if reason, forbidden := config.ForbiddenOverrideHeader(name); !forbidden || reason != goPairs[name] {
			t.Errorf("config.ForbiddenOverrideHeader(%q) = (%q, %v), want (%q, true)", name, reason, forbidden, goPairs[name])
		}
	}

	// The prefix rule: the x-proxy- control denial. Both literals are
	// scoped to their owning functions (roForbiddenReason mirrors
	// ForbiddenOverrideHeader), the two sides must agree, and the oracle
	// proves the parsed Go literals are the live behavior.
	jsFn := sourceRegion(t, string(src), "function roForbiddenReason")
	goFn := sourceRegion(t, cfgSrc, "func ForbiddenOverrideHeader(")
	jsPrefix := firstSubmatch(t, jsFn, `startsWith\('([^']+)'\)`)
	goPrefix := firstSubmatch(t, goFn, `strings\.HasPrefix\(lower, "([^"]+)"\)`)
	jsControl := firstSubmatch(t, jsFn, `return '([^']+)'`)
	goControl := firstSubmatch(t, goFn, `return "([^"]+)", true`)
	if jsPrefix != goPrefix || jsControl != goControl {
		t.Fatalf("the control-prefix rule differs: chrome.js (%q returns %q), config (%q returns %q)", jsPrefix, jsControl, goPrefix, goControl)
	}
	if reason, forbidden := config.ForbiddenOverrideHeader(jsPrefix + "pin"); !forbidden || reason != goControl {
		t.Errorf("config.ForbiddenOverrideHeader(%q) = (%q, %v), want (%q, true)", jsPrefix+"pin", reason, forbidden, goControl)
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
