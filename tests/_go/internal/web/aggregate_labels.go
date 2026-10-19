package web

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestProjectionLabelBindingLiveStoredParity pins the A/B for the live label
// sidecar: a contribution built from a live record and the same contribution
// interned into a projection must satisfy every single-valued scope filter
// identically, including empty labels, a padded model canonicalized by rules,
// key/conversation scopes and parent lineage. Projected rows must never read a
// nil sidecar.
func TestProjectionLabelBindingLiveStoredParity(t *testing.T) {
	start := time.UnixMilli(1_700_000_000_000)
	records := []*metrics.Record{
		{ID: "empty", StatusCode: 200, Start: start},
		{ID: "padded", Provider: "P", Model: "  MODEL-A  ", StatusCode: 200, Start: start.Add(time.Second)},
		{ID: "root", Client: "c1", Provider: "P", Model: "model-a", KeyHash: "k1",
			ConversationID: "s:root", StatusCode: 200, Start: start.Add(2 * time.Second)},
		{ID: "child", Client: "c1", Provider: "P", Model: "MODEL-B", KeyHash: "k1",
			ConversationID: "s:child", ParentConversationID: "s:root",
			StatusCode: 500, ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "boom",
			Start: start.Add(3 * time.Second)},
		{ID: "other", Client: "c2", Provider: "P", Model: "model-b", KeyHash: "k2",
			ConversationID: "s:other", ParentConversationID: "s:root",
			StatusCode: 200, Start: start.Add(4 * time.Second)},
	}
	mcz := &modelCanonizer{
		exec:     config.CompileModelRules([]config.ModelRule{{Mode: config.ModelRuleLower}}),
		identity: "lower",
		memo:     map[string]string{},
	}
	live := make([]contrib, len(records))
	for i, r := range records {
		live[i] = fromRecord(r, mcz)
		if live[i].labels == nil {
			t.Fatalf("live row %s lost its label sidecar", r.ID)
		}
	}
	p, err := buildProjection(slices.Clone(live), storage.ProjectionCursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range p.rows {
		if p.rows[i].labels != nil {
			t.Fatalf("projected row %d retained the live label sidecar", i)
		}
	}
	filters := []struct {
		fs   []scopeFilter
		want int
	}{
		{nil, 5},
		{[]scopeFilter{{dim: "model", id: "model-a"}}, 2},
		{[]scopeFilter{{dim: "model", id: "MODEL-A"}}, 0}, // canonical filters are case-sensitive
		{[]scopeFilter{{dim: "provider", id: "P"}}, 4},
		{[]scopeFilter{{dim: "client", id: ""}}, 2},
		{[]scopeFilter{{dim: "client", id: "c1"}}, 2},
		{[]scopeFilter{{dim: "key", id: "k1"}}, 2},
		{[]scopeFilter{{dim: "conversation", id: "s:child"}}, 1},
		{[]scopeFilter{{dim: "provider", id: "missing"}}, 0},
		{[]scopeFilter{{dim: "model", id: "model-b"}, {dim: "key", id: "k2"}}, 1},
	}
	for _, filter := range filters {
		bound := bindScope(filter.fs, &p, mcz)
		matches := 0
		for i := range p.rows {
			stored := scopedMatch(&p.rows[i], "", bound)
			liveMatch := scopedMatch(&live[i], "", filter.fs)
			if stored != liveMatch {
				t.Fatalf("filter %+v row %s: projected match=%v, live match=%v", filter.fs, p.rows[i].id, stored, liveMatch)
			}
			if stored {
				matches++
			}
		}
		if matches != filter.want {
			t.Fatalf("filter %+v matched %d rows, want %d", filter.fs, matches, filter.want)
		}
	}
	raw := newExplorerFold("conversation", "", nil)
	encoded := newExplorerFold("conversation", "", nil)
	encoded.prepare(&p, mcz)
	for i := range p.rows {
		if err := raw.fold(&live[i]); err != nil {
			t.Fatal(err)
		}
		if err := encoded.fold(&p.rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	got, want := encoded.payload(), raw.payload()
	if got.Conversations != (conversationSummary{Main: 1, Sub: 2}) {
		t.Fatalf("lineage summary = %+v, want one root and two declared children", got.Conversations)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage/filter fold differs:\ngot  %+v\nwant %+v", got, want)
	}
}
