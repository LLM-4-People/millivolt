package web

import (
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func sameDimRows(t *testing.T) *storage.Store {
	t.Helper()
	s := testStoreWithQueryTimeout(t, 5*time.Second)
	start := time.UnixMilli(1_800_000_000_000)
	rows := []*metrics.Record{
		{ID: "tool-a", Provider: "p1", Client: "c1", Model: "model-a", KeyHash: "k1", StatusCode: 200,
			Start: start, ToolNames: []string{"tool_a"}, ToolCalls: 1},
		{ID: "tool-b", Provider: "p1", Client: "c1", Model: "model-b", KeyHash: "k1", StatusCode: 200,
			Start: start.Add(time.Second), ToolNames: []string{"tool_b"}, ToolCalls: 1},
		{ID: "tool-ab", Provider: "p1", Client: "c1", Model: "model-b", KeyHash: "k1", StatusCode: 200,
			Start: start.Add(2 * time.Second), ToolNames: []string{"tool_a", "tool_b"}, ToolCalls: 2},
		{ID: "none", Provider: "p2", Client: "c1", Model: "model-a", KeyHash: "k1", StatusCode: 200,
			Start: start.Add(3 * time.Second)},
	}
	for _, r := range rows {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	waitTotals(t, s, int64(len(rows)))
	return s
}

func chartRequests(t *testing.T, s *storage.Store, query string) int64 {
	t.Helper()
	p := get(t, http.HandlerFunc(NewAggAPI(metrics.NewBuffer(8), s, 5*time.Second).HandleAggChart), query)
	var req int64
	for _, raw := range p["buckets"].([]any) {
		req += int64(raw.(map[string]any)["req"].(float64))
	}
	return req
}

func logIDs(t *testing.T, s *storage.Store, query string) []string {
	t.Helper()
	p := get(t, http.HandlerFunc(NewAggAPI(metrics.NewBuffer(8), s, 5*time.Second).HandleLogPage), query)
	var ids []string
	for _, raw := range p["records"].([]any) {
		ids = append(ids, raw.(map[string]any)["id"].(string))
	}
	return ids
}

func explorerMatches(t *testing.T, s *storage.Store, query string) int64 {
	t.Helper()
	p := get(t, http.HandlerFunc(NewAggAPI(metrics.NewBuffer(8), s, 5*time.Second).HandleAggExplorer), query)
	return int64(p["scope"].(map[string]any)["matches"].(float64))
}

// TestScopeSameDimensionIsOrEverywhere pins WEB-1 in chart, explorer and log:
// repeated filters for one dimension are alternatives, and cross-dimension
// filters still intersect.
func TestScopeSameDimensionIsOrEverywhere(t *testing.T) {
	s := sameDimRows(t)
	if got := chartRequests(t, s, "/metrics/agg/chart?window=all&f=tool:tool_a&f=tool:tool_b"); got != 3 {
		t.Fatalf("chart tool A OR B requests = %d, want 3", got)
	}
	if got := explorerMatches(t, s, "/metrics/agg/explorer?dim=client&f=tool:tool_a&f=tool:tool_b"); got != 3 {
		t.Fatalf("explorer tool A OR B matches = %d, want 3", got)
	}
	if ids := logIDs(t, s, "/metrics/agg/log?limit=10&f=tool:tool_a&f=tool:tool_b"); len(ids) != 3 {
		t.Fatalf("log tool A OR B ids = %v, want 3", ids)
	}
	if got := explorerMatches(t, s, "/metrics/agg/explorer?dim=client&f=model:model-a&f=model:model-b"); got != 4 {
		t.Fatalf("explorer model A OR B matches = %d, want 4", got)
	}
	if got := chartRequests(t, s, "/metrics/agg/chart?window=all&f=model:model-a&f=model:model-b"); got != 4 {
		t.Fatalf("chart model A OR B requests = %d, want 4", got)
	}

	// A pushable same-dimension group becomes one SQL OR group, not an AND.
	plan := planLogPage([]scopeFilter{
		{dim: "provider", id: "p1"}, {dim: "provider", id: "p2"},
	}, "")
	if !plan.allPushed || len(plan.filter.All) != 0 || len(plan.filter.Any) != 1 ||
		len(plan.filter.Any[0]) != 2 || len(plan.goFilters) != 0 {
		t.Fatalf("provider OR plan = %+v", plan)
	}
	if ids := logIDs(t, s, "/metrics/agg/log?limit=10&f=provider:p1&f=provider:p2"); len(ids) != 4 {
		t.Fatalf("log provider OR ids = %v, want 4", ids)
	}

	// Controls: a single filter and a cross-dimension intersection stay exact.
	if got := explorerMatches(t, s, "/metrics/agg/explorer?dim=client&f=tool:tool_a"); got != 2 {
		t.Fatalf("explorer tool_a matches = %d, want 2", got)
	}
	if got := explorerMatches(t, s, "/metrics/agg/explorer?dim=client&f=provider:p1&f=tool:tool_a"); got != 2 {
		t.Fatalf("explorer provider AND tool matches = %d, want 2", got)
	}
	if ids := logIDs(t, s, "/metrics/agg/log?limit=10&f=provider:p2"); len(ids) != 1 || ids[0] != "none" {
		t.Fatalf("log provider control ids = %v", ids)
	}
}

// TestLogPageGroupsAlternativesByDimension pins the two-dimensional SQL shape:
// provider alternatives and client alternatives are separate AND-ed groups, so
// a row matching only one group is never admitted by an all-pushed page.
func TestLogPageGroupsAlternativesByDimension(t *testing.T) {
	s := testStoreWithQueryTimeout(t, 5*time.Second)
	start := time.UnixMilli(1_800_100_000_000)
	rows := []*metrics.Record{
		{ID: "both-a", Provider: "p1", Client: "c1", Model: "m", KeyHash: "k", StatusCode: 200, Start: start},
		{ID: "both-b", Provider: "p2", Client: "c2", Model: "m", KeyHash: "k", StatusCode: 200, Start: start.Add(time.Second)},
		{ID: "provider-only", Provider: "p1", Client: "c3", Model: "m", KeyHash: "k", StatusCode: 200, Start: start.Add(2 * time.Second)},
		{ID: "client-only", Provider: "p3", Client: "c1", Model: "m", KeyHash: "k", StatusCode: 200, Start: start.Add(3 * time.Second)},
		{ID: "neither", Provider: "p3", Client: "c3", Model: "m", KeyHash: "k", StatusCode: 200, Start: start.Add(4 * time.Second)},
	}
	for _, row := range rows {
		s.Record(row)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	waitTotals(t, s, int64(len(rows)))

	plan := planLogPage([]scopeFilter{
		{dim: "provider", id: "p1"}, {dim: "provider", id: "p2"},
		{dim: "client", id: "c1"}, {dim: "client", id: "c2"},
	}, "")
	if !plan.allPushed || len(plan.filter.All) != 0 || len(plan.filter.Any) != 2 ||
		len(plan.filter.Any[0]) != 2 || len(plan.filter.Any[1]) != 2 || len(plan.goFilters) != 0 {
		t.Fatalf("two-dimension OR plan = %+v", plan)
	}

	ids := logIDs(t, s, "/metrics/agg/log?limit=10&f=provider:p1&f=provider:p2&f=client:c1&f=client:c2")
	got := make(map[string]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got["both-a"] || !got["both-b"] {
		t.Fatalf("two-dimension log ids = %v, want both-a and both-b only", ids)
	}

	// Control: one predicate in each dimension still intersects exactly.
	ids = logIDs(t, s, "/metrics/agg/log?limit=10&f=provider:p1&f=client:c1")
	if len(ids) != 1 || ids[0] != "both-a" {
		t.Fatalf("single-filter control ids = %v, want both-a", ids)
	}
}

// TestStatusSpellingIsCanonicalEverywhere pins MCP-9/WEB-2: non-canonical
// numeric spellings match the same records as the canonical one in chart,
// explorer and log.
func TestStatusSpellingIsCanonicalEverywhere(t *testing.T) {
	s := testStoreWithQueryTimeout(t, 5*time.Second)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range 3 {
		s.Record(&metrics.Record{
			ID: fmt.Sprintf("r%d", i), Provider: "p", Client: "c", Model: "m", KeyHash: "k",
			StatusCode: 200, Start: start.Add(time.Duration(i) * time.Second),
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	waitTotals(t, s, 3)
	for _, spelling := range []string{"200", "0200", "%2B200"} {
		if got := chartRequests(t, s, "/metrics/agg/chart?window=all&s="+spelling); got != 3 {
			t.Fatalf("chart s=%s requests = %d, want 3", spelling, got)
		}
		if got := explorerMatches(t, s, "/metrics/agg/explorer?dim=client&s="+spelling); got != 3 {
			t.Fatalf("explorer s=%s matches = %d, want 3", spelling, got)
		}
		if ids := logIDs(t, s, "/metrics/agg/log?limit=10&s="+spelling); len(ids) != 3 {
			t.Fatalf("log s=%s ids = %v, want 3", spelling, ids)
		}
	}
}

// TestLineageScratchIsReused checks the pooling lifecycle directly: a dirty
// solve borrows a full-projection workspace, release returns it to the pool and
// clears every borrowed slice, and a later view can solve again. Actual reuse is
// pinned by the allocation bound in this file, since sync.Pool may drop entries.
func TestLineageScratchIsReused(t *testing.T) {
	base := &conversationTopology{nodes: make([]conversationNode, 4096)}
	for i := range base.nodes {
		base.nodes[i].identity = conversationKey{id: uint32(i + 1)}
	}
	l := &conversationLineage{base: base, names: &queryDimensions{}}
	l.addExtra(conversationKey{id: 5000}, 5000, false, false)
	l.solve()
	if l.issueAt(l.baseNodes()) == conversationIssueNone {
		t.Fatal("dirty overlay did not resolve its new node issue")
	}
	first := l.scratch
	if first == nil || cap(first.issues) < int(l.baseNodes())+len(l.extras) {
		t.Fatalf("scratch = %+v", first)
	}
	l.releaseScratch()
	if l.scratch != nil || l.issues != nil || l.state != nil || l.path != nil {
		t.Fatal("release retained the full-projection workspace")
	}
	// sync.Pool may drop entries at any time (notably under race-triggered
	// GC), so reuse is pinned by the allocation bound, not object identity.
	// Here only the lifecycle matters: a later solve still works from the pool
	// and leaves no workspace behind on its own.
	reuse := &conversationLineage{base: base, names: &queryDimensions{}}
	reuse.addExtra(conversationKey{id: 5001}, 5001, false, false)
	reuse.solve()
	if reuse.issueAt(reuse.baseNodes()) == conversationIssueNone {
		t.Fatal("second dirty solve did not resolve its new node issue")
	}
	reuse.releaseScratch()
	if reuse.scratch != nil || reuse.issues != nil {
		t.Fatal("second release retained the full-projection workspace")
	}
}

// legacyScopedMatch is the pre-mask implementation: for each dimension it
// rescans the remaining filter slice. The benchmark compares it with the
// single-pass production matcher on the same contributions.
func legacyScopedMatch(c *contrib, statusCode string, fs []scopeFilter) bool {
	if statusCode != "" {
		if liveStatusFilter(statusCode) {
			return false
		}
		if strconv.Itoa(c.status) != statusCode {
			return false
		}
	}
	var done uint32
	for i := range fs {
		bit := fs[i].bit
		if bit == 0 {
			bit = scopeDimBit(fs[i].dim)
		}
		if done&bit != 0 {
			continue
		}
		done |= bit
		matched := false
		for j := i; j < len(fs); j++ {
			if fs[j].dim != fs[i].dim {
				continue
			}
			if fs[j].matches(c) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

var scopedMatchSink bool

func BenchmarkScopedMatch(b *testing.B) {
	c := &contrib{status: 200}
	labels := rowLabels(c)
	labels.prov, labels.client, labels.model, labels.conv, labels.key = "p", "c", "m", "conv", "k"
	many := make([]scopeFilter, 0, 40)
	for _, dim := range []string{"provider", "client", "model", "conversation", "key"} {
		hit := map[string]string{"provider": "p", "client": "c", "model": "m", "conversation": "conv", "key": "k"}[dim]
		for i := 0; i < 8; i++ {
			id := "miss"
			if i == 7 {
				id = hit
			}
			many = append(many, scopeFilter{dim: dim, id: id})
		}
	}
	cases := []struct {
		name string
		fs   []scopeFilter
	}{
		{"one", []scopeFilter{{dim: "provider", id: "p"}}},
		{"five", []scopeFilter{
			{dim: "provider", id: "p"}, {dim: "client", id: "c"}, {dim: "model", id: "m"},
			{dim: "conversation", id: "conv"}, {dim: "key", id: "k"},
		}},
		{"same-dim", []scopeFilter{
			{dim: "provider", id: "p1"}, {dim: "provider", id: "p2"}, {dim: "provider", id: "p"},
		}},
		{"many", many},
	}
	for _, tc := range cases {
		b.Run(tc.name+"/old", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				scopedMatchSink = legacyScopedMatch(c, "", tc.fs)
			}
		})
		b.Run(tc.name+"/new", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				scopedMatchSink = scopedMatch(c, "", tc.fs)
			}
		})
	}
	_ = scopedMatchSink
}

// TestLineageDirtyExplorerAllocationIsPooled pins WEB-3: a dirty overlay no
// longer allocates full-projection arrays per explorer request. The bound is a
// generous ceiling over the clean/dirty delta at this node count. Race
// instrumentation plus sync.Pool clearing has measured up to ~420 KiB here,
// while the old full-array implementation is ~721 KiB; 512 KiB still catches
// that regression without skipping this test under -race.
func TestLineageDirtyExplorerAllocationIsPooled(t *testing.T) {
	const n = 100_000
	rows := make([]contrib, n)
	for i := range rows {
		rows[i] = lineageRow(fmt.Sprintf("s:%d", i), "")
		rowLabels(&rows[i]).prov = "fixture.example"
	}
	p := explorerProjection(rows)
	clean := lineageRow("s:clean", "")
	rowLabels(&clean).prov = "fixture.example"
	dirty := lineageRow("s:dirty", "s:0")
	rowLabels(&dirty).prov = "fixture.example"

	// A manual warm run and a bounded, GC-disabled sample avoid the
	// testing.Benchmark calibration floor and keep sync.Pool reuse stable even
	// when the gate runs with a short -benchtime under concurrent load.
	measure := func(extra *contrib) (bytesPerOp, allocsPerOp uint64) {
		fold := func() {
			f := newExplorerFold("provider", "", nil)
			f.prepare(p, nil)
			for i := range p.rows {
				if err := f.fold(&p.rows[i]); err != nil {
					t.Fatal(err)
				}
			}
			if extra != nil {
				if err := f.fold(extra); err != nil {
					t.Fatal(err)
				}
			}
			lineagePayloadSink = f.payload()
		}
		fold()
		runtime.GC()
		previous := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(previous)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		const runs = 4
		for range runs {
			fold()
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs,
			(after.Mallocs - before.Mallocs) / runs
	}
	cleanBytes, _ := measure(&clean)
	dirtyBytes, _ := measure(&dirty)
	delta := int64(dirtyBytes) - int64(cleanBytes)
	t.Logf("dirty lineage adds %d B/op at %d nodes (clean=%d dirty=%d)",
		delta, n, cleanBytes, dirtyBytes)
	if delta > 512<<10 {
		t.Fatalf("dirty lineage adds %d B/op at %d nodes (clean=%d dirty=%d), want a pooled bound",
			delta, n, cleanBytes, dirtyBytes)
	}
}

var lineagePayloadSink any
