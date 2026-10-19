package web

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func TestProjectionDimensionsShareStoredLabels(t *testing.T) {
	var d contribDimensions
	first := contrib{client: "fixture-client", prov: "fixture.example", model: "MODEL-A", conv: "conversation", key: "key-hash", status: 200}
	d.intern(&first)
	for range 16 {
		row := contrib{status: 200}
		for _, dim := range dimensionNames {
			if field := dimField(dim, &row); field != nil {
				*field = strings.Clone(*dimField(dim, &first))
			}
		}
		d.intern(&row)
		for dim, name := range dimensionNames {
			field := dimField(name, &row)
			if field == nil {
				continue
			}
			id := row.dimensionIDs[dim]
			canonical := d.dict[dim].names[id]
			// Pointer inspection is test-only: production uses ordinary
			// immutable Go strings, with no unsafe aliases or conversions.
			if *field != canonical || unsafe.StringData(*field) != unsafe.StringData(canonical) {
				t.Fatalf("%s retained a per-row copy instead of dictionary bytes", name)
			}
			if d.dict[dim].names[id] != *dimField(name, &first) {
				t.Fatalf("%s canonicalization changed its raw spelling", name)
			}
		}
	}
	for _, dim := range []int{dimClient, dimProvider, dimModel, dimConversation, dimKey} {
		if len(d.dict[dim].names) != 2 || d.dict[dim].counts[1] != 17 {
			t.Fatalf("%s duplicate changed membership: %+v", dimensionNames[dim], d.dict[dim])
		}
	}
}

func TestProjectionEmptyModelCanonicalization(t *testing.T) {
	for _, replacement := range []string{"unknown", ""} {
		t.Run(replacement, func(t *testing.T) {
			mcz := &modelCanonizer{exec: config.CompileModelRules([]config.ModelRule{{Mode: config.ModelRulePattern, From: "^$", To: replacement}}), memo: map[string]string{}}
			p := &projectionData{rows: []contrib{{id: "stored", status: 200, start: 1000, ttft: 10, tps: 2}}}
			p.dimensions.intern(&p.rows[0])
			fs := []scopeFilter{{dim: "model", id: replacement}}
			bound := bindScope(fs, p, mcz)
			if !scopedMatch(&p.rows[0], "", bound) {
				t.Fatal("projection lost valid empty-model canonicalization")
			}
			if fs[0].modelIDs != nil {
				t.Fatal("bound scope mutated caller")
			}
			raw, indexed := newExplorerFold("model", "", fs), newExplorerFold("model", "", fs)
			indexed.prepare(p, mcz)
			c := p.rows[0]
			c.dimensionIDs = [dimTool]uint32{}
			c.model = mcz.model(c.model)
			_ = raw.fold(&c)
			_ = indexed.fold(&p.rows[0])
			if got, want := indexed.payload(), raw.payload(); !reflect.DeepEqual(got, want) {
				t.Fatalf("empty model parity: got %+v, want %+v", got, want)
			}
		})
	}
}

// TestProjectionToolNamesShareDictionaryBytes pins the tool-name repoint in
// intern: the per-row decode copies must become garbage after interning, not
// stay live for the history's lifetime. Pointer inspection is test-only, like
// the stored-label pin above; production uses ordinary immutable Go strings.
func TestProjectionToolNamesShareDictionaryBytes(t *testing.T) {
	var d contribDimensions
	row := contrib{status: 200, toolsL: []string{strings.Clone("web_search"), strings.Clone("bash_exec")}}
	d.intern(&row)
	for i, name := range row.toolsL {
		canonical := d.dict[dimTool].names[row.toolIDs[i]]
		if name != canonical || unsafe.StringData(name) != unsafe.StringData(canonical) {
			t.Fatalf("tool name %q retained a per-row copy instead of dictionary bytes", name)
		}
	}
	for _, id := range row.toolIDs {
		if d.dict[dimTool].counts[id] != 1 {
			t.Fatalf("tool membership drifted: %+v", d.dict[dimTool])
		}
	}
}

func TestProjectionParentIdentitySharesConversationDictionary(t *testing.T) {
	var d contribDimensions
	child := contrib{conv: "child", parentConv: strings.Repeat("parent", 30), status: 200}
	d.intern(&child)
	parentID := d.dict[dimConversation].ids[child.parentConv]
	if parentID == 0 || d.dict[dimConversation].counts[parentID] != 0 {
		t.Fatal("a referenced parent fabricated an observed membership")
	}
	parent := contrib{conv: strings.Clone(child.parentConv), status: 200}
	d.intern(&parent)
	if unsafe.StringData(child.parentConv) != unsafe.StringData(parent.conv) || d.dict[dimConversation].counts[parentID] != 1 {
		t.Fatal("parent identity failed to reuse canonical bytes/counts")
	}
}

// TestDimensionNamesMirrorExplorerRail pins the dimension dictionary's name
// set. The dictionary indices are internal slots, but the NAMES own the API
// dimension allowlist (validDims) and are what the explorer rail mirrors
// (XP_RAIL_DIMS in explorer.js, whose order the jsdom suite pins). A
// renamed, added or removed dimension must redden here instead of silently
// stranding the browser rail or the filter grammar. Set equality is
// order-independent against the rail; the array's own order is pinned too
// because it defines the dictionary slot indices.
func modelCacheProjection(t *testing.T, models int) *projectionData {
	t.Helper()
	rows := make([]contrib, models)
	for i := range rows {
		rows[i] = contrib{model: fmt.Sprintf("Vendor/Model-%d", i), status: 200}
	}
	p, err := buildProjection(rows, storage.ProjectionCursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return &p
}

// The canonical model IDs and dictionary are projection-owned: consecutive
// requests share the immutable snapshot, a rules change rebuilds it, a
// non-destructive version bump extends it, and a destructive replacement
// publishes a fresh cache.
func TestProjectionModelCacheReuseAndInvalidation(t *testing.T) {
	p := modelCacheProjection(t, 1000)
	lower := &modelCanonizer{
		exec:     config.CompileModelRules([]config.ModelRule{{Mode: config.ModelRuleLower}}),
		identity: "lower",
		memo:     map[string]string{},
	}
	var first, second queryDimensions
	first.prepare(p, lower)
	second.prepare(p, lower)
	if len(first.modelIDs) != 1001 || len(second.modelIDs) != 1001 {
		t.Fatalf("canonical IDs = %d/%d, want 1001", len(first.modelIDs), len(second.modelIDs))
	}
	if &first.modelIDs[0] != &second.modelIDs[0] || &first.base[dimModel].names[0] != &second.base[dimModel].names[0] {
		t.Fatal("consecutive requests re-canonicalized the projection")
	}
	if got := first.base[dimModel].names[first.modelIDs[1]]; got != "vendor/model-0" {
		t.Fatalf("canonical model = %q", got)
	}
	raw := &modelCanonizer{identity: "raw", memo: map[string]string{}}
	var rulesChanged queryDimensions
	rulesChanged.prepare(p, raw)
	if rulesChanged.base[dimModel].names[rulesChanged.modelIDs[1]] != "Vendor/Model-0" {
		t.Fatal("rules change reused the old canonical dictionary")
	}
	// A version bump with an unchanged raw dictionary is a cache hit, not a
	// rebuild; a new raw name extends the prefix in place.
	names := p.dimensions.dict[dimModel].names
	ids, _ := p.modelCache.canonical(p.version+1, raw.identity, names, raw)
	if len(ids) != len(names) || &ids[0] != &rulesChanged.modelIDs[0] {
		t.Fatal("unchanged model dictionary rebuilt on an append version")
	}
	extended := append(append([]string(nil), names...), "vendor/model-new")
	ids, models := p.modelCache.canonical(p.version+2, raw.identity, extended, raw)
	if len(ids) != len(extended) || models.names[ids[len(extended)-1]] != "vendor/model-new" {
		t.Fatalf("extended dictionary = %q", models.names[ids[len(extended)-1]])
	}
	if ids[1] != rulesChanged.modelIDs[1] {
		t.Fatal("extension did not retain the immutable canonical prefix")
	}
	replacement := modelCacheProjection(t, 10)
	if replacement.modelCache == p.modelCache {
		t.Fatal("destructive replacement reused the previous cache")
	}
}

func TestDimensionNamesMirrorExplorerRail(t *testing.T) {
	want := []string{"client", "provider", "model", "conversation", "key", "status", "time", "tool", "error"}
	if !reflect.DeepEqual(dimensionNames[:], want) {
		t.Fatalf("dimensionNames = %v, want %v (the explorer rail's dimension set, in dictionary slot order)", dimensionNames[:], want)
	}
	if len(validDims) != len(want) {
		t.Fatalf("validDims = %v, want exactly the rail dimensions accepted", validDims)
	}
	for _, name := range want {
		if !validDims[name] {
			t.Fatalf("validDims[%q] = false, want every rail dimension accepted by the explorer API", name)
		}
	}
}
