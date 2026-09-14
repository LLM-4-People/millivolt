package web

import (
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/LLM-4-People/millivolt/internal/config"
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
