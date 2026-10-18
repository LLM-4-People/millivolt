package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestQueryBeforeRejectsUnknownLogField pins deny-by-default filter
// validation: a typo or future enum value must error, never silently broaden
// the page to every row. Every known field keeps its existing SQL.
func TestQueryBeforeRejectsUnknownLogField(t *testing.T) {
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: "row", Provider: "p", Model: "m", KeyHash: "k", Client: "c", ConversationID: "c-1", StatusCode: 200, Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	unknown := &LogFilter{All: []LogPredicate{{Field: LogField(99), Value: "p"}}}
	rows, err := s.QueryBefore(t.Context(), nil, unknown, 10)
	if err == nil {
		t.Fatalf("unknown log field returned %d rows without an error", len(rows))
	}
	if !strings.Contains(err.Error(), "unknown log field") {
		t.Fatalf("unknown-field error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unknown log field returned %d rows", len(rows))
	}

	known := []struct {
		field  LogField
		clause string
	}{
		{LogProvider, " AND provider = ?"},
		{LogClient, " AND client = ?"},
		{LogKeyHash, " AND key_hash = ?"},
		{LogConversation, " AND conversation_id = ?"},
		{LogModel, " AND model = ?"},
		{LogStatus, " AND status_code = ?"},
	}
	for _, tc := range known {
		filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: "x"}}}
		where, args, err := filter.where()
		if err != nil {
			t.Fatalf("field %d: %v", tc.field, err)
		}
		if where != tc.clause || len(args) != 1 || args[0] != "x" {
			t.Fatalf("field %d generated where=%q args=%v, want %q with one argument", tc.field, where, args, tc.clause)
		}
		// buildQueryBefore is the statement owner; a known field must still
		// produce a query rather than an error.
		if _, _, err := buildQueryBefore(nil, filter, 1); err != nil {
			t.Fatalf("field %d: %v", tc.field, err)
		}
	}
}
