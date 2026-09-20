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
	"strconv"
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

// TestRequestOverrideRuleCardWiringsPinnedInSource guards the chrome.js
// rule-card wirings jsdom cannot exercise: the three scope inputs'
// datalist associations, the generator's matching datalist id, the
// focus-time refresher's matching selector, and the rule-number label's
// routing through the one owner. The list attribute
// is native browser behavior, so ui_check can only see the rendered
// datalist elements and their options, never that an input is actually
// associated with its datalist; a hand-spelled generator id renders the
// same ids today but can drift from the list attributes under every green
// jsdom row; and the initial render's label is roRuleLabel's to produce
// (ui_check pins the renumbered text after a removal). A dropped or
// hand-spelled wiring reddens here before the editor can ship a scope
// input without its autocomplete or a rule head whose number drifts from
// the server's cited index.
func TestRequestOverrideRuleCardWiringsPinnedInSource(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	region := sourceRegion(t, string(src), "function roRuleCardHTML")
	for _, kind := range []string{"client", "provider", "model"} {
		wiring := "list=\"${roDlId('" + kind + "')}\""
		if !strings.Contains(region, wiring) {
			t.Errorf("roRuleCardHTML's %s scope input lost its datalist wiring %q", kind, wiring)
		}
	}
	if !strings.Contains(region, "roRuleLabel(i)") {
		t.Error("roRuleCardHTML's rule-number label no longer routes through roRuleLabel, the owner shared with roRenumber")
	}
	// The generator's id side of the same association: the datalist the
	// three list attributes reference must be the datalist roDatalistHTML
	// renders, and the id spelling routes through roDlId exactly like the
	// attributes do - a hand-spelled id is the one drift jsdom rows can
	// never see (the rendered string is identical while it matches).
	genRegion := sourceRegion(t, string(src), "function roDatalistHTML")
	if !strings.Contains(genRegion, `id="${roDlId(kind)}"`) {
		t.Error("roDatalistHTML's datalist id no longer routes through roDlId, the owner shared with the rule cards' list attributes and the focus-time refresher")
	}
	// The focus-time refresher's selector side of the same association:
	// roSyncDatalists must look its datalist up through roDlId too. The
	// focus wiring only runs in a real browser, and a hand-spelled
	// selector resolves the same ids today, so jsdom rows stay green
	// while the refresher drifts from the generator and the list
	// attributes - the pin is the only row that can see it.
	syncRegion := sourceRegion(t, string(src), "function roSyncDatalists")
	if !strings.Contains(syncRegion, `'#' + roDlId(kind)`) {
		t.Error("roSyncDatalists' datalist selector no longer routes through roDlId, the owner shared with the generator and the rule cards' list attributes")
	}
}

// TestRequestOverrideCapAndBandMatchGoOwner pins the two numeric
// contracts the request-overrides editor shares with config: the rule
// cap (chrome.js REQUEST_OVERRIDES_MAX, the count line and template
// add gate, against config.RequestOverridesMax, the load-boundary cap)
// and the body-ceiling band (chrome.js REQUEST_OVERRIDE_BODY_MAX plus
// validateRoRow's band check and message, against config's
// validateRequestOverrides comparison and errRange bounds). Both sides
// are read from their sources, the ForbiddenHeaders-pin precedent, and
// config's comparison and error wording must agree with each other too,
// so a one-sided Go edit cannot pass. Drift would let the editor accept
// a ceiling the server rejects, warn about one it accepts, or count
// rules against a different cap than the load boundary.
func TestRequestOverrideCapAndBandMatchGoOwner(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	cfgSrc := configOverrideSource(t)

	// The rule cap: both literals from source, the Go side confirmed
	// against the exported constant (its compile-time identity).
	jsCap := firstSubmatch(t, string(src), `const REQUEST_OVERRIDES_MAX = (\d+);`)
	goCap := firstSubmatch(t, cfgSrc, `const RequestOverridesMax = (\d+)`)
	if jsCap != goCap {
		t.Errorf("the rule cap differs: chrome.js REQUEST_OVERRIDES_MAX = %s, config RequestOverridesMax = %s", jsCap, goCap)
	}
	if n, err := strconv.Atoi(goCap); err != nil || n != config.RequestOverridesMax {
		t.Errorf("config.go's RequestOverridesMax literal %q disagrees with the exported config.RequestOverridesMax = %d", goCap, config.RequestOverridesMax)
	}

	// The body band, config side: the comparison and the errRange wording
	// inside validateRequestOverrides must spell the same pair.
	goBand := sourceRegion(t, cfgSrc, "func validateRequestOverrides")
	goMin := firstSubmatch(t, goBand, `\*f\.value < (\d+) \|\| \*f\.value > `)
	goMax := strings.ReplaceAll(firstSubmatch(t, goBand, `\*f\.value > ([\d_]+)`), "_", "")
	errMin := firstSubmatch(t, goBand, `, "(\d+)", "`)
	errMax := firstSubmatch(t, goBand, `, "(\d+)", strconv\.Itoa`)
	if goMin != errMin || goMax != errMax {
		t.Fatalf("config's body band disagrees with itself: comparison %s..%s, errRange %s..%s", goMin, goMax, errMin, errMax)
	}

	// The body band, chrome.js side: the ceiling const owns the number,
	// the live check and its message route through it, and both body
	// inputs' max attributes ride the same owner.
	jsMax := firstSubmatch(t, string(src), `const REQUEST_OVERRIDE_BODY_MAX = (\d+);`)
	if jsMax != goMax {
		t.Errorf("the body ceiling differs: chrome.js REQUEST_OVERRIDE_BODY_MAX = %s, config band max = %s", jsMax, goMax)
	}
	jsRow := sourceRegion(t, string(src), "function validateRoRow")
	jsMin := firstSubmatch(t, jsRow, `n < (\d+) \|\| n > REQUEST_OVERRIDE_BODY_MAX`)
	if jsMin != goMin {
		t.Errorf("the body band minimum differs: chrome.js validateRoRow uses %s, config uses %s", jsMin, goMin)
	}
	msgMin := firstSubmatch(t, jsRow, `between (\d+) and \$\{REQUEST_OVERRIDE_BODY_MAX\}`)
	if msgMin != goMin {
		t.Errorf("the band message minimum differs: chrome.js says between %s and the ceiling, config uses %s", msgMin, goMin)
	}
	if n := strings.Count(sourceRegion(t, string(src), "function roRuleCardHTML"), `max="${REQUEST_OVERRIDE_BODY_MAX}"`); n != 2 {
		t.Errorf("roRuleCardHTML wires %d body inputs through REQUEST_OVERRIDE_BODY_MAX, want 2", n)
	}
}

// TestSubConversationEditorWiringsPinnedInSource guards the chrome.js
// sub-conversations editor wirings jsdom cannot exercise: the client
// input's datalist association, the generator's matching datalist id, the
// focus-time refresher's matching selector, the options' shared known-set
// source, the entry label's routing through the one ordinal owner, and the
// add gates' and delegated remove branches' routing through the cap
// constants and the shared click wiring. The list attribute is native
// browser behavior, so ui_check can only see the rendered datalist and its
// options, never that the client input is actually associated with it; a
// hand-spelled generator id renders the same ids today but can drift from
// the list attribute under every green jsdom row. A dropped or hand-spelled
// wiring reddens here before the editor can ship a client input without
// its autocomplete, an entry head whose number drifts from the server's
// cited index, or an add control that ignores the cap.
func TestSubConversationEditorWiringsPinnedInSource(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	region := sourceRegion(t, string(src), "function scCardHTML")
	if !strings.Contains(region, `list="${scDlId()}"`) {
		t.Error(`scCardHTML's client input lost its datalist wiring "list=\"${scDlId()}\""`)
	}
	if !strings.Contains(region, "scEntryLabel(i)") {
		t.Error("scCardHTML's entry-number label no longer routes through scEntryLabel, the owner shared with scRenumber")
	}
	genRegion := sourceRegion(t, string(src), "function scDatalistHTML")
	if !strings.Contains(genRegion, `id="${scDlId()}"`) {
		t.Error("scDatalistHTML's datalist id no longer routes through scDlId, the owner shared with the entry cards' list attribute and the focus-time refresher")
	}
	syncRegion := sourceRegion(t, string(src), "function scSyncDatalists")
	if !strings.Contains(syncRegion, `'#' + scDlId()`) {
		t.Error("scSyncDatalists' datalist selector no longer routes through scDlId, the owner shared with the generator and the entry cards' list attribute")
	}
	if !strings.Contains(syncRegion, "scDlOptionsHTML()") {
		t.Error("scSyncDatalists' option refresh no longer derives through scDlOptionsHTML, the owner shared with the initial render")
	}
	// The options source: the same known-set union the request-overrides
	// scope datalists read seeds this editor's client autocomplete, so a
	// classified client observed anywhere on the dashboard is offered by
	// every editor alike.
	optRegion := sourceRegion(t, string(src), "function scDlOptionsHTML")
	if !strings.Contains(optRegion, "roKnownOptions('client')") {
		t.Error("scDlOptionsHTML no longer derives its options from roKnownOptions('client'), the shared known-set source the ro scope datalists read")
	}
	addRegion := sourceRegion(t, string(src), "function addScCard")
	if !strings.Contains(addRegion, ">= SUB_CONVERSATIONS_MAX") {
		t.Error("addScCard's entry add gate no longer routes through SUB_CONVERSATIONS_MAX")
	}
	paramRegion := sourceRegion(t, string(src), "function addScParamRow")
	if !strings.Contains(paramRegion, ">= SUB_CONVERSATION_PARAMS_MAX") {
		t.Error("addScParamRow's param add gate no longer routes through SUB_CONVERSATION_PARAMS_MAX")
	}
	clickRegion := sourceRegion(t, string(src), "function providersEditorClick")
	if !strings.Contains(clickRegion, "[data-sc-rm]") {
		t.Error("providersEditorClick lost the entry-card remove branch [data-sc-rm]")
	}
	if !strings.Contains(clickRegion, "[data-sc-p-rm]") {
		t.Error("providersEditorClick lost the param-row remove branch [data-sc-p-rm]")
	}
}

// TestSubConversationParamGrammarMatchGoOwner pins the chrome.js live param
// grammar to config.checkSubConversationParam, the single owner of the
// tracked-param name grammar, and the editor's live message vocabulary to
// the server's exact wording. Both sides are read from their sources (the
// ForbiddenHeaders-pin precedent): the byte bounds compare pairwise, the
// quote (0x22) and backslash (0x5c) rejections are identity constants
// spelled hex on the JS side and as the characters themselves on the Go
// side, and every shared error fragment must survive on both sides so the
// editor can never warn about a name the server accepts or accept one the
// server rejects with different words.
func TestSubConversationParamGrammarMatchGoOwner(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	jsSrc := string(src)
	cfgSrc := configOverrideSource(t)

	jsFn := sourceRegion(t, jsSrc, "function scParamOK")
	goFn := sourceRegion(t, cfgSrc, "func checkSubConversationParam")
	// Both sides spell the band in hex bytes (0x20..0x7e); the capture
	// must take the hex digits, or "0x7e" vs "0x60" would compare their
	// leading zeros and pass vacuously.
	goLower := firstSubmatch(t, goFn, `b < 0x([0-9a-fA-F]+)`)
	jsLower := firstSubmatch(t, jsFn, `c < 0x([0-9a-fA-F]+)`)
	goUpper := firstSubmatch(t, goFn, `b > 0x([0-9a-fA-F]+)`)
	jsUpper := firstSubmatch(t, jsFn, `c > 0x([0-9a-fA-F]+)`)
	if jsLower != goLower || jsUpper != goUpper {
		t.Errorf("the param byte band differs: chrome.js scParamOK rejects outside %s..%s, config checkSubConversationParam outside %s..%s", jsLower, jsUpper, goLower, goUpper)
	}
	if !strings.Contains(jsFn, "c === 0x22") || !strings.Contains(jsFn, "c === 0x5c") {
		t.Error("chrome.js scParamOK no longer rejects the quote (0x22) and backslash (0x5c) bytes")
	}
	if !strings.Contains(goFn, `b == '"'`) || !strings.Contains(goFn, `b == '\\'`) {
		t.Error("config checkSubConversationParam no longer rejects the quote and backslash bytes")
	}

	// The live message vocabulary: the sub_conversations[i] key prefix
	// becomes the card's inline context and %q becomes the quoted name,
	// but the wording fragments themselves are the server's.
	jsCard := sourceRegion(t, jsSrc, "function validateScCard")
	for _, wording := range []string{
		"is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)",
		"must not be empty or only whitespace",
	} {
		if !strings.Contains(goFn, wording) {
			t.Errorf("config checkSubConversationParam no longer carries the wording %q", wording)
		}
		if !strings.Contains(jsCard, wording) {
			t.Errorf("chrome.js validateScCard lost the server wording %q", wording)
		}
	}
	// The byte-bound message: the Go side routes through errRange with the
	// 1..N bytes shape; the JS side composes the same text from
	// SUB_CONVERSATION_PARAM_MAX_BYTES, which the caps pin ties to the
	// same number.
	if !strings.Contains(goFn, `errRange(key, "1", strconv.Itoa(subConversationParamMaxBytes)+" bytes"`) {
		t.Error(`config checkSubConversationParam's byte-bound error no longer routes through errRange(key, "1", strconv.Itoa(subConversationParamMaxBytes)+" bytes"`)
	}
	if !strings.Contains(jsCard, "must be 1..${SUB_CONVERSATION_PARAM_MAX_BYTES} bytes, got ") {
		t.Error("chrome.js validateScCard's byte-bound message no longer routes through SUB_CONVERSATION_PARAM_MAX_BYTES with the errRange wording")
	}

	// The list-level vocabulary: the required client, the required param,
	// the duplicate-param and duplicate-client rejections.
	goList := sourceRegion(t, cfgSrc, "func validateSubConversations")
	listWording := []struct{ goText, jsText, what string }{
		{"client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry",
			"client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry", "the required-client wording"},
		{"no param set - give the entry a request body field to track, or remove the entry",
			"no param set - give the entry a request body field to track, or remove the entry", "the no-param wording"},
		{"merge the entries or change one client", "merge the entries or change one client", "the duplicate-client remedy"},
		{`params: duplicate %q`, `params: duplicate '`, "the duplicate-param wording"},
		{"duplicate client %q with sub_conversations[%d]", `duplicate client '`, "the duplicate-client wording"},
	}
	for _, w := range listWording {
		if !strings.Contains(goList, w.goText) {
			t.Errorf("config validateSubConversations no longer carries %s %q", w.what, w.goText)
		}
		if !strings.Contains(jsCard, w.jsText) {
			t.Errorf("chrome.js validateScCard lost %s: the server says %q", w.what, w.goText)
		}
	}
}

// TestSubConversationCapsMatchGoOwner pins the three numeric contracts the
// sub-conversations editor shares with config (the
// TestRequestOverrideCapAndBandMatchGoOwner precedent): the entry cap
// (chrome.js SUB_CONVERSATIONS_MAX, the count line and entry add gate,
// against config.SubConversationsMax, the load-boundary cap), the params
// cap (chrome.js SUB_CONVERSATION_PARAMS_MAX, the param add gate, against
// config.SubConversationParamsMax) and the param byte bound (chrome.js
// SUB_CONVERSATION_PARAM_MAX_BYTES, the live grammar check, against the
// package-private subConversationParamMaxBytes, readable from source
// only), plus the count line's own label. Drift would let the editor count
// entries against a different cap than the load boundary or accept a
// param list or name the server rejects.
func TestSubConversationCapsMatchGoOwner(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	jsSrc := string(src)
	cfgSrc := configOverrideSource(t)

	jsEntries := firstSubmatch(t, jsSrc, `const SUB_CONVERSATIONS_MAX = (\d+);`)
	goEntries := firstSubmatch(t, cfgSrc, `SubConversationsMax\s*=\s*(\d+)`)
	if jsEntries != goEntries {
		t.Errorf("the entry cap differs: chrome.js SUB_CONVERSATIONS_MAX = %s, config SubConversationsMax = %s", jsEntries, goEntries)
	}
	if n, err := strconv.Atoi(goEntries); err != nil || n != config.SubConversationsMax {
		t.Errorf("config.go's SubConversationsMax literal %q disagrees with the exported config.SubConversationsMax = %d", goEntries, config.SubConversationsMax)
	}

	jsParams := firstSubmatch(t, jsSrc, `const SUB_CONVERSATION_PARAMS_MAX = (\d+);`)
	goParams := firstSubmatch(t, cfgSrc, `SubConversationParamsMax\s*=\s*(\d+)`)
	if jsParams != goParams {
		t.Errorf("the params cap differs: chrome.js SUB_CONVERSATION_PARAMS_MAX = %s, config SubConversationParamsMax = %s", jsParams, goParams)
	}
	if n, err := strconv.Atoi(goParams); err != nil || n != config.SubConversationParamsMax {
		t.Errorf("config.go's SubConversationParamsMax literal %q disagrees with the exported config.SubConversationParamsMax = %d", goParams, config.SubConversationParamsMax)
	}

	jsBytes := firstSubmatch(t, jsSrc, `const SUB_CONVERSATION_PARAM_MAX_BYTES = (\d+);`)
	goBytes := firstSubmatch(t, cfgSrc, `const subConversationParamMaxBytes = (\d+)`)
	if jsBytes != goBytes {
		t.Errorf("the param byte bound differs: chrome.js SUB_CONVERSATION_PARAM_MAX_BYTES = %s, config subConversationParamMaxBytes = %s", jsBytes, goBytes)
	}

	// The count line label: "N / 16 entries" names the list's own
	// vocabulary (the sub_conversations entries), like the ro count line
	// names its rules; and the add-button gate rides the same constant.
	countRegion := sourceRegion(t, jsSrc, "function scSyncCount")
	if label := firstSubmatch(t, countRegion, `\+ SUB_CONVERSATIONS_MAX \+ ' ([a-z]+)'`); label != "entries" {
		t.Errorf("the count line labels the cards %q, want \"entries\" (the sub_conversations list's own vocabulary)", label)
	}
	if !strings.Contains(countRegion, ">= SUB_CONVERSATIONS_MAX") {
		t.Error("scSyncCount's add-button gate no longer routes through SUB_CONVERSATIONS_MAX")
	}
}

// TestSubConversationEditorCopyMatchSchemaOwner pins the sub-conversations
// editor's condensed copy - the params label, the strip toggle label and the
// category hint - to the schema help's owner semantics for sub_conversations
// (config.FieldByKey, the live registry entry the settings help renders):
// top-level only, the first present field decides, the absent/drop split,
// and strip removing every configured field. The schema help once moved to
// new semantics while these condensed mirrors stayed behind, uncovered by
// any pin - the round-21 L8-1/L8-2 findings. Every essential fragment must
// survive on both sides, so neither an owner reword nor an editor reword
// can silently strand the other; the absent/drop split is pinned as its
// two halves, absent continuing the scan (the next field is checked) while
// drop stops it (no later field is consulted).
func TestSubConversationEditorCopyMatchSchemaOwner(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	f := config.FieldByKey("sub_conversations")
	if f == nil {
		t.Fatal("config.FieldByKey: the schema registry has no sub_conversations field")
	}
	card := sourceRegion(t, string(src), "function scCardHTML")
	hint := sourceRegion(t, string(src), "function subConversationsEditorHTML")
	for _, p := range []struct {
		what   string
		owner  string // the fragment the live schema help must still carry
		editor string // the condensed mirror the editor copy must still carry
		site   string // card, hint or both: the editor strings that must carry the mirror
	}{
		{"the top-level extraction scope", "matched only at the request body's top level", "top-level request body fields", "both"},
		{"the first-present rule", "the first present field decides", "the first present field decides", "both"},
		{"the absent class", "counts as absent and the next field is checked", "counts as absent and the next field is checked", "hint"},
		{"the unusable-string class", "cannot be decoded or fails the identity rules", "cannot be decoded or fails the identity rules", "hint"},
		{"the identity drop", "drops the identity for the request", "drops the identity for the request", "hint"},
		{"the no-later-consulted rule", "no later field is consulted", "no later field is consulted", "hint"},
		{"the strip scope", "configured fields", "every configured field present at the top level", "hint"},
		{"the strip toggle scope", "configured fields", "every configured field from the top level", "card"},
	} {
		if !strings.Contains(f.Help, p.owner) {
			t.Errorf("the sub_conversations schema help lost %s: %q is gone from the live owner wording", p.what, p.owner)
		}
		var ok bool
		switch p.site {
		case "card":
			ok = strings.Contains(card, p.editor)
		case "hint":
			ok = strings.Contains(hint, p.editor)
		default: // both: the params label and the category hint each carry it
			ok = strings.Contains(card, p.editor) && strings.Contains(hint, p.editor)
		}
		if !ok {
			t.Errorf("the sub-conversations editor copy lost %s: want %q in the editor strings (pinned site: %s)", p.what, p.editor, p.site)
		}
	}
}

// TestSettingsRevealCallSitesPinnedInSource pins applySettings' reveal
// discipline: every path that blocks a settings save must route its
// offender through revealSettingsOffender, the one reveal that can
// surface an offender whose category the sheet is not showing. Exactly
// four call sites live inside applySettings - the three editor save
// gates (model rules, request overrides, sub-conversations) and the
// collectSettingsValues(true) catch for scalar offenders - so a
// dropped gate call, a catch reverted to a bare focus(), or an ad-hoc
// fifth call reddens here. The jsdom rows cover each path's behavior,
// but none of them counts the call sites.
func TestSettingsRevealCallSitesPinnedInSource(t *testing.T) {
	src, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	region := sourceRegion(t, string(src), "function applySettings")
	if got := strings.Count(region, "revealSettingsOffender("); got != 4 {
		t.Errorf("applySettings routes %d offenders through revealSettingsOffender, want exactly 4 (the model-rules, request-overrides and sub-conversations save gates plus the collectSettingsValues catch)", got)
	}
	if got := strings.Count(string(src), "function revealSettingsOffender("); got != 1 {
		t.Errorf("chrome.js declares revealSettingsOffender %d times, want exactly 1 (the count above is applySettings' call sites, not declarations)", got)
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
