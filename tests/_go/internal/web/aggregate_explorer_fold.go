package web

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func explorerFoldFixtures() []contrib {
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.Local).UnixMilli()
	rows := make([]contrib, 137) // cross multiple bitset words
	for i := range rows {
		rows[i] = contrib{
			id: fmt.Sprint(i), start: start + int64(i)*60000, status: 200,
			client: fmt.Sprintf("client-%03d", i), prov: "old.example",
			model: "MODEL-A", conv: fmt.Sprint(i % 11), key: fmt.Sprint(i % 3),
			cost: float64(i%5) / 100, in: 100, out: 20, cacheR: 25, reason: 4,
			ttft: int64(100 + i), tps: float64(i)/4 + 1,
		}
		if i%2 == 0 {
			rows[i].model = "model-a"
			rows[i].toolsL = []string{"lookup", "", "lookup", "read"}
			rows[i].tools = 3
		}
		if i%3 == 0 {
			rows[i].prov = "new.example"
		}
		if i%5 == 0 {
			rows[i].status, rows[i].isErr = 500, true
			rows[i].ent = []errEnt{
				{typ: "upstream", code: "500", msg: "failure", at: rows[i].start},
				{typ: "upstream", code: "500", msg: "failure", at: rows[i].start + 100},
				{typ: "timeout", code: "408", at: rows[i].start + 200},
			}
		}
	}
	// No single-valued optional memberships; status/time still count.
	rows = append(rows, contrib{id: "empty", start: start, status: 200})
	return rows
}

func explorerProjection(rows []contrib) *projectionData {
	p := &projectionData{rows: slices.Clone(rows), ids: make(map[string]struct{}, len(rows))}
	for i := range p.rows {
		p.rows[i].rowIndex = uint32(i + 1)
		p.dimensions.intern(&p.rows[i])
		p.ids[p.rows[i].id] = struct{}{}
	}
	p.metrics.append(p.rows, 0)
	p.lineage = buildConversationTopology(p.rows)
	return p
}

func TestExplorerEncodedFoldMatchesRaw(t *testing.T) {
	rows := explorerFoldFixtures()
	p := explorerProjection(rows)
	rules := config.CompileModelRules([]config.ModelRule{{Mode: config.ModelRuleLower}})
	mcz := &modelCanonizer{exec: rules, memo: make(map[string]string)}
	for _, dim := range dimensionNames {
		for _, fs := range [][]scopeFilter{
			nil,
			{{dim: "provider", id: "old.example"}},
			{{dim: "model", id: "model-a"}, {dim: "tool", id: "lookup"}},
			{{dim: "error", id: "upstream|500|failure"}},
			{{dim: "client", id: "missing"}},
		} {
			t.Run(fmt.Sprint(dim, fs), func(t *testing.T) {
				raw, encoded := newExplorerFold(dim, "", fs), newExplorerFold(dim, "", fs)
				encoded.prepare(p, mcz)
				for i, c := range rows {
					c.model = mcz.model(c.model)
					if err := raw.fold(&c); err != nil {
						t.Fatal(err)
					}
					if err := encoded.fold(&p.rows[i]); err != nil {
						t.Fatal(err)
					}
				}
				// Local ring/pending membership can reuse projected IDs or add
				// fresh names without ever mutating the shared dictionary.
				extra := contrib{id: "extra", start: rows[0].start, live: true, stream: true,
					client: "new-client", prov: "old.example", model: "model-a", toolsL: []string{"lookup", "new-tool"}}
				_ = raw.fold(&extra)
				_ = encoded.fold(&extra)
				for _, g := range encoded.groups {
					if len(g.ttftS) != 0 || len(g.tpsS) != 0 {
						t.Fatalf("projected group %q copied metric samples", g.name)
					}
				}
				if got, want := encoded.payload(), raw.payload(); !reflect.DeepEqual(got, want) {
					t.Fatalf("encoded payload differs from raw:\ngot  %+v\nwant %+v", got, want)
				}
				if len(encoded.seen) != 0 {
					t.Fatalf("retained %d historical request IDs with no pending rows", len(encoded.seen))
				}
			})
		}
	}
	if _, changed := p.dimensions.dict[dimClient].ids["new-client"]; changed {
		t.Fatal("query mutated projected dictionary")
	}
}

// TestExplorerGroupWireKeySet pins the group card's wire shape: the marshaled
// group carries exactly this key set (plus the conversation dim's lineage
// block), nothing more. A key re-added to jsonEnt - the dropped reasoning and
// spark inputs, or any new field - fails here before it drifts into the
// dashboard unseen.
func TestExplorerGroupWireKeySet(t *testing.T) {
	rows := explorerFoldFixtures()
	for i, dim := range dimensionNames {
		f := newExplorerFold(dim, "", nil)
		for j := range rows {
			if err := f.fold(&rows[j]); err != nil {
				t.Fatal(err)
			}
		}
		groups := f.payload().Groups
		if len(groups) == 0 {
			t.Fatalf("%s: fold produced no groups", dim)
		}
		raw, err := json.Marshal(groups[0])
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{
			"name": true, "n": true, "cost": true, "in": true, "out": true,
			"cache": true, "err_final": true, "rate_limit_requests": true,
			"tools": true, "cost_per_mtok": true, "ttft_p50": true,
			"ttft_p95": true, "tps_p50": true, "tps_p95": true,
			"err_events": true, "code": true, "last_ms": true,
		}
		if i == dimConversation {
			want["conversation"] = true
		}
		got := make(map[string]bool, len(decoded))
		for k := range decoded {
			got[k] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: group wire keys %v, want %v", dim, got, want)
		}
	}
}

func TestExplorerFoldOccurrenceSemantics(t *testing.T) {
	rows := []contrib{
		{id: "a", start: 1000, status: 500, isErr: true, cost: .1, in: 10, out: 2,
			toolsL: []string{"lookup", "lookup", ""}, tools: 2,
			ent: []errEnt{{typ: "failure", code: "500", at: 1100}, {typ: "failure", code: "500", at: 1200}}},
		{id: "b", start: 2000, status: 200, toolsL: []string{"lookup"}, tools: 1,
			ent: []errEnt{{typ: "failure", code: "500", at: 2100}}},
	}
	for _, dim := range []string{"tool", "error"} {
		f := newExplorerFold(dim, "", nil)
		for _, c := range rows {
			_ = f.fold(&c)
		}
		p := f.payload()
		wantErrTotal := int64(1)
		if dim == "error" {
			// Both rows carry an error card membership, including the
			// recovered row whose final status is 200.
			wantErrTotal = 2
		}
		if p.Total != 2 || p.ErrorTot != wantErrTotal || p.Rail[dim] != 1 || len(p.Groups) != 1 {
			t.Fatalf("%s scope/rail = %+v", dim, p)
		}
		g := p.Groups[0]
		wantCost := .1 // one affected request reports .1; b reports 0
		if dim == "error" {
			wantCost = .2 // error events keep occurrence multiplicity
		}
		if g.N != 2 || g.Cost != wantCost || g.ErrFinal != 1 {
			t.Fatalf("%s affected-request aggregation = %+v", dim, g)
		}
		if dim == "error" {
			if g.ErrEvents != 3 || g.LastMs != 2100 || g.Code != "500" {
				t.Fatalf("error occurrence metadata = %+v", g)
			}
		} else if g.Tools != 3 {
			// Two calls on request a plus one on request b, summed once per
			// affected request rather than once per repeated tool name.
			t.Fatalf("tool calls = %d, want 3", g.Tools)
		}
	}
}

func TestExplorerFoldPendingDedupeIsBounded(t *testing.T) {
	f := newExplorerFold("status", "", nil)
	f.pendingIDs = map[string]struct{}{"stored": {}, "ring": {}, "live": {}}
	p := &projectionData{ids: map[string]struct{}{"stored": {}, "irrelevant": {}}}
	f.prepare(p, nil)
	for _, c := range []contrib{{id: "stored", status: 500, isErr: true}, {id: "ring", status: 200}, {id: "other", status: 200}} {
		_ = f.fold(&c)
	}
	if !reflect.DeepEqual(f.seen, map[string]struct{}{"stored": {}, "ring": {}}) {
		t.Fatalf("seen must contain only finalized pending candidates: %v", f.seen)
	}
	pending := []*metrics.Record{{ID: "stored", Stream: true}, {ID: "ring", Stream: true}, {ID: "live", Stream: true}}
	if err := eachPending(pending, f.seen, nil, f.fold); err != nil {
		t.Fatal(err)
	}
	got := f.payload()
	if got.Total != 4 || got.ErrorTot != 1 || got.Rail["status"] != 3 {
		t.Fatalf("pending snapshot regressed finalized state or duplicated rows: %+v", got)
	}
}

// Metric sample counting follows the projected memberships exactly (error
// keeps failure-event occurrences; tool keeps one sample per affected
// request), and the per-dimension membership owner retains one entry per
// projected membership (single id per row for single-valued dims, CSR for
// multi).
func TestExplorerFoldMetricCountsFollowProjectedMemberships(t *testing.T) {
	rows := explorerFoldFixtures()
	p := explorerProjection(rows)
	mcz := &modelCanonizer{exec: config.CompileModelRules([]config.ModelRule{{Mode: config.ModelRuleLower}}), memo: make(map[string]string)}
	wants := map[string]map[string]int{
		"model": {"model-a": 137},
		"tool":  {"lookup": 69, "read": 69}, // repeated tool names in a row are one membership
		"error": {"upstream|500|failure": 56, "timeout|408|": 28},
	}
	for dim, counts := range wants {
		f := newExplorerFold(dim, "", nil)
		f.prepare(p, mcz)
		for i := range p.rows {
			_ = f.fold(&p.rows[i])
		}
		var memberships int
		for _, count := range counts {
			memberships += count
		}
		for _, g := range f.groups {
			want := counts[g.name]
			if g.ttftN != want || g.tpsN != want || len(g.ttftS) != 0 || len(g.tpsS) != 0 {
				t.Fatalf("%s/%s metric counts=%d/%d, copied samples=%d/%d, want membership=%d",
					dim, g.name, g.ttftN, g.tpsN, len(g.ttftS), len(g.tpsS), want)
			}
		}
		if f.multiMember {
			if len(f.members) != memberships {
				t.Fatalf("%s retained %d CSR occurrences, want %d", dim, len(f.members), memberships)
			}
		} else {
			// Single-valued dimensions own one group id per projected row.
			kept := 0
			for _, id := range f.singleMember {
				if id != 0 {
					kept++
				}
			}
			if kept != memberships {
				t.Fatalf("%s retained %d row memberships, want %d", dim, kept, memberships)
			}
		}
	}
}

func TestExplorerRankedMetricsMergeExtras(t *testing.T) {
	rows := make([]contrib, 3)
	for i := range rows {
		rows[i] = contrib{id: fmt.Sprint(i), prov: "old.example", status: 200,
			start: int64(i+1) * 1000, ttft: int64(i+1) * 10, tps: float64(i + 1),
			toolsL: []string{"lookup"}, ent: []errEnt{{typ: "failure"}}}
		if i == 1 {
			rows[i].toolsL = append(rows[i].toolsL, "lookup")
			rows[i].ent = append(rows[i].ent, rows[i].ent[0])
		}
	}
	p := explorerProjection(rows)
	for _, dim := range []string{"provider", "tool", "error"} {
		f := newExplorerFold(dim, "", nil)
		f.prepare(p, nil)
		for i := range p.rows {
			_ = f.fold(&p.rows[i])
		}
		if dim == "provider" && f.payload().Groups[0].TTFTP50 != nil {
			t.Fatal("three projected samples bypassed the sparse gate")
		}
		extra := contrib{id: "ring", prov: "old.example", status: 200, start: 4000,
			ttft: 100, tps: 10, toolsL: []string{"lookup"}, ent: []errEnt{{typ: "failure"}}}
		_ = f.fold(&extra)
		g := f.payload().Groups[0]
		wantN, wantP50, wantP95 := int64(4), 25.0, 89.5
		if dim == "error" {
			// Error samples still follow failure-event occurrences: row 1's
			// repeated entry contributes a second sample.
			wantP50, wantP95 = 20, 86
			if g.ErrEvents != 5 {
				t.Fatalf("error occurrences = %d, want 5", g.ErrEvents)
			}
		}
		if g.N != wantN || g.TTFTP50 == nil || *g.TTFTP50 != wantP50 || g.TTFTP95 == nil || math.Abs(*g.TTFTP95-wantP95) > 1e-10 ||
			g.TPSP50 == nil || *g.TPSP50 != wantP50/10 || g.TPSP95 == nil || math.Abs(*g.TPSP95-wantP95/10) > 1e-10 {
			t.Fatalf("%s projected/extras percentile merge = %+v", dim, g)
		}
		for _, acc := range f.groups {
			if len(acc.ttftS) != 1 || len(acc.tpsS) != 1 {
				t.Fatalf("%s retained projected samples: %d/%d", dim, len(acc.ttftS), len(acc.tpsS))
			}
		}
	}
}

func TestExplorerFoldTopCardsBeforeSampleDerivation(t *testing.T) {
	for _, dim := range []string{"client", "error"} {
		f := newExplorerFold(dim, "", nil)
		// Equal request counts deliberately arrive in reverse lexical order.
		for i := xpNodeCap + 5; i >= 0; i-- {
			name := fmt.Sprintf("entity-%02d", i)
			for sample, ttft := range []int64{40, 10, 30, 20} {
				c := contrib{id: fmt.Sprint(sample), client: name, ttft: ttft, tps: float64(ttft) / 10, start: int64(sample)}
				if dim == "error" {
					c.ent = []errEnt{{typ: name}}
					// More error occurrences must not outrank more affected IDs.
					if i >= xpNodeCap {
						c.ent = append(c.ent, c.ent[0], c.ent[0])
					}
				}
				_ = f.fold(&c)
			}
		}
		before := make(map[string][]int64)
		for _, g := range f.groups {
			before[g.name] = slices.Clone(g.ttftS)
		}
		p := f.payload()
		if len(p.Groups) != xpNodeCap || cap(p.Groups) != xpNodeCap {
			t.Fatalf("retained cards %d/%d", len(p.Groups), cap(p.Groups))
		}
		selected := make(map[string]struct{})
		for i, g := range p.Groups {
			want := fmt.Sprintf("entity-%02d", i)
			if dim == "error" {
				want += "||"
			}
			if g.Name != want || g.N != 4 || g.TTFTP50 == nil || *g.TTFTP50 != 25 || g.TPSP95 == nil || math.Abs(*g.TPSP95-3.85) > 1e-12 {
				t.Fatalf("rank/percentile card %d = %+v", i, g)
			}
			selected[g.Name] = struct{}{}
		}
		for _, g := range f.groups {
			if _, kept := selected[g.name]; !kept && !slices.Equal(g.ttftS, before[g.name]) {
				t.Fatalf("discarded group %q samples were needlessly selected/sorted", g.name)
			}
		}
	}
}

// explorerPercentilesFromOccurrences is the independent oracle for the compact
// selection test: it ranks one group's exact occurrence multiset with the
// shared R7 position owner, without reading the explorer's selection path.
func explorerPercentilesFromOccurrences(values []float64) (p50, p95 *float64) {
	if len(values) < pctMinSamples {
		return nil, nil
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	at := func(p float64) *float64 {
		lo, hi, fraction, _ := percentilePosition(len(sorted), p)
		v := percentileInterpolate(sorted[lo], sorted[hi], fraction)
		return &v
	}
	return at(50), at(95)
}

func checkExplorerPercentilePair(t *testing.T, label string, gotP50, gotP95, wantP50, wantP95 *float64) {
	t.Helper()
	got, want := [2]*float64{gotP50, gotP95}, [2]*float64{wantP50, wantP95}
	for i := range got {
		if (got[i] == nil) != (want[i] == nil) {
			t.Fatalf("%s percentile %d = %v, want %v", label, i, got[i], want[i])
		}
		if got[i] != nil && math.Abs(*got[i]-*want[i]) > 1e-12 {
			t.Fatalf("%s percentile %d = %v, want %v", label, i, *got[i], *want[i])
		}
	}
}

func explorerGroupNames(groups []jsonEnt) map[string]struct{} {
	names := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		names[g.Name] = struct{}{}
	}
	return names
}

// TestExplorerPayloadCompactSelectionEquivalence pins the compact selected-group
// lookup to an occurrence-level oracle. It covers a single-valued dimension
// whose selected ids sit far above xpNodeCap, tool and error dimensions with
// duplicate occurrences inside one row, and groups outside the returned top
// that must never contribute a bucket.
func TestExplorerPayloadCompactSelectionEquivalence(t *testing.T) {
	t.Run("single-valued/large-id-gap", func(t *testing.T) {
		const coldRows = 5000
		rows := make([]contrib, 0, coldRows+120)
		wantTTFT, wantTPS := make(map[string][]float64), make(map[string][]float64)
		for i := 0; i < coldRows; i++ {
			name, ttft, tps := fmt.Sprintf("cold-%05d", i), int64(1000+i), float64(i+1)/2
			rows = append(rows, contrib{id: fmt.Sprintf("cold-row-%05d", i), start: int64(i+1) * 1000,
				status: 200, client: name, prov: "p", model: "m", ttft: ttft, tps: tps})
			wantTTFT[name], wantTPS[name] = append(wantTTFT[name], float64(ttft)), append(wantTPS[name], tps)
		}
		for i := 0; i < 120; i++ {
			name, ttft, tps := fmt.Sprintf("hot-%02d", i%10), int64(100000+i), float64(100+i)
			rows = append(rows, contrib{id: fmt.Sprintf("hot-row-%03d", i), start: int64(coldRows+i+1) * 1000,
				status: 200, client: name, prov: "p", model: "m", ttft: ttft, tps: tps})
			wantTTFT[name], wantTPS[name] = append(wantTTFT[name], float64(ttft)), append(wantTPS[name], tps)
		}
		p := explorerProjection(rows)
		f := newExplorerFold("client", "", nil)
		f.prepare(p, nil)
		for i := range p.rows {
			if err := f.fold(&p.rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		var hotID uint32
		for id, g := range f.groups {
			if g.name == "hot-00" {
				hotID = id
			}
		}
		if hotID <= coldRows || hotID <= xpNodeCap {
			t.Fatalf("selected hot id = %d, want a gap above %d cold ids and xpNodeCap %d", hotID, coldRows, xpNodeCap)
		}
		got := f.payload()
		if len(got.Groups) != xpNodeCap || got.Groups[0].Name != "hot-00" {
			t.Fatalf("selected cards = %+v", got.Groups)
		}
		for _, group := range got.Groups {
			p50, p95 := explorerPercentilesFromOccurrences(wantTTFT[group.Name])
			checkExplorerPercentilePair(t, group.Name+" ttft", group.TTFTP50, group.TTFTP95, p50, p95)
			p50, p95 = explorerPercentilesFromOccurrences(wantTPS[group.Name])
			checkExplorerPercentilePair(t, group.Name+" tps", group.TPSP50, group.TPSP95, p50, p95)
		}
		if _, kept := explorerGroupNames(got.Groups)["cold-00014"]; kept {
			t.Fatal("group outside top produced a card")
		}
	})

	t.Run("tool/affected-vs-calls", func(t *testing.T) {
		rows := make([]contrib, 40)
		wantTTFT, wantTPS := make(map[string][]float64), make(map[string][]float64)
		var wantCalls int64
		for i := range rows {
			ttft, tps := int64(10*i+1), float64(i+1)/4
			var tools []string
			if i < 30 {
				tools = []string{fmt.Sprintf("tool-%02d", i), "hot", "hot"}
			} else {
				tools = []string{"hot", "tool-00"}
			}
			rows[i] = contrib{id: fmt.Sprint(i), start: int64(i+1) * 1000, status: 200,
				toolsL: tools, tools: int64(len(tools)), ttft: ttft, tps: tps}
			wantCalls += int64(len(tools))
			// Repeated names are calls, not extra affected requests: one
			// percentile sample per request per tool card.
			seen := make(map[string]struct{}, len(tools))
			for _, name := range tools {
				if _, duplicate := seen[name]; duplicate {
					continue
				}
				seen[name] = struct{}{}
				wantTTFT[name], wantTPS[name] = append(wantTTFT[name], float64(ttft)), append(wantTPS[name], tps)
			}
		}
		p := explorerProjection(rows)
		f := newExplorerFold("tool", "", nil)
		f.prepare(p, nil)
		for i := range p.rows {
			if err := f.fold(&p.rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		got := f.payload()
		if len(got.Groups) != xpNodeCap || got.Groups[0].Name != "hot" || got.Groups[0].N != 40 || got.Groups[0].Tools != wantCalls {
			t.Fatalf("affected tool card = %+v, want hot N=40 calls=%d", got.Groups[0], wantCalls)
		}
		for _, group := range got.Groups {
			p50, p95 := explorerPercentilesFromOccurrences(wantTTFT[group.Name])
			checkExplorerPercentilePair(t, group.Name+" ttft", group.TTFTP50, group.TTFTP95, p50, p95)
			p50, p95 = explorerPercentilesFromOccurrences(wantTPS[group.Name])
			checkExplorerPercentilePair(t, group.Name+" tps", group.TPSP50, group.TPSP95, p50, p95)
		}
		names := explorerGroupNames(got.Groups)
		for _, excluded := range []string{"tool-23", "tool-29"} {
			if _, kept := names[excluded]; kept {
				t.Fatalf("group %q outside top produced a card", excluded)
			}
		}
		if again := f.payload(); !reflect.DeepEqual(got, again) {
			t.Fatal("payload differs across calls on one completed fold")
		}
	})

	t.Run("error/affected-vs-occurrences", func(t *testing.T) {
		rows := make([]contrib, 40)
		wantTTFT, wantTPS := make(map[string][]float64), make(map[string][]float64)
		for i := range rows {
			ttft, tps := int64(10*i+1), float64(i+1)/4
			var ent []errEnt
			if i < 30 {
				ent = []errEnt{{typ: "boom", code: "500"}, {typ: "boom", code: "500"},
					{typ: fmt.Sprintf("kind-%02d", i), code: "500"}}
			} else {
				ent = []errEnt{{typ: "boom", code: "500"}, {typ: "kind-00", code: "500"}}
			}
			rows[i] = contrib{id: fmt.Sprint(i), start: int64(i+1) * 1000, status: 500, isErr: true,
				ent: ent, ttft: ttft, tps: tps}
			for _, e := range ent {
				key := errorKey(e)
				wantTTFT[key], wantTPS[key] = append(wantTTFT[key], float64(ttft)), append(wantTPS[key], tps)
			}
		}
		p := explorerProjection(rows)
		f := newExplorerFold("error", "", nil)
		f.prepare(p, nil)
		for i := range p.rows {
			if err := f.fold(&p.rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		got := f.payload()
		boom := got.Groups[0]
		if len(got.Groups) != xpNodeCap || boom.Name != "boom|500|" || boom.N != 40 || boom.ErrEvents != 70 {
			t.Fatalf("error affected/event counts = %+v", boom)
		}
		for _, group := range got.Groups {
			p50, p95 := explorerPercentilesFromOccurrences(wantTTFT[group.Name])
			checkExplorerPercentilePair(t, group.Name+" ttft", group.TTFTP50, group.TTFTP95, p50, p95)
			p50, p95 = explorerPercentilesFromOccurrences(wantTPS[group.Name])
			checkExplorerPercentilePair(t, group.Name+" tps", group.TPSP50, group.TPSP95, p50, p95)
		}
		names := explorerGroupNames(got.Groups)
		for _, excluded := range []string{"kind-23|500|", "kind-29|500|"} {
			if _, kept := names[excluded]; kept {
				t.Fatalf("group %q outside top produced a card", excluded)
			}
		}
	})
}

// TestExplorerSelectedGroupsLookupPaths pins low, boundary and high selection
// to one bucket mapping, with unselected ids always resolving to zero.
func TestExplorerSelectedGroupsLookupPaths(t *testing.T) {
	for _, base := range []uint32{0, explorerDenseSelection - 2, 1 << 20} {
		top := []*entAcc{{id: base + 1, name: "a"}, {id: base + 2, name: "b"}, {id: base + 3, name: "c"}}
		selected := newExplorerSelectedGroups(top)
		for id, want := range map[uint32]uint8{base: 0, base + 1: 1, base + 2: 2, base + 3: 3, base + 4: 0, base + 99: 0} {
			if got := selected.bucket(id); got != want {
				t.Fatalf("base %d id %d bucket = %d, want %d", base, id, got, want)
			}
		}
	}
}

// explorerCollidingHighIDs returns two distinct high ids with the same initial
// open-addressed slot, so the mixed test exercises linear probing.
func explorerCollidingHighIDs() (uint32, uint32) {
	first := make(map[int]uint32, explorerHighSelection)
	for id := uint32(explorerDenseSelection); id < explorerDenseSelection+1_000_000; id++ {
		slot := explorerHighSlot(id)
		if other, ok := first[slot]; ok && other != id {
			return other, id
		}
		first[slot] = id
	}
	return 0, 0
}

// TestExplorerSelectedGroupsMixedAndCollision pins a table holding both low and
// history-scale selected ids: low ids stay direct-indexed, colliding high ids
// probe to their exact buckets, and missing ids stay zero.
func TestExplorerSelectedGroupsMixedAndCollision(t *testing.T) {
	if xpNodeCap >= explorerHighSelection || explorerHighSelection&(explorerHighSelection-1) != 0 {
		t.Fatalf("high table slots = %d, xpNodeCap = %d: capacity must be a power of two above the cap",
			explorerHighSelection, xpNodeCap)
	}
	first, second := explorerCollidingHighIDs()
	if first == 0 || second == 0 || explorerHighSlot(first) != explorerHighSlot(second) {
		t.Fatalf("collision fixture = %d/%d, slots %d/%d", first, second,
			explorerHighSlot(first), explorerHighSlot(second))
	}
	top := []*entAcc{
		{id: 2, name: "low-a"},
		{id: first, name: "high-a"},
		{id: 255, name: "low-b"},
		{id: second, name: "high-b"},
		{id: 1 << 20, name: "high-c"},
	}
	selected := newExplorerSelectedGroups(top)
	want := []struct {
		id     uint32
		bucket uint8
	}{
		{1, 0}, {2, 1}, {255, 3}, {first, 2}, {second, 4}, {1 << 20, 5}, {257, 0}, {1 << 21, 0},
	}
	for _, probe := range want {
		if got := selected.bucket(probe.id); got != probe.bucket {
			t.Fatalf("id %d bucket = %d, want %d (high slots %d/%d)", probe.id, got, probe.bucket,
				explorerHighSlot(first), explorerHighSlot(second))
		}
	}
	if selected.dense[2] != 1 || selected.dense[255] != 3 {
		t.Fatalf("low ids left the dense table: %d/%d", selected.dense[2], selected.dense[255])
	}
}

// TestExplorerSelectedVisitorOccurrenceOrder pins multi-valued emission: every
// selected occurrence emits in source order, duplicates repeat, unselected ids
// emit nothing, and missing rows are skipped. Both selection storage paths must
// agree.
func TestExplorerSelectedVisitorOccurrenceOrder(t *testing.T) {
	for _, base := range []uint32{1, 1 << 20} {
		f := newExplorerFold("tool", "", nil)
		f.multiMember = true
		f.memberSpans = []explorerMemberSpan{{start: 0, count: 5}, {start: 5, count: 1}}
		f.members = []uint32{base + 2, base + 3, base, base + 2, base + 1, base + 3}
		f.groups = map[uint32]*entAcc{
			base:     {id: base, name: "one", n: 1},
			base + 1: {id: base + 1, name: "two", n: 1},
			base + 2: {id: base + 2, name: "three", n: 3},
		}
		top := f.rankedGroups()
		if len(top) != 3 || top[0].id != base+2 || top[1].id != base || top[2].id != base+1 {
			t.Fatalf("base %d ranking = %+v", base, top)
		}
		selected := newExplorerSelectedGroups(top)
		visit := f.selectedVisitor(&selected)
		emitted := func(row uint32) []uint8 {
			var buckets []uint8
			visit(row, func(bucket uint8) { buckets = append(buckets, bucket) })
			return buckets
		}
		if got, want := emitted(0), []uint8{1, 2, 1, 3}; !slices.Equal(got, want) {
			t.Fatalf("base %d row 0 buckets = %v, want %v", base, got, want)
		}
		if got := emitted(1); len(got) != 0 {
			t.Fatalf("base %d unselected occurrence emitted %v", base, got)
		}
		if got := emitted(2); len(got) != 0 {
			t.Fatalf("base %d out-of-range row emitted %v", base, got)
		}
	}
}

// TestExplorerSelectedVisitorSingleValued pins one group per row through the
// compact lookup, including zero and unselected ids.
func TestExplorerSelectedVisitorSingleValued(t *testing.T) {
	f := newExplorerFold("client", "", nil)
	f.singleMember = []uint32{2, 0, 1, 999}
	f.groups = map[uint32]*entAcc{
		1: {id: 1, name: "one", n: 1},
		2: {id: 2, name: "two", n: 1},
	}
	selected := newExplorerSelectedGroups(f.rankedGroups())
	visit := f.selectedVisitor(&selected)
	for row, want := range [][]uint8{{2}, nil, {1}, nil, nil} {
		var got []uint8
		visit(uint32(row), func(bucket uint8) { got = append(got, bucket) })
		if !slices.Equal(got, want) {
			t.Fatalf("row %d buckets = %v, want %v", row, got, want)
		}
	}
}

// explorerDistinctNames builds zero-padded names so lexical tie-breaking in
// the card ranking matches numeric fixture order.
func explorerDistinctNames(prefix string, count int) []string {
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%07d", prefix, i)
	}
	return names
}

func explorerDistinctDictionary(names []string) dimensionDictionary {
	d := dimensionDictionary{
		names:  make([]string, len(names)+1),
		ids:    make(map[string]uint32, len(names)),
		counts: make([]int, len(names)+1),
	}
	for i, name := range names {
		id := uint32(i + 1)
		d.names[id] = name
		d.ids[name] = id
		d.counts[id] = 1
	}
	return d
}

// explorerDistinctConversationFold mirrors a prepared projection whose
// dictionary and resolved lineage already own one entry per history value.
// The fold itself must not allocate another per-value structure.
func explorerDistinctConversationFold(names []string) *explorerFold {
	p := &projectionData{}
	p.dimensions.dict[dimConversation] = explorerDistinctDictionary(names)
	p.lineage.nodes = make([]conversationNode, len(names))
	p.lineage.groups = make([]conversationGroupNode, len(names))
	p.lineage.groupIndex = make(map[uint32]uint32, len(names))
	p.lineage.rowNodes = make([]uint32, len(names))
	for i := range names {
		id := uint32(i + 1)
		p.lineage.nodes[i] = conversationNode{identity: conversationKey{id: id}, group: uint32(i)}
		p.lineage.groups[i] = conversationGroupNode{conv: id, node: uint32(i)}
		p.lineage.groupIndex[id] = uint32(i)
		p.lineage.rowNodes[i] = uint32(i + 1)
	}
	f := newExplorerFold("conversation", "", nil)
	f.prepare(p, nil)
	f.rowCount = len(names)
	return f
}

func explorerDistinctErrorFold(names []string) *explorerFold {
	p := &projectionData{}
	p.dimensions.dict[dimError] = explorerDistinctDictionary(names)
	f := newExplorerFold("error", "", nil)
	f.prepare(p, nil)
	f.rowCount = len(names)
	return f
}

// explorerFoldAllocatedBytes reports TotalAlloc for one synchronous fold. The
// fixture is built before this call, so the bound covers the fold and payload
// work that attacker-controlled distinct values could amplify.
func explorerFoldAllocatedBytes(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestExplorerSyntheticOtherIsDistinctFromRealOther pins the overflow card's
// wire identity: an explicit synthetic marker separates it from an exact value
// named "other", so the real value keeps its own selectable card. The marker is
// omitted from normal cards.
func TestExplorerSyntheticOtherIsDistinctFromRealOther(t *testing.T) {
	const fillers = explorerGroupBudget - 1
	rows := make([]contrib, 0, fillers+100+50)
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.Local).UnixMilli()
	add := func(client string) {
		rows = append(rows, contrib{
			id: fmt.Sprintf("row-%05d", len(rows)), start: start + int64(len(rows)),
			status: 200, client: client,
		})
	}
	for i := range fillers {
		add(fmt.Sprintf("client-%05d", i))
	}
	for range 100 {
		add("other")
	}
	for i := range 50 {
		add(fmt.Sprintf("overflow-%02d", i))
	}
	p := explorerProjection(rows)
	f := newExplorerFold("client", "", nil)
	f.prepare(p, nil)
	for i := range p.rows {
		if err := f.fold(&p.rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	payload := f.payload()
	var realOther, syntheticOther *jsonEnt
	for i := range payload.Groups {
		g := &payload.Groups[i]
		if g.Name != explorerOtherName {
			continue
		}
		if g.Synthetic {
			syntheticOther = g
		} else {
			realOther = g
		}
	}
	if realOther == nil || syntheticOther == nil {
		t.Fatalf("cards = %+v, want both a real %q and the synthetic overflow card", payload.Groups, explorerOtherName)
	}
	if realOther.N != 100 || syntheticOther.N != 50 {
		t.Fatalf("real/synthetic other N = %d/%d, want 100/50", realOther.N, syntheticOther.N)
	}
	rawReal, err := json.Marshal(realOther)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReal map[string]json.RawMessage
	if err := json.Unmarshal(rawReal, &decodedReal); err != nil {
		t.Fatal(err)
	}
	if _, marked := decodedReal["synthetic"]; marked {
		t.Fatalf("real %q card carries a synthetic marker: %s", explorerOtherName, rawReal)
	}
	rawSynthetic, err := json.Marshal(syntheticOther)
	if err != nil {
		t.Fatal(err)
	}
	var decodedSynthetic map[string]json.RawMessage
	if err := json.Unmarshal(rawSynthetic, &decodedSynthetic); err != nil {
		t.Fatal(err)
	}
	if string(decodedSynthetic["synthetic"]) != "true" {
		t.Fatalf("synthetic card marker = %s, want true in %s", decodedSynthetic["synthetic"], rawSynthetic)
	}
}

// TestExplorerDistinctGroupsStayBounded pins the explorer's internal group
// budget at one million distinct values. Exact per-value accumulators stop at
// explorerGroupBudget and the remainder folds into the synthetic other card,
// so neither the error affected-ID maps nor the conversation payload map
// scales with history cardinality. The top cards and scope/rail totals remain
// exact and the completed payload is stable across calls.
func TestExplorerDistinctGroupsStayBounded(t *testing.T) {
	const distinct = 1_000_000
	const maxAlloc = 64 << 20
	names := map[string][]string{
		"conversation": explorerDistinctNames("conv", distinct),
		"error":        explorerDistinctNames("err", distinct),
	}
	rowIDs := make([]string, distinct)
	for i := range rowIDs {
		rowIDs[i] = strconv.Itoa(i)
	}
	for _, dim := range []string{"conversation", "error"} {
		t.Run(dim, func(t *testing.T) {
			values := names[dim]
			var f *explorerFold
			if dim == "conversation" {
				f = explorerDistinctConversationFold(values)
			} else {
				f = explorerDistinctErrorFold(values)
			}
			errorIDs := make([]uint32, 1)
			ent := []errEnt{{typ: "failure", code: "500"}}
			var got explorerPayload
			allocated := explorerFoldAllocatedBytes(func() {
				for i := range values {
					c := contrib{id: rowIDs[i], rowIndex: uint32(i + 1), start: int64(i), status: 200}
					if dim == "conversation" {
						c.conv = values[i]
					} else {
						errorIDs[0] = uint32(i + 1)
						c.status, c.isErr = 500, true
						c.errorIDs, c.ent = errorIDs, ent
					}
					if err := f.fold(&c); err != nil {
						t.Fatal(err)
					}
				}
				got = f.payload()
			})
			if allocated > maxAlloc {
				t.Fatalf("%s distinct fold allocated %.1f MiB, want at most %d MiB", dim,
					float64(allocated)/(1<<20), maxAlloc>>20)
			}
			if tracked := len(f.groups); tracked != explorerGroupBudget {
				t.Fatalf("%s tracked groups = %d, want the %d-group budget", dim, tracked, explorerGroupBudget)
			}
			overflow := distinct - explorerGroupBudget
			if len(got.Groups) != xpNodeCap || got.Groups[0].Name != explorerOtherName || got.Groups[0].N != int64(overflow) {
				t.Fatalf("%s overflow card = %+v (cards %d), want %q N=%d", dim, got.Groups[0], len(got.Groups), explorerOtherName, overflow)
			}
			if dim == "error" && got.Groups[0].ErrEvents != int64(overflow) {
				t.Fatalf("error overflow occurrences = %d, want %d", got.Groups[0].ErrEvents, overflow)
			}
			if got.Total != distinct || got.Scope.Matches != distinct || got.Rail[dim] != distinct {
				t.Fatalf("%s totals = total %d scope %+v rail %d, want %d", dim, got.Total, got.Scope, got.Rail[dim], distinct)
			}
			if dim == "error" && (got.ErrorTot != distinct || got.Scope.Errors != distinct || got.Groups[0].ErrFinal != int64(overflow)) {
				t.Fatalf("error health totals = scope %+v card %+v", got.Scope, got.Groups[0])
			}
			// The synthetic card leads, followed by the exact lexical top of
			// the tracked set. No untracked value can displace a tracked card.
			for i := 1; i < xpNodeCap; i++ {
				if got.Groups[i].Name != values[i-1] || got.Groups[i].N != 1 {
					t.Fatalf("%s card %d = %q N=%d, want %q N=1", dim, i, got.Groups[i].Name, got.Groups[i].N, values[i-1])
				}
				if dim == "conversation" && (got.Groups[i].Conversation == nil || got.Groups[i].Conversation.Role != conversationMain) {
					t.Fatalf("conversation card %d lost lineage: %+v", i, got.Groups[i].Conversation)
				}
			}
			if again := f.payload(); !reflect.DeepEqual(got, again) {
				t.Fatalf("%s payload differs across calls", dim)
			}
			t.Logf("%s: %d distinct values -> %d tracked, %d overflow, %.1f MiB allocated", dim,
				distinct, len(f.groups), overflow, float64(allocated)/(1<<20))
		})
	}
}

// TestExplorerGroupBudgetExceedsNodeCap keeps the bounded top-card ranking
// meaningful: an overflow bucket only ever aggregates values that could not
// have appeared as cards themselves. It also pins the budget published in
// docs/operations.md so that contract and the owner cannot drift apart.
func TestExplorerGroupBudgetExceedsNodeCap(t *testing.T) {
	if explorerGroupBudget < xpNodeCap {
		t.Fatalf("explorerGroupBudget = %d, want at least xpNodeCap %d", explorerGroupBudget, xpNodeCap)
	}
	if explorerGroupBudget != 8192 {
		t.Fatalf("explorerGroupBudget = %d, want the 8192 documented in docs/operations.md", explorerGroupBudget)
	}
}

// TestExplorerOverflowAffectedRequests pins the overflow bucket's health
// semantics: error occurrences still count individually, while N and ErrFinal
// count affected requests, including when one request contributes several
// overflow errors.
func TestExplorerOverflowAffectedRequests(t *testing.T) {
	f := newExplorerFold("error", "", nil)
	for i := range explorerGroupBudget {
		c := contrib{id: fmt.Sprintf("tracked-%d", i), status: 500, isErr: true,
			ent: []errEnt{{typ: fmt.Sprintf("tracked-%d", i), code: "500"}}}
		if err := f.fold(&c); err != nil {
			t.Fatal(err)
		}
	}
	if f.overflow || len(f.groups) != explorerGroupBudget {
		t.Fatalf("exact budget = %d groups, overflow=%v", len(f.groups), f.overflow)
	}
	first := contrib{id: "overflow-1", status: 500, isErr: true,
		ent: []errEnt{{typ: "extra-a", code: "500"}, {typ: "extra-b", code: "500"}, {typ: "extra-a", code: "500"}}}
	second := contrib{id: "overflow-2", status: 500, isErr: true,
		ent: []errEnt{{typ: "extra-a", code: "500"}}}
	for _, c := range []*contrib{&first, &second} {
		if err := f.fold(c); err != nil {
			t.Fatal(err)
		}
	}
	if !f.overflow || len(f.groups) != explorerGroupBudget {
		t.Fatalf("overflow = %v with %d tracked groups", f.overflow, len(f.groups))
	}
	if f.other.n != 4 || f.otherAffected != 2 || f.other.errFin != 2 || f.other.code != "" {
		t.Fatalf("overflow accumulator = %+v (affected %d)", f.other, f.otherAffected)
	}
	card := f.payload().Groups[0]
	if card.Name != explorerOtherName || card.N != 2 || card.ErrEvents != 4 || card.ErrFinal != 2 || card.Code != "" {
		t.Fatalf("overflow card = %+v", card)
	}
}

func BenchmarkExplorerEncodedFold(b *testing.B) {
	rows := explorerFoldFixtures()
	rows = slices.Repeat(rows, 724) // approximately 100k neutral contributions
	p := explorerProjection(rows)
	for _, encoded := range []bool{false, true} {
		b.Run(fmt.Sprint("encoded=", encoded), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				f := newExplorerFold("provider", "", nil)
				input := rows
				if encoded {
					f.prepare(p, nil)
					input = p.rows
				}
				for i := range input {
					_ = f.fold(&input[i])
				}
				_ = f.payload()
			}
		})
	}
}

// explorerPayloadBenchFold builds one payload-ready fold for a synthetic
// history: every row carries a valid ttft/tps sample and one selected-group
// membership. baseID shifts the selected dictionary ids without changing row
// or occurrence counts, so payload allocations must not follow it.
func explorerPayloadBenchFold(history int, multi bool, baseID uint32) *explorerFold {
	f := newExplorerFold("client", "", nil)
	f.rowCount = history
	if multi {
		f.dim, f.dimIndex = "tool", dimTool
	}
	f.multiMember = multi
	f.groups = make(map[uint32]*entAcc, xpNodeCap)
	rows := make([]int, xpNodeCap)
	f.order.base.ttft = make([]metricSample[int64], history)
	f.order.base.tps = make([]metricSample[float64], history)
	if multi {
		f.memberSpans = make([]explorerMemberSpan, history)
		f.members = make([]uint32, history)
	} else {
		f.singleMember = make([]uint32, history)
	}
	for row := range history {
		group := row % xpNodeCap
		rows[group]++
		id := baseID + uint32(group)
		f.order.base.ttft[row] = metricSample[int64]{row: uint32(row), value: int64(row + 1)}
		f.order.base.tps[row] = metricSample[float64]{row: uint32(row), value: float64(row + 1)}
		if multi {
			f.memberSpans[row] = explorerMemberSpan{start: uint32(row), count: 1}
			f.members[row] = id
		} else {
			f.singleMember[row] = id
		}
	}
	for group := range xpNodeCap {
		id := baseID + uint32(group)
		f.groups[id] = &entAcc{id: id, name: fmt.Sprintf("group-%02d", group),
			n: rows[group], ttftN: rows[group], tpsN: rows[group]}
	}
	return f
}

// BenchmarkExplorerPayloadSelectedLookup isolates the payload's selected-group
// lookup at a million-row history. Selected ids move from the low cards to an
// 8M-high sparse range, and the multi-valued cases retain 100k to 1M tool
// occurrences; B/op and allocs/op must stay bounded by xpNodeCap instead of
// maxGroupID or occurrence count.
func BenchmarkExplorerPayloadSelectedLookup(b *testing.B) {
	cases := []struct {
		name    string
		history int
		multi   bool
		baseID  uint32
	}{
		{"single/id=24", 1_000_000, false, 1},
		{"single/id=8m", 1_000_000, false, 8_000_000},
		{"multi/occurrences=100k", 100_000, true, 1},
		{"multi/occurrences=1m", 1_000_000, true, 1},
		{"multi/occurrences=1m-id=8m", 1_000_000, true, 8_000_000},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			f := explorerPayloadBenchFold(tc.history, tc.multi, tc.baseID)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = f.payload()
			}
		})
	}
}
