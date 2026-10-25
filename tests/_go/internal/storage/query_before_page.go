package storage

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestQueryBeforeNewestFirstExclusive(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.UnixMilli(1_700_000_000_000)
	recs := make([]*metrics.Record, 5)
	for i := 0; i < 5; i++ {
		r := &metrics.Record{
			ID: "q" + string(rune('0'+i)), Provider: "p", Model: "m", KeyHash: "k",
			StatusCode: 200, Start: base.Add(time.Duration(i) * time.Second),
		}
		metrics.FinalizeRecord(r)
		recs[i] = r
	}
	if err := s.insertBatch(recs); err != nil {
		t.Fatal(err)
	}

	got, err := s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[4].Start.UnixMilli()}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("len=%d, want 4 (exclusive of the cursor row)", len(got))
	}
	want := []string{"q3", "q2", "q1", "q0"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("got[%d]=%s, want %s", i, got[i].ID, id)
		}
	}

	got, err = s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[4].Start.UnixMilli()}, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "q3" || got[1].ID != "q2" {
		t.Fatalf("limit 2 = [%s %s], want [q3 q2]", got[0].ID, got[1].ID)
	}

	none, err := s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[0].Start.UnixMilli()}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("before oldest = %d, want 0", len(none))
	}

	z, err := s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[4].Start.UnixMilli()}, nil, 0)
	if err != nil || z != nil {
		t.Fatalf("limit 0 = %v, %v; want nil, nil", z, err)
	}
}

func TestQueryBeforeExactPushdownPredicates(t *testing.T) {
	ids := func(rs []*metrics.Record) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.ID
		}
		return out
	}
	s := openTestStore(t)
	start := time.UnixMilli(1_800_000_000_000)
	rows := []*metrics.Record{
		{ID: "a", Provider: "p1", Client: "c1", Model: "m", Start: start, StatusCode: 200},
		{ID: "b", Provider: "p1", Client: "c2", Model: "m", Start: start.Add(time.Millisecond), StatusCode: 200},
		{ID: "c", Provider: "p2", Client: "c1", Model: "m", Start: start.Add(2 * time.Millisecond), StatusCode: 500},
	}
	for _, r := range rows {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryBefore(t.Context(), nil, &LogFilter{All: []LogPredicate{
		{Field: LogProvider, Value: "p1"},
		{Field: LogClient, Value: "c1"},
	}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("filtered page = %v, want [a]", ids(got))
	}
	// Contradictory predicates must return nothing, not widen the scope.
	got, err = s.QueryBefore(t.Context(), nil, &LogFilter{All: []LogPredicate{
		{Field: LogProvider, Value: "p1"},
		{Field: LogProvider, Value: "p2"},
	}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("contradictory predicates returned %v", ids(got))
	}
	// The cursor still applies together with the pushdown predicate.
	got, err = s.QueryBefore(t.Context(), &LogCursor{StartMs: rows[1].Start.UnixMilli(), ID: "b"}, &LogFilter{All: []LogPredicate{{Field: LogProvider, Value: "p1"}}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("cursor + filter page = %v, want [a]", ids(got))
	}
}
