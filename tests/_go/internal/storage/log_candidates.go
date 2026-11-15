package storage

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestQueryBeforeCandidatesProjectsMatcherFields pins the narrow projection:
// every field the Go log-page matcher derives must round-trip identically to
// the full-record scan, without materializing any response-only column.
func TestQueryBeforeCandidatesProjectsMatcherFields(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	attempts := []metrics.RetryAttempt{
		{StatusCode: 503, ErrorType: "overloaded", ErrorCode: "E1", ErrorMsg: "retry", At: base.Add(-time.Second)},
		{StatusCode: 500, ErrorType: "upstream", ErrorCode: "E2", ErrorMsg: "boom"},
	}
	rows := []*metrics.Record{
		{ID: "a", Provider: "p1", Model: " Model-A ", Start: base, StatusCode: 200, ToolNames: []string{"search", "fetch"}},
		{ID: "b", Provider: "p2", Model: "model-b", Start: base.Add(time.Millisecond), StatusCode: 500,
			ErrorType: "upstream", ErrorCode: "E2", ErrorMsg: "boom", Attempts: attempts},
		{ID: "c", Provider: "p1", Model: "model-c", Start: base.Add(2 * time.Millisecond), StatusCode: 429},
	}
	for _, r := range rows {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	full, err := s.QueryBefore(t.Context(), nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := s.QueryBeforeCandidates(t.Context(), nil, nil, 10, LogCandidatesAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != len(full) {
		t.Fatalf("candidates = %d, full rows = %d", len(candidates), len(full))
	}
	for i, r := range full {
		c := candidates[i]
		if c.Cursor.ID != r.ID || c.Cursor.StartMs != r.Start.UnixMilli() {
			t.Fatalf("candidate[%d] cursor = %+v, want %s/%d", i, c.Cursor, r.ID, r.Start.UnixMilli())
		}
		if c.Status != r.StatusCode || c.Model != r.Model ||
			!reflect.DeepEqual(c.ToolNames, r.ToolNames) ||
			c.ErrorType != r.ErrorType || c.ErrorCode != r.ErrorCode || c.ErrorMsg != r.ErrorMsg {
			t.Fatalf("candidate[%d] projection = %+v, want status/model/tools/error from %+v", i, c, r)
		}
		if len(c.Attempts) != len(r.Attempts) {
			t.Fatalf("candidate[%d] attempts = %d, want %d", i, len(c.Attempts), len(r.Attempts))
		}
		for j := range r.Attempts {
			got, want := c.Attempts[j], r.Attempts[j]
			if got.StatusCode != want.StatusCode || got.ErrorType != want.ErrorType ||
				got.ErrorCode != want.ErrorCode || got.ErrorMsg != want.ErrorMsg ||
				!got.At.Equal(want.At) {
				t.Fatalf("candidate[%d] attempt %d = %+v, want %+v", i, j, got, want)
			}
		}
	}
}

// TestQueryBeforeCandidatesAppliesExactFilterAndCursor pins that the narrow
// read shares the full page's SQL predicate and cursor semantics.
func TestQueryBeforeCandidatesAppliesExactFilterAndCursor(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	for i := 0; i < 12; i++ {
		s.Record(&metrics.Record{
			ID: fmtRowID(i), Provider: map[bool]string{true: "target", false: "other"}[i%2 == 0],
			Model: "m", Start: base.Add(time.Duration(i) * time.Millisecond), StatusCode: 200,
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	filter := &LogFilter{All: []LogPredicate{{Field: LogProvider, Value: "target"}}}
	before := &LogCursor{StartMs: base.Add(9 * time.Millisecond).UnixMilli(), ID: fmtRowID(9)}
	full, err := s.QueryBefore(t.Context(), before, filter, 5)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := s.QueryBeforeCandidates(t.Context(), before, filter, 5, LogCandidatesAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != len(candidates) || len(full) == 0 {
		t.Fatalf("full = %d, candidates = %d, want equal and non-empty", len(full), len(candidates))
	}
	for i := range full {
		if candidates[i].Cursor.ID != full[i].ID {
			t.Fatalf("candidate[%d] = %s, want %s", i, candidates[i].Cursor.ID, full[i].ID)
		}
	}
}

// TestLogPageRecordsPreservesAcceptedOrder pins the one bounded
// materialization call: accepted scan cursors come back as complete records in
// their original newest-first order.
func TestLogPageRecordsPreservesAcceptedOrder(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	for i := 0; i < 6; i++ {
		s.Record(&metrics.Record{
			ID: fmtRowID(i), Provider: "p", Model: "m-" + fmtRowID(i),
			Start: base.Add(time.Duration(i) * time.Millisecond), StatusCode: 200,
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	all, err := s.QueryBeforeCandidates(t.Context(), nil, nil, 10, LogCandidatesAll)
	if err != nil {
		t.Fatal(err)
	}
	accepted := []LogCursor{all[1].Cursor, all[3].Cursor, all[4].Cursor}
	records, err := s.LogPageRecords(t.Context(), accepted)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(accepted) {
		t.Fatalf("materialized %d rows, want %d", len(records), len(accepted))
	}
	for i, c := range accepted {
		if records[i].ID != c.ID || records[i].Start.UnixMilli() != c.StartMs {
			t.Fatalf("record[%d] = %s/%d, want %s/%d", i, records[i].ID, records[i].Start.UnixMilli(), c.ID, c.StartMs)
		}
		if records[i].Model != "m-"+c.ID {
			t.Fatalf("record[%d] model = %q, want full row", i, records[i].Model)
		}
	}
}

// TestQueryBeforeCandidatesSelectsOnlyRequestedFields pins the field mask: a
// plan that names no derived field must not decode any stored JSON, and a mask
// naming one field must leave the others zero. Status and the cursor are always
// present.
func TestQueryBeforeCandidatesSelectsOnlyRequestedFields(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	s.Record(&metrics.Record{ID: "r", Provider: "p", Model: "model-r", Start: base, StatusCode: 500,
		ToolNames: []string{"search"}, ErrorType: "upstream", ErrorCode: "E2", ErrorMsg: "boom",
		Attempts: []metrics.RetryAttempt{{StatusCode: 503, ErrorType: "overloaded"}}})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		fields LogCandidateFields
		want   func(t *testing.T, c LogCandidate)
	}{
		{"no fields", 0, func(t *testing.T, c LogCandidate) {
			if c.Model != "" || c.ToolNames != nil || c.Attempts != nil || c.ErrorType != "" ||
				c.ErrorCode != "" || c.ErrorMsg != "" {
				t.Fatalf("candidate = %+v, want only the cursor and status", c)
			}
		}},
		{"tools only", LogCandidatesTools, func(t *testing.T, c LogCandidate) {
			if !reflect.DeepEqual(c.ToolNames, []string{"search"}) || c.Model != "" || c.Attempts != nil {
				t.Fatalf("candidate = %+v, want only the tool names", c)
			}
		}},
		{"errors only", LogCandidatesErrors, func(t *testing.T, c LogCandidate) {
			if c.ErrorType != "upstream" || c.ErrorCode != "E2" || c.ErrorMsg != "boom" ||
				len(c.Attempts) != 1 || c.ToolNames != nil || c.Model != "" {
				t.Fatalf("candidate = %+v, want only the error columns", c)
			}
		}},
		{"model only", LogCandidatesModel, func(t *testing.T, c LogCandidate) {
			if c.Model != "model-r" || c.ToolNames != nil || c.Attempts != nil {
				t.Fatalf("candidate = %+v, want only the model", c)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidates, err := s.QueryBeforeCandidates(t.Context(), nil, nil, 1, tc.fields)
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 {
				t.Fatalf("candidates = %d, want 1", len(candidates))
			}
			if candidates[0].Cursor.ID != "r" || candidates[0].Cursor.StartMs != base.UnixMilli() ||
				candidates[0].Status != 500 {
				t.Fatalf("cursor/status = %+v/%d, want r/%d/500", candidates[0].Cursor, candidates[0].Status, base.UnixMilli())
			}
			tc.want(t, candidates[0])
		})
	}
}

// TestLogPageRecordsAcceptsEmptyRecordID pins that the schema's empty id is a
// valid exact key. QueryBeforeCandidates returns an accepted row with id "",
// and the log page used to reject that same key during materialization, so one
// accepted empty-id row made every /metrics/agg/log request fail with 500.
func TestLogPageRecordsAcceptsEmptyRecordID(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	for _, r := range []*metrics.Record{
		{ID: "row-0", Provider: "p", Model: "m-row-0", Start: base, StatusCode: 200},
		{ID: "", Provider: "p", Model: "m-empty", Start: base.Add(time.Millisecond), StatusCode: 200},
		{ID: "row-2", Provider: "p", Model: "m-row-2", Start: base.Add(2 * time.Millisecond), StatusCode: 200},
	} {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	candidates, err := s.QueryBeforeCandidates(t.Context(), nil, nil, 10, LogCandidatesAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidates = %d, want 3", len(candidates))
	}
	for i, want := range []string{"row-2", "", "row-0"} {
		if candidates[i].Cursor.ID != want {
			t.Fatalf("candidate[%d] id = %q, want %q", i, candidates[i].Cursor.ID, want)
		}
	}

	accepted := make([]LogCursor, 0, len(candidates))
	for _, candidate := range candidates {
		accepted = append(accepted, candidate.Cursor)
	}
	records, err := s.LogPageRecords(t.Context(), accepted)
	if err != nil {
		t.Fatalf("LogPageRecords rejected an accepted empty id: %v", err)
	}
	if len(records) != len(accepted) {
		t.Fatalf("materialized %d rows, want %d", len(records), len(accepted))
	}
	for i, cursor := range accepted {
		if records[i].ID != cursor.ID || records[i].Start.UnixMilli() != cursor.StartMs {
			t.Fatalf("record[%d] = %q/%d, want %q/%d", i, records[i].ID, records[i].Start.UnixMilli(), cursor.ID, cursor.StartMs)
		}
	}
	if records[1].ID != "" || records[1].Model != "m-empty" {
		t.Fatalf("empty-id row = %+v, want the full stored row", records[1])
	}
}

// TestLogPageRecordsFailsClosed pins that an accepted row missing from the
// materialization query is an explicit consistency error, never a shortened
// successful page.
func TestLogPageRecordsFailsClosed(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	s.Record(&metrics.Record{ID: "present", Provider: "p", Model: "m", Start: base, StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LogPageRecords(t.Context(), nil); err != nil || got != nil {
		t.Fatalf("empty page = %v, %v; want nil, nil", got, err)
	}
	cases := map[string][]LogCursor{
		"missing row":     {{StartMs: base.UnixMilli(), ID: "absent"}},
		"absent empty id": {{StartMs: base.UnixMilli(), ID: ""}},
		"duplicate id":    {{StartMs: base.UnixMilli(), ID: "present"}, {StartMs: base.UnixMilli(), ID: "present"}},
		"mixed missing":   {{StartMs: base.UnixMilli(), ID: "present"}, {StartMs: base.UnixMilli(), ID: "absent"}},
	}
	for name, cursors := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := s.LogPageRecords(t.Context(), cursors); err == nil || got != nil {
				t.Fatalf("LogPageRecords = %v, %v; want explicit error and no partial page", got, err)
			}
		})
	}
}

func fmtRowID(i int) string {
	return "row-" + strconv.Itoa(i)
}
