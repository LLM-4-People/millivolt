package web

import (
	"fmt"
	"math"
	"reflect"
	"slices"
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
		if p.Total != 2 || p.ErrorTot != 1 || p.Rail[dim] != 1 || len(p.Groups) != 1 {
			t.Fatalf("%s scope/rail = %+v", dim, p)
		}
		g := p.Groups[0]
		wantN, wantSpark := int64(3), 3
		if dim == "error" {
			wantN, wantSpark = 2, 2
			if g.ErrEvents != 3 || g.LastMs != 2100 || g.Code != "500" {
				t.Fatalf("error occurrence metadata = %+v", g)
			}
		}
		var sparkN int
		for _, n := range g.Spark {
			sparkN += n
		}
		if g.N != wantN || sparkN != wantSpark || g.Cost != .2 || g.ErrFinal != 1 {
			t.Fatalf("%s occurrence aggregation = %+v, spark requests = %d", dim, g, sparkN)
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

func TestExplorerFoldSampleReservationIsBounded(t *testing.T) {
	rows := make([]contrib, 1000)
	for i := range rows {
		rows[i] = contrib{id: fmt.Sprint(i), prov: "old.example", status: 200, ttft: 10, tps: 1}
		if i == len(rows)-1 {
			rows[i].prov = "new.example"
		}
	}
	p := explorerProjection(rows)
	for _, filtered := range []bool{false, true} {
		var fs []scopeFilter
		if filtered {
			fs = []scopeFilter{{dim: "client", id: ""}}
		}
		f := newExplorerFold("provider", "", fs)
		f.prepare(p, nil)
		_ = f.fold(&p.rows[0])
		_ = f.fold(&p.rows[len(p.rows)-1])
		for i := range 10 {
			c := contrib{prov: fmt.Sprintf("ring-%d.example", i), ttft: 10, tps: 1}
			_ = f.fold(&c)
		}
		var reserved int
		for _, g := range f.groups {
			want := 0
			if !filtered && g.name == "old.example" {
				want = len(p.rows) - 1
			} else if !filtered && g.name == "new.example" {
				want = 1
			}
			if g.startCap != want {
				t.Fatalf("filtered=%v group=%q reserved %d starts, want %d", filtered, g.name, g.startCap, want)
			}
			reserved += g.startCap
		}
		if reserved > len(p.rows) {
			t.Fatalf("reserved %d samples for %d projected memberships", reserved, len(p.rows))
		}
	}
}

func TestExplorerFoldReservationsFollowProjectedMemberships(t *testing.T) {
	rows := explorerFoldFixtures()
	p := explorerProjection(rows)
	mcz := &modelCanonizer{exec: config.CompileModelRules([]config.ModelRule{{Mode: config.ModelRuleLower}}), memo: make(map[string]string)}
	wants := map[string]map[string]int{
		"model": {"model-a": 137},
		"tool":  {"lookup": 138, "read": 69}, // duplicate occurrences count
		"error": {"upstream|500|failure": 56, "timeout|408|": 28},
	}
	for dim, counts := range wants {
		f := newExplorerFold(dim, "", nil)
		f.prepare(p, mcz)
		for i := range p.rows {
			_ = f.fold(&p.rows[i])
		}
		var reserved, memberships int
		for _, count := range counts {
			memberships += count
		}
		for _, g := range f.groups {
			want := counts[g.name]
			if g.startCap != want || cap(g.starts) != want || cap(g.startEr) != want ||
				g.ttftN != want || g.tpsN != want || len(g.ttftS) != 0 || len(g.tpsS) != 0 {
				t.Fatalf("%s/%s start reservation=%d, metric counts=%d/%d, copied samples=%d/%d, want membership=%d",
					dim, g.name, g.startCap, g.ttftN, g.tpsN, len(g.ttftS), len(g.tpsS), want)
			}
			reserved += g.startCap
		}
		if reserved != memberships {
			t.Fatalf("%s reserved %d samples for %d projected memberships", dim, reserved, memberships)
		}
		if len(f.members) != memberships {
			t.Fatalf("%s retained %d CSR occurrences, want %d", dim, len(f.members), memberships)
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
		if dim != "provider" {
			wantP50, wantP95 = 20, 86
			if dim == "tool" {
				wantN = 5
			} else if g.ErrEvents != 5 {
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
