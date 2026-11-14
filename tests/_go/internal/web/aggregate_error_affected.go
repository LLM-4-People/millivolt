package web

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
)

// TestExplorerErrorCardAffectedRequestsBounded pins the error card's affected
// request count to the exact set semantics Record.IsError and the gallery
// share use, while keeping the accumulator free of any history-proportional
// state. One request contributes one error-card membership per distinct
// signature it carries, and a signature may repeat within that request (several
// absorbed attempts) and recur across requests. Visits for one request arrive
// contiguously, so the previous request id is the whole dedupe state: the
// reference implementation below builds the ID set the old code kept per card,
// and the fold must agree with it on every signature.
//
// The accumulator must also stay constant per card: the structural walk fails
// if a per-card field is a map, and the allocation bound proves a signature
// recurring across many distinct requests does not retain one entry per
// request.
func TestExplorerErrorCardAffectedRequestsBounded(t *testing.T) {
	type entry struct{ typ, code, msg string }
	rows := []struct {
		id      string
		entries []entry
	}{
		// s1 twice inside one request: affected count stays 1 for that request.
		{"a", []entry{{"upstream", "500", "a"}, {"upstream", "500", "a"}, {"timeout", "504", "a"}}},
		// s1 and s2 recur across requests; s2 twice inside this one.
		{"b", []entry{{"upstream", "500", "a"}, {"timeout", "504", "a"}, {"timeout", "504", "a"}}},
		{"c", []entry{{"timeout", "504", "a"}}},
		// A plain request with no error entries must not disturb the cards.
		{"d", nil},
	}

	// Reference: the old per-card request-ID set, computed independently.
	wantAffected := map[string]map[string]struct{}{}
	wantEvents := map[string]int{}
	for _, row := range rows {
		for _, e := range row.entries {
			key := e.typ + "|" + e.code + "|" + e.msg
			wantEvents[key]++
			if wantAffected[key] == nil {
				wantAffected[key] = map[string]struct{}{}
			}
			wantAffected[key][row.id] = struct{}{}
		}
	}

	f := newExplorerFold("error", "", nil)
	for _, row := range rows {
		c := contrib{id: row.id, status: 200}
		for _, e := range row.entries {
			c.ent = append(c.ent, errEnt{typ: e.typ, code: e.code, msg: e.msg})
		}
		if err := f.fold(&c); err != nil {
			t.Fatal(err)
		}
	}
	p := f.payload()
	if len(p.Groups) != len(wantAffected) {
		t.Fatalf("cards = %d, want %d", len(p.Groups), len(wantAffected))
	}
	for _, card := range p.Groups {
		want := wantAffected[card.Name]
		if card.N != int64(len(want)) {
			t.Errorf("%s affected N = %d, want %d (distinct requests)", card.Name, card.N, len(want))
		}
		if card.ErrEvents != int64(wantEvents[card.Name]) {
			t.Errorf("%s err_events = %d, want %d (occurrences)", card.Name, card.ErrEvents, wantEvents[card.Name])
		}
		if card.N > p.ErrorTot {
			t.Errorf("%s N %d exceeds error_total %d", card.Name, card.N, p.ErrorTot)
		}
	}
	if p.ErrorTot != 3 {
		t.Fatalf("error_total = %d, want 3 affected requests (a, b, c)", p.ErrorTot)
	}
}

// TestExplorerErrorCardAccumulatorHasNoRequestSet pins the bounded-accumulator
// invariant two ways. Structurally, no per-card field may be a map: a map keyed
// by request id is exactly the history-proportional state the last-request
// guard replaces. Behaviourally, one error signature recurring across many
// distinct requests must not RETAIN per-request state: a card's live heap after
// a huge fold stays the size of one accumulator plus its bounded samples, not
// one map entry per request.
func TestExplorerErrorCardAccumulatorHasNoRequestSet(t *testing.T) {
	typ := reflect.TypeOf(entAcc{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() == reflect.Map {
			t.Errorf("entAcc.%s is a map: per-card maps keyed by request id grow with history; visits for one request are contiguous, so lastRequest owns the dedupe", field.Name)
		}
	}

	// One signature, many distinct requests. A request-ID set retains one key
	// plus map bucket per request; the bounded guard retains one accumulator.
	const requests = 200_000
	ids := make([]string, requests)
	for i := range ids {
		ids[i] = fmt.Sprintf("row-%07d", i)
	}
	ent := []errEnt{{typ: "upstream", code: "500", msg: "boom"}}
	build := func() *explorerFold {
		f := newExplorerFold("error", "", nil)
		for i, id := range ids {
			c := contrib{id: id, start: int64(i), status: 500, isErr: true, ent: ent}
			if err := f.fold(&c); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	f := build()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// A per-request set for this fixture retains far more than this: the key
	// strings are shared with ids, but each map entry and its bucket costs tens
	// of bytes, so 200k requests is several megabytes. One accumulator plus its
	// samples is a few kilobytes.
	const maxRetained = 1 << 20
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if retained > maxRetained {
		t.Fatalf("one signature over %d requests retained %.1f MiB, want at most %d MiB (per-request set?)",
			requests, float64(retained)/(1<<20), maxRetained>>20)
	}
	if got := f.payload().Groups[0].N; got != requests {
		t.Fatalf("affected N = %d, want %d", got, requests)
	}
	runtime.KeepAlive(f)
}
