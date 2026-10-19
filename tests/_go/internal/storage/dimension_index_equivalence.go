package storage

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// logQuerySnapshot is the result shape the equivalence regression compares: the
// projected candidate rows, the full records, and the purge-preview counts. It
// is deliberately plain data so equality means the query results and their
// order are identical, not merely the same row count.
type logQuerySnapshot struct {
	candidates []LogCandidate
	records    []*metrics.Record
	counts     map[string]int64
}

// dimensionIndexFixture seeds every equality dimension with enough matching
// rows to cross a small page limit, plus the edge shapes the predicate must not
// change: an empty id, empty client and conversation labels, repeated
// timestamps, and exact status codes whose classes exist only in Go.
func dimensionIndexFixture(t *testing.T, s *Store) {
	t.Helper()
	base := time.UnixMilli(1_900_000_000_000)
	rows := make([]*metrics.Record, 0, 40)
	for i := range 40 {
		id := fmt.Sprintf("eq-%02d", i)
		if i == 17 {
			// The schema admits one empty primary key. Keep its timestamp tied
			// with its neighbours so the descending id tie-break decides it.
			id = ""
		}
		model := "m1"
		switch i % 3 {
		case 0:
			model = " m0 "
		case 1:
			model = "m0"
		}
		client := ""
		if i%2 == 0 {
			client = "c0"
		}
		conversation := ""
		if i%5 == 0 {
			conversation = "conv-0"
		}
		status := []int{200, 429, 500}[i%3]
		row := &metrics.Record{
			ID: id, Provider: fmt.Sprintf("p%d", i%2), Model: model,
			Client: client, KeyHash: fmt.Sprintf("k%d", i%4), ConversationID: conversation,
			StatusCode: status, Start: base.Add(time.Duration(i/3) * time.Millisecond),
		}
		if status == 500 {
			row.ErrorType = "upstream_error"
			row.ErrorMsg = "fixture failure"
		}
		rows = append(rows, row)
	}
	if err := s.insertBatch(rows); err != nil {
		t.Fatal(err)
	}
}

// snapshotLogQueries captures the production read paths whose SQL predicates
// are shared with the index layout: every exact LogFilter equality with and
// without a cursor, plus the purge-preview counts.
func snapshotLogQueries(t *testing.T, s *Store) logQuerySnapshot {
	t.Helper()
	cursor := &LogCursor{StartMs: time.UnixMilli(1_900_000_000_000).Add(5 * time.Millisecond).UnixMilli(), ID: "eq-15"}
	fields := []struct {
		field LogField
		value string
	}{
		{LogProvider, "p0"},
		{LogModel, "m0"},
		{LogClient, "c0"},
		{LogKeyHash, "k0"},
		{LogConversation, "conv-0"},
		{LogStatus, "500"},
	}
	snapshot := logQuerySnapshot{counts: map[string]int64{}}
	for _, tc := range fields {
		for _, before := range []*LogCursor{nil, cursor} {
			filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: tc.value}}}
			candidates, err := s.QueryBeforeCandidates(t.Context(), before, filter, 7)
			if err != nil {
				t.Fatalf("candidates %v/%q: %v", tc.field, tc.value, err)
			}
			records, err := s.QueryBefore(t.Context(), before, filter, 7)
			if err != nil {
				t.Fatalf("records %v/%q: %v", tc.field, tc.value, err)
			}
			snapshot.candidates = append(snapshot.candidates, candidates...)
			snapshot.records = append(snapshot.records, records...)
		}
	}
	beforeMs := time.UnixMilli(1_900_000_000_000).Add(10 * time.Millisecond).UnixMilli()
	for name, filter := range map[string]PurgeFilter{
		"provider":            {Provider: "p0"},
		"model":               {Model: "m0"},
		"client":              {Client: "c0"},
		"conversation":        {ConversationID: "conv-0"},
		"status":              {StatusCode: 500},
		"before":              {BeforeMs: beforeMs},
		"provider_and_status": {Provider: "p0", StatusCode: 500},
		"has_error":           {HasError: true},
		"error_type":          {ErrorType: "upstream_error"},
	} {
		n, err := s.CountWhere(t.Context(), filter)
		if err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		snapshot.counts[name] = n
	}
	return snapshot
}

// TestDimensionIndexesAreOptionalForQueryResults is the regression for removing
// the six per-dimension composites: the same fixture is queried before and
// after recreating them, and every candidate page, full-record page, cursor
// page and purge count must be identical. Only the access path may differ.
func TestDimensionIndexesAreOptionalForQueryResults(t *testing.T) {
	s := openTestStore(t)
	dimensionIndexFixture(t, s)
	providerFilter := &LogFilter{All: []LogPredicate{{Field: LogProvider, Value: "p0"}}}
	planQuery, planArgs, err := buildQueryBefore(nil, providerFilter, 7)
	if err != nil {
		t.Fatal(err)
	}
	if plan := explainQueryPlan(t, s.rdb, planQuery, planArgs); !strings.Contains(plan, "idx_requests_log") {
		t.Fatalf("baseline provider page does not use idx_requests_log:\n%s", plan)
	}

	withoutIndexes := snapshotLogQueries(t, s)
	if len(withoutIndexes.candidates) == 0 || len(withoutIndexes.records) == 0 {
		t.Fatal("fixture produced no filtered rows; the regression would be vacuous")
	}
	createLegacyDimensionRequestIndexes(t, s.db)
	for _, index := range legacyDimensionRequestIndexes {
		if !requestIndexExists(t, s.rdb, index.name) {
			t.Fatalf("fixture did not create legacy index %s", index.name)
		}
	}
	if _, err := s.db.ExecContext(t.Context(), "ANALYZE requests"); err != nil {
		t.Fatalf("analyze with legacy indexes: %v", err)
	}
	// Prove the comparison exercised a different access path, not merely an
	// ignored index: the old schema must win the planner's cost comparison.
	if plan := explainQueryPlan(t, s.rdb, planQuery, planArgs); !strings.Contains(plan, "idx_requests_provider_log") {
		t.Fatalf("provider page did not switch to the old composite index:\n%s", plan)
	}

	withIndexes := snapshotLogQueries(t, s)
	if !reflect.DeepEqual(withIndexes, withoutIndexes) {
		t.Fatalf("results changed when the six dimension indexes were present:\nwith indexes: %+v\nwithout: %+v", withIndexes, withoutIndexes)
	}
}
