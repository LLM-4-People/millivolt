package web

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func lineageRow(id, parent string) contrib {
	return contrib{conv: id, parentConv: parent, client: "fixture-client", key: "fixture-key", status: 200}
}

func TestConversationLineageResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []contrib
		want conversationSummary
	}{
		{"parent child grandchild", []contrib{lineageRow("s:parent", ""), lineageRow("s:child", "s:parent"), lineageRow("s:grandchild", "s:child")}, conversationSummary{Main: 1, Sub: 2}},
		{"missing parent", []contrib{lineageRow("s:child", "s:missing")}, conversationSummary{Sub: 1}},
		{"omission is not conflict", []contrib{lineageRow("s:child", ""), lineageRow("s:child", "s:parent"), lineageRow("s:child", "s:parent")}, conversationSummary{Sub: 1}},
		{"conflicting parents", []contrib{lineageRow("s:child", "s:first"), lineageRow("s:child", "s:second")}, conversationSummary{Unresolved: 1}},
		{"self parent", []contrib{lineageRow("s:child", "s:child")}, conversationSummary{Unresolved: 1}},
		{"cycle and dependent", []contrib{lineageRow("s:first", "s:second"), lineageRow("s:second", "s:first"), lineageRow("s:dependent", "s:first")}, conversationSummary{Unresolved: 3}},
		{"conflicting ancestor", []contrib{lineageRow("s:parent", "s:first"), lineageRow("s:parent", "s:second"), lineageRow("s:child", "s:parent")}, conversationSummary{Unresolved: 2}},
		// k: ids (the sub_conversations tracked identity) ride the same
		// ConversationID column with zero web branching: a leaf renders role
		// main, and only a declared parent makes one a sub. The proxy never
		// attaches a parent to a k: identity (the s: header wins identity when
		// a parent is declared), so the paired row is a synthetic pin of the
		// namespace-agnostic lineage rules, not a proxy-produced shape.
		{"k identity leaf is a main conversation", []contrib{lineageRow("k:task-1", "")}, conversationSummary{Main: 1}},
		{"k identity is a sub only under a declared parent", []contrib{lineageRow("k:root", ""), lineageRow("k:task-1", "k:root")}, conversationSummary{Main: 1, Sub: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var baseline map[string]*conversationInfo
			for seed := range int64(12) {
				var l conversationLineage
				for _, i := range rand.New(rand.NewSource(seed)).Perm(len(tc.rows)) {
					l.observe(&tc.rows[i], true, true)
				}
				got, groups := l.resolve()
				if got != tc.want {
					t.Fatalf("seed=%d summary=%+v want=%+v", seed, got, tc.want)
				}
				infos := make(map[string]*conversationInfo)
				for id, g := range groups {
					infos[id] = l.info(g)
					if infos[id].Role == conversationUnknown && (infos[id].ParentID != "" || len(infos[id].ParentScope) != 0) {
						t.Fatal("invalid lineage selected a parent")
					}
				}
				if seed == 0 {
					baseline = infos
				} else if !reflect.DeepEqual(infos, baseline) {
					t.Fatalf("order-dependent lineage: got=%+v want=%+v", infos, baseline)
				}
			}
		})
	}
}

func TestConversationLineageGroupLookupMatchesResolvedGroups(t *testing.T) {
	rows := []contrib{
		lineageRow("s:parent", ""),
		lineageRow("s:child", "s:parent"),
		lineageRow("s:orphan", "s:missing"),
	}
	for seed := range int64(8) {
		var l conversationLineage
		for _, i := range rand.New(rand.NewSource(seed)).Perm(len(rows)) {
			l.observe(&rows[i], true, true)
		}
		_, groups := l.resolve()
		for name, want := range groups {
			got := l.group(name)
			if got.lineage != &l || got.index != want.index {
				t.Fatalf("seed=%d name=%q lookup=%+v want index %d", seed, name, got, want.index)
			}
			if !reflect.DeepEqual(l.info(got), l.info(want)) {
				t.Fatalf("seed=%d name=%q metadata differs", seed, name)
			}
		}
		if got := l.group("s:missing"); got.lineage != nil {
			t.Fatalf("seed=%d missing name resolved to %+v", seed, got)
		}
	}
}

func TestConversationLineageConversationIDMatchesDictionary(t *testing.T) {
	var base dimensionDictionary
	base.intern("base-a")
	base.intern("base-b")
	q := queryDimensions{}
	q.base[dimConversation] = base
	for _, name := range []string{"base-a", "base-b", "extra-a"} {
		id := q.intern(dimConversation, name)
		l := conversationLineage{names: &q}
		if got := l.conversationID(q.name(dimConversation, id)); got != id {
			t.Fatalf("name %q id %d round-tripped as %d", name, id, got)
		}
	}
	l := conversationLineage{names: &q}
	if got := l.conversationID("missing"); got != 0 {
		t.Fatalf("missing name resolved to id %d", got)
	}
}

func TestConversationLineageNamespaceAndPivots(t *testing.T) {
	for _, missingKind := range []string{"different key", "different client", "same scope", "keyless"} {
		t.Run(missingKind, func(t *testing.T) {
			parent, child := lineageRow("s:parent", ""), lineageRow("s:child", "s:parent")
			switch missingKind {
			case "different key":
				parent.key = "another-key"
			case "different client":
				parent.client = "another-client"
			case "keyless":
				parent.key, child.key = "", ""
			}
			var l conversationLineage
			l.observe(&parent, false, false)
			l.observe(&child, true, true)
			summary, groups := l.resolve()
			info := l.info(groups[child.conv])
			wantObserved := missingKind == "same scope" || missingKind == "keyless"
			if summary != (conversationSummary{Sub: 1}) || info.Role != conversationSub || info.ParentID != parent.conv || info.ParentObserved == nil || *info.ParentObserved != wantObserved || info.ParentInScope == nil || *info.ParentInScope {
				t.Fatalf("summary=%+v info=%+v", summary, info)
			}
			if missingKind == "keyless" {
				if len(info.ParentScope) != 0 {
					t.Fatal("empty key pivot broadened namespace")
				}
			} else {
				want := []conversationPivotFilter{{Dim: "client", ID: child.client}, {Dim: "key", ID: child.key}, {Dim: "conversation", ID: parent.conv}}
				if !reflect.DeepEqual(info.ParentScope, want) {
					t.Fatalf("parent scope=%+v want=%+v", info.ParentScope, want)
				}
			}
		})
	}
	var l conversationLineage
	a, b := lineageRow("s:same-label", "s:first"), lineageRow("s:same-label", "s:second")
	b.key = "another-key"
	l.observe(&a, true, true)
	l.observe(&b, true, true)
	summary, groups := l.resolve()
	info := l.info(groups[a.conv])
	if summary != (conversationSummary{Unresolved: 1}) || info.Role != conversationUnknown || info.Issue != "multiple_scopes" || info.ParentID != "" || len(info.ParentScope) != 0 {
		t.Fatalf("ambiguous public ID selected a namespace: %+v %+v", summary, info)
	}
}

func TestConversationLineageLongChainIsIterative(t *testing.T) {
	var l conversationLineage
	const count = 20000
	for i := range count {
		parent := ""
		if i > 0 {
			parent = fmt.Sprint(i - 1)
		}
		r := lineageRow(fmt.Sprint(i), parent)
		l.observe(&r, true, true)
	}
	got, _ := l.resolve()
	if got != (conversationSummary{Main: 1, Sub: count - 1}) {
		t.Fatalf("summary=%+v", got)
	}
}

func TestConversationLineageStoredRingPendingAndExactScope(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(16)
	now := time.Now()
	makeRecord := func(id, parent string) *metrics.Record {
		r := mkRec(id, now, 200, "", nil, 10, 20, 1, 1, 0, 0)
		r.ConversationID, r.ParentConversationID = "s:"+id, parent
		return r
	}
	parent, child := makeRecord("parent", ""), makeRecord("child", "s:parent")
	grandchild := makeRecord("grandchild", "s:child")
	api := NewAggAPI(buf, s, time.Second)
	// Child arrives before its parent; both persisted rows also remain in the
	// ring, and a stale pending snapshot of the child carries a false parent.
	s.Record(child)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	p, err := api.explorer(t.Context(), "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Conversations != (conversationSummary{Sub: 1}) || p.Rail["conversation"] != 1 || len(p.Groups) != 1 || *p.Groups[0].Conversation.ParentObserved {
		t.Fatalf("parent reference fabricated an observed root: %+v", p)
	}
	s.Record(parent)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	stale := *child
	stale.ParentConversationID = "s:obsolete"
	buf.PublishLive("begin", &stale)
	buf.Record(child)
	buf.Record(parent)
	buf.PublishLive("begin", grandchild)
	p, err = api.explorer(t.Context(), "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 3 || p.Conversations != (conversationSummary{Main: 1, Sub: 2}) || p.Rail["conversation"] != 3 {
		t.Fatalf("deduped payload=%+v", p)
	}
	for _, g := range p.Groups {
		if g.Name == "s:grandchild" && (g.Conversation.ParentID != "s:child" || !*g.Conversation.ParentObserved || !*g.Conversation.ParentInScope) {
			t.Fatalf("direct parent replaced by root: %+v", g.Conversation)
		}
	}
	// A leaf filter stays exact; selecting the parent must not silently fold
	// its descendants into chart/log/footer scope. Gallery remains cross-filtered.
	p, err = api.explorer(t.Context(), "conversation", []scopeFilter{{dim: "conversation", id: "s:parent"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Matches != 1 || p.Total != 3 || p.Conversations != (conversationSummary{Main: 1}) {
		t.Fatalf("parent filter became subtree: %+v", p)
	}
	for _, g := range p.Groups {
		if g.Name == "s:grandchild" && *g.Conversation.ParentInScope {
			t.Fatal("parent outside exact scope reported inside")
		}
	}
	// Destructive projection invalidation must also remove old parent evidence.
	if _, err := s.Clear(t.Context(), &storage.PurgeFilter{ConversationID: "s:parent"}, nil); err != nil {
		t.Fatal(err)
	}
	p, err = api.explorer(t.Context(), "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Conversations != (conversationSummary{Sub: 2}) || p.Rail["conversation"] != 2 {
		t.Fatalf("deleted root was resurrected: %+v", p.Conversations)
	}
	for _, g := range p.Groups {
		if g.Name == "s:child" && *g.Conversation.ParentObserved {
			t.Fatal("deleted parent still observed")
		}
	}
}

func TestConversationLineageCollectsAncestorsOutsideScope(t *testing.T) {
	fold := newExplorerFold("conversation", "", []scopeFilter{{dim: "provider", id: "child.example"}})
	parent, child := lineageRow("s:parent", ""), lineageRow("s:child", "s:parent")
	parent.prov, child.prov = "parent.example", "child.example"
	if err := fold.fold(&parent); err != nil {
		t.Fatal(err)
	}
	if err := fold.fold(&child); err != nil {
		t.Fatal(err)
	}
	p := fold.payload()
	if p.Conversations != (conversationSummary{Sub: 1}) || len(p.Groups) != 1 {
		t.Fatalf("scope summary=%+v groups=%+v", p.Conversations, p.Groups)
	}
	info := p.Groups[0].Conversation
	if !*info.ParentObserved || *info.ParentInScope {
		t.Fatalf("outside-view parent lost: %+v", info)
	}
}

// lineageTopologyFixtures cover mixed namespaces, conflicts, self-edges,
// cycles, unresolved parents and ordinary chains. The projection-built view
// must answer exactly like the standalone declaration walk.
func lineageTopologyFixtures() []contrib {
	rows := []contrib{
		lineageRow("s:root", ""),
		lineageRow("s:child", "s:root"),
		lineageRow("s:missing", "s:absent"),
		lineageRow("s:conflicted", "s:first"),
		lineageRow("s:conflicted", "s:second"),
		lineageRow("s:cycle-a", "s:cycle-b"),
		lineageRow("s:cycle-b", "s:cycle-a"),
		lineageRow("s:self", "s:self"),
		lineageRow("s:mixed", ""),
	}
	mixed := lineageRow("s:mixed", "")
	mixed.key = "other-key"
	return append(rows, mixed)
}

func TestExplorerLineageProjectionParity(t *testing.T) {
	rows := lineageTopologyFixtures()
	for i := range rows {
		if i%2 == 0 {
			rows[i].prov = "fixture.example"
		} else {
			rows[i].prov = "other.example"
		}
	}
	projected := explorerProjection(rows)
	reversed := make([]contrib, len(rows))
	for i := range rows {
		reversed[i] = rows[len(rows)-1-i]
	}
	shuffled := explorerProjection(reversed)
	scopes := [][]scopeFilter{
		nil,
		{{dim: "provider", id: "fixture.example"}},
		{{dim: "status", id: "2xx"}},
		{{dim: "conversation", id: "s:root"}},
	}
	for _, dim := range []string{"conversation", "provider", "status"} {
		for _, fs := range scopes {
			encoded := newExplorerFold(dim, "", fs)
			encoded.prepare(projected, nil)
			raw := newExplorerFold(dim, "", fs)
			for i := range projected.rows {
				if err := encoded.fold(&projected.rows[i]); err != nil {
					t.Fatal(err)
				}
				c := rows[i]
				if err := raw.fold(&c); err != nil {
					t.Fatal(err)
				}
			}
			got, want := encoded.payload(), raw.payload()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %v cached lineage differs from raw declarations: got=%+v want=%+v", dim, fs, got, want)
			}
			other := newExplorerFold(dim, "", fs)
			other.prepare(shuffled, nil)
			for i := range shuffled.rows {
				if err := other.fold(&shuffled.rows[i]); err != nil {
					t.Fatal(err)
				}
			}
			if reversed := other.payload(); !reflect.DeepEqual(got, reversed) {
				t.Fatalf("%s %v projection order changed the payload: got=%+v reversed=%+v", dim, fs, got, reversed)
			}
		}
	}
}

func TestExplorerLineageTopologyReusedAcrossRequests(t *testing.T) {
	const conversations = 5000
	rows := make([]contrib, conversations)
	for i := range rows {
		parent := ""
		if i%7 == 0 && i > 0 {
			parent = fmt.Sprint("s:", i-1)
		}
		rows[i] = lineageRow(fmt.Sprint("s:", i), parent)
		rows[i].prov = "fixture.example"
	}
	p := explorerProjection(rows)
	nodes := len(p.lineage.nodes)
	allocs := testing.AllocsPerRun(3, func() {
		fold := newExplorerFold("provider", "", nil)
		fold.prepare(p, nil)
		for i := range p.rows {
			if err := fold.fold(&p.rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		_ = fold.payload()
	})
	if len(p.lineage.nodes) != nodes || nodes != conversations {
		t.Fatalf("cached topology rebuilt: nodes=%d want=%d", len(p.lineage.nodes), conversations)
	}
	// A per-request string-keyed graph costs several allocations per
	// conversation; the cached view retains only flags and payload shapes.
	if allocs > 256 {
		t.Fatalf("warm explorer allocated %.0f objects per request, want <= 256", allocs)
	}
}

// A ring/pending row can declare a parent for a conversation the projection
// already contains. The shared topology must stay immutable while the request
// view resolves the effective graph.
func TestExplorerLineagePendingOverlayChangesProjectedRole(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(16)
	now := time.Now().Add(-time.Minute)
	record := func(id, conv, parent string) *metrics.Record {
		r := mkRec(id, now, 200, "", nil, 10, 20, 1, 1, 0, 0)
		r.ConversationID, r.ParentConversationID = conv, parent
		return r
	}
	parent, child := record("parent", "s:parent", ""), record("child", "s:child", "")
	s.Record(parent)
	s.Record(child)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(buf, s, time.Second)
	p, err := api.explorer(t.Context(), "conversation", nil, "")
	if err != nil || p.Conversations != (conversationSummary{Main: 2}) {
		t.Fatalf("baseline=%+v err=%v", p.Conversations, err)
	}
	// The in-flight request is a second request in the projected child
	// conversation, not a duplicate finalized ID.
	pendingChild := record("pending-child", "s:child", "s:parent")
	buf.PublishLive("begin", pendingChild)
	p, err = api.explorer(t.Context(), "conversation", nil, "")
	if err != nil || p.Conversations != (conversationSummary{Main: 1, Sub: 1}) {
		t.Fatalf("pending parent declaration ignored: %+v err=%v", p.Conversations, err)
	}
	var childInfo *conversationInfo
	for _, g := range p.Groups {
		if g.Name == "s:child" {
			childInfo = g.Conversation
		}
	}
	if childInfo == nil || childInfo.Role != conversationSub || childInfo.ParentID != "s:parent" ||
		childInfo.ParentObserved == nil || !*childInfo.ParentObserved ||
		childInfo.ParentInScope == nil || !*childInfo.ParentInScope {
		t.Fatalf("pending child card=%+v", childInfo)
	}
	// A second pending declaration closes a cycle with the projection. Both
	// nodes must become unresolved; the cached projection topology is untouched.
	pendingParent := record("pending-parent", "s:parent", "s:child")
	buf.PublishLive("begin", pendingParent)
	p, err = api.explorer(t.Context(), "conversation", nil, "")
	if err != nil || p.Conversations != (conversationSummary{Unresolved: 2}) {
		t.Fatalf("pending cycle not resolved: %+v err=%v", p.Conversations, err)
	}
	for _, g := range p.Groups {
		if g.Name != "s:child" && g.Name != "s:parent" {
			continue
		}
		if g.Conversation == nil || g.Conversation.Role != conversationUnknown || g.Conversation.Issue != "cycle" {
			t.Fatalf("cycle card %s = %+v", g.Name, g.Conversation)
		}
	}
	if len(api.projection.data.lineage.nodes) != 2 || api.projection.data.lineage.nodes[0].issue != conversationIssueNone {
		t.Fatal("request overlay mutated the published topology")
	}
}

// A pending row can be the first observed node for a parent reference. The
// request overlay must link it without rebuilding or mutating the projection.
func TestExplorerLineagePendingParentObservation(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(16)
	now := time.Now().Add(-time.Minute)
	orphan := mkRec("orphan", now, 200, "", nil, 10, 20, 1, 1, 0, 0)
	orphan.ConversationID, orphan.ParentConversationID = "s:orphan", "s:ghost"
	s.Record(orphan)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(buf, s, time.Second)
	p, err := api.explorer(t.Context(), "conversation", nil, "")
	if err != nil || p.Conversations != (conversationSummary{Sub: 1}) {
		t.Fatalf("missing parent baseline=%+v err=%v", p.Conversations, err)
	}
	if p.Groups[0].Conversation == nil || *p.Groups[0].Conversation.ParentObserved {
		t.Fatalf("unobserved parent invented: %+v", p.Groups[0].Conversation)
	}
	ghost := mkRec("ghost", now.Add(time.Second), 200, "", nil, 10, 20, 1, 1, 0, 0)
	ghost.ConversationID = "s:ghost"
	buf.PublishLive("begin", ghost)
	p, err = api.explorer(t.Context(), "conversation", nil, "")
	if err != nil || p.Conversations != (conversationSummary{Main: 1, Sub: 1}) {
		t.Fatalf("pending parent not observed: %+v err=%v", p.Conversations, err)
	}
	for _, g := range p.Groups {
		if g.Name == "s:orphan" && (g.Conversation == nil || !*g.Conversation.ParentObserved ||
			!*g.Conversation.ParentInScope || g.Conversation.ParentID != "s:ghost") {
			t.Fatalf("pending parent card=%+v", g.Conversation)
		}
	}
}

func TestConversationTopologyAppendUpdatesIssues(t *testing.T) {
	var d contribDimensions
	rows := []contrib{lineageRow("s:child", "s:missing"), lineageRow("s:root", "")}
	for i := range rows {
		d.intern(&rows[i])
	}
	top := buildConversationTopology(rows)
	child := top.index[conversationKey{
		client: rows[0].dimensionIDs[dimClient],
		key:    rows[0].dimensionIDs[dimKey],
		id:     rows[0].dimensionIDs[dimConversation],
	}]
	if got := conversationIssueName(top.nodes[child].issue); got != "" {
		t.Fatalf("missing parent must not invent an issue, got %q", got)
	}
	incoming := []contrib{lineageRow("s:missing", "s:child")}
	d.intern(&incoming[0])
	if !top.appendRows(incoming) {
		t.Fatal("new parent declaration must dirty the topology")
	}
	top.resolve()
	if got := conversationIssueName(top.nodes[child].issue); got != "cycle" {
		t.Fatalf("append did not resolve the new cycle: %q", got)
	}
	if len(top.rowNodes) != len(rows)+len(incoming) {
		t.Fatalf("row nodes = %d, want %d", len(top.rowNodes), len(rows)+len(incoming))
	}
	leaf := lineageRow("s:leaf", "")
	d.intern(&leaf)
	if top.appendRows([]contrib{leaf}) {
		t.Fatal("a parentless leaf must not force a full re-resolution")
	}
	if len(top.rowNodes) != len(rows)+len(incoming)+1 {
		t.Fatal("leaf row node missing")
	}
}

func BenchmarkConversationLineageFold(b *testing.B) {
	for _, conversations := range []int{1, 1000, 10000} {
		b.Run(fmt.Sprint(conversations), func(b *testing.B) {
			rows := make([]contrib, 100000)
			for i := range rows {
				id := i % conversations
				parent := ""
				if id > 0 {
					parent = "conversation-0"
				}
				rows[i] = lineageRow(fmt.Sprint("conversation-", id), parent)
			}
			b.ReportAllocs()
			for b.Loop() {
				var l conversationLineage
				for i := range rows {
					l.observe(&rows[i], true, true)
				}
				l.resolve()
			}
		})
	}
}
