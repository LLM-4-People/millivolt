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
	if _, err := s.PurgeWhere(t.Context(), storage.PurgeFilter{ConversationID: "s:parent"}); err != nil {
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
