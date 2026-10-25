package storage

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestPurgeEmptiesStore(t *testing.T) {
	path := testDatabase(t)
	s, _ := Open(path, testOpts)
	for _, id := range []string{"p1", "p2", "p3"} {
		r := &metrics.Record{ID: id, Provider: "p", Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()}
		metrics.FinalizeRecord(r)
		s.Record(r)
	}
	s.Close()

	s2, _ := Open(path, testOpts)
	defer s2.Close()
	before, _ := s2.LoadRecent(t.Context(), 100)
	if len(before) != 3 {
		t.Fatalf("before purge: %d records, want 3", len(before))
	}
	if _, err := s2.Clear(t.Context(), nil, nil); err != nil {
		t.Fatalf("purge: %v", err)
	}
	after, _ := s2.LoadRecent(t.Context(), 100)
	if len(after) != 0 {
		t.Errorf("after purge: %d records, want 0", len(after))
	}
}

// seedPurgeStore writes a fixed set of records synchronously (bypassing the
// async write channel) so purge tests are deterministic.
func seedPurgeStore(t *testing.T, s *Store) {
	t.Helper()
	mk := func(id, provider string, status int, errType string) *metrics.Record {
		r := &metrics.Record{
			ID: id, Provider: provider, Model: "m", Client: "c", KeyHash: "k",
			StatusCode: status, ErrorType: errType, Start: time.Now(),
		}
		metrics.FinalizeRecord(r)
		return r
	}
	recs := []*metrics.Record{
		mk("ok-1", "alpha", 200, ""),
		mk("ok-2", "alpha", 200, ""),
		mk("rl-1", "alpha", 429, "rate_limit"), // 429 = flow control, NOT an error
		mk("err-2", "beta", 500, ""),
		mk("err-3", "beta", 200, "transport"), // status 0-style: error_type set, code < 400
		// Recovered after an absorbed 5xx attempt: final 200 but the upstream
		// genuinely failed once - counts as an error under the new semantics.
		func() *metrics.Record {
			r := mk("ret-5xx", "alpha", 200, "")
			r.Retries = 1
			r.Attempts = []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: time.Now()}}
			return r
		}(),
		// Recovered after an absorbed 429: flow control only, NOT an error.
		func() *metrics.Record {
			r := mk("ret-429", "alpha", 200, "")
			r.Retries = 1
			r.Attempts = []metrics.RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit", At: time.Now()}}
			return r
		}(),
		// 499 = local client closed the connection (its own cancellation):
		// flow event, NOT an error.
		func() *metrics.Record {
			r := mk("cancel-1", "alpha", metrics.StatusClientClosedRequest, "")
			r.ClientDisconnected = true
			return r
		}(),
		// 499 AFTER an absorbed 5xx: the upstream genuinely failed once, so it
		// still counts as an error (the cancel doesn't mask the failure).
		func() *metrics.Record {
			r := mk("cancel-5xx", "alpha", metrics.StatusClientClosedRequest, "")
			r.ClientDisconnected = true
			r.Retries = 1
			r.Attempts = []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: time.Now()}}
			return r
		}(),
		// 499 with a structured error: the provider failed in-band on the
		// stream (the error event reached the record) and the client aborted
		// around it - a genuine provider failure, not a plain cancellation.
		func() *metrics.Record {
			r := mk("cancel-err", "alpha", metrics.StatusClientClosedRequest, "api_error")
			r.ClientDisconnected = true
			return r
		}(),
	}
	if err := s.insertBatch(recs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestClearEmptyFilterRejected pins the deny-by-default invariant: an empty
// filter must error and delete nothing, so a malformed filter can never
// silently become a full wipe.
func TestClearEmptyFilterRejected(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPurgeStore(t, s)

	if _, err := s.Clear(t.Context(), &PurgeFilter{}, nil); err == nil {
		t.Fatal("Clear(empty filter) = nil error, want rejection")
	}
	n, err := s.CountWhere(t.Context(), PurgeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("after rejected purge: %d rows, want 10 (nothing deleted)", n)
	}
}

// TestClearFiltersRows verifies a filtered purge deletes exactly the
// matching rows and leaves the rest.
func TestClearFiltersRows(t *testing.T) {
	cases := []struct {
		name     string
		filter   PurgeFilter
		wantGone []string
		wantKept []string
	}{
		{
			name:     "by provider",
			filter:   PurgeFilter{Provider: "alpha"},
			wantGone: []string{"ok-1", "ok-2", "rl-1", "ret-5xx", "ret-429", "cancel-1", "cancel-5xx", "cancel-err"},
			wantKept: []string{"err-2", "err-3"},
		},
		{
			name:     "by status code",
			filter:   PurgeFilter{StatusCode: 200},
			wantGone: []string{"ok-1", "ok-2", "err-3", "ret-5xx", "ret-429"},
			wantKept: []string{"rl-1", "err-2", "cancel-1", "cancel-5xx", "cancel-err"},
		},
		{
			// HasError must match exactly the records IsError() flags - 429
			// (rl-1), an absorbed 429 (ret-429), and a plain 499 client-close
			// (cancel-1) are flow events, NOT errors; 500 (err-2), a structured
			// error_type (err-3), an absorbed 5xx (ret-5xx), and the 499s that
			// carry a failure - after an absorbed 5xx (cancel-5xx) or with the
			// in-band error observed before the client left (cancel-err - the
			// upstream still failed) - ARE. This pins the SQL predicate to the
			// canonical Go one.
			name:     "has error matches IsError",
			filter:   PurgeFilter{HasError: true},
			wantGone: []string{"err-2", "err-3", "ret-5xx", "cancel-5xx", "cancel-err"},
			wantKept: []string{"ok-1", "ok-2", "rl-1", "ret-429", "cancel-1"},
		},
		{
			name:     "combined provider+status",
			filter:   PurgeFilter{Provider: "alpha", StatusCode: 200},
			wantGone: []string{"ok-1", "ok-2", "ret-5xx", "ret-429"},
			wantKept: []string{"rl-1", "err-2", "err-3", "cancel-1", "cancel-5xx", "cancel-err"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := testDatabase(t)
			s, err := Open(path, testOpts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			seedPurgeStore(t, s)

			result, err := s.Clear(t.Context(), &tc.filter, nil)
			if err != nil {
				t.Fatalf("filtered Clear: %v", err)
			}
			if result.Deleted != int64(len(tc.wantGone)) {
				t.Errorf("deleted = %d, want %d", result.Deleted, len(tc.wantGone))
			}
			remaining, err := s.LoadRecent(t.Context(), 100)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, r := range remaining {
				got[r.ID] = true
			}
			for _, id := range tc.wantGone {
				if got[id] {
					t.Errorf("row %q survived purge, want deleted", id)
				}
			}
			for _, id := range tc.wantKept {
				if !got[id] {
					t.Errorf("row %q deleted, want kept", id)
				}
			}
		})
	}
}

// TestPurgeFilterMatchRecordParity pins SQL and in-memory purge predicates to
// the same result set, so the live ring and the database can never disagree
// about what a filter deletes.
func TestPurgeFilterMatchRecordParity(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPurgeStore(t, s)

	filters := []PurgeFilter{
		{Provider: "alpha"},
		{StatusCode: 200},
		{HasError: true},
		{Provider: "alpha", StatusCode: 200},
		{ErrorType: "transport"},
		{StatusCode: metrics.StatusClientClosedRequest},
	}
	all, err := s.LoadRecent(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range filters {
		// In-memory expectation.
		var wantMatch []string
		for _, r := range all {
			if f.MatchRecord(r) {
				wantMatch = append(wantMatch, r.ID)
			}
		}
		// SQL count must equal the in-memory match count.
		n, err := s.CountWhere(t.Context(), f)
		if err != nil {
			t.Fatalf("CountWhere(%+v): %v", f, err)
		}
		if int(n) != len(wantMatch) {
			t.Errorf("filter %+v: SQL matches %d rows, MatchRecord matches %d", f, n, len(wantMatch))
		}
	}
}

// TestExportWhereFiltersAndStreams pins the log-export contract: an empty
// filter exports EVERYTHING, a filtered export streams exactly the rows the
// purge/count predicate matches (never fewer - the dashboard's download
// preview and the download itself share this code path), and the output is a
// valid JSON array of complete records.
func TestExportWhereFiltersAndStreams(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPurgeStore(t, s) // 10 rows: see the purge tests for the fixture's shape

	// Full export: all 10 rows, valid JSON, records round-trip.
	var all bytes.Buffer
	n, err := s.ExportWhere(t.Context(), PurgeFilter{}, &all)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("full export = %d rows, want 10", n)
	}
	var recs []metrics.Record
	if err := json.Unmarshal(all.Bytes(), &recs); err != nil {
		t.Fatalf("export is not a JSON array of records: %v\n%s", err, all.String()[:min(len(all.String()), 200)])
	}
	if len(recs) != 10 {
		t.Errorf("full export decoded %d records, want 10", len(recs))
	}

	// Filtered export: same predicate as CountWhere/MatchRecord.
	var fb bytes.Buffer
	n, err = s.ExportWhere(t.Context(), PurgeFilter{Provider: "alpha"}, &fb)
	if err != nil {
		t.Fatal(err)
	}
	var got []metrics.Record
	if err := json.Unmarshal(fb.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantN, _ := s.CountWhere(t.Context(), PurgeFilter{Provider: "alpha"})
	if n != wantN || len(got) != int(wantN) {
		t.Errorf("filtered export = %d rows (%d decoded), CountWhere = %d - must agree", n, len(got), wantN)
	}
	for _, r := range got {
		if r.Provider != "alpha" {
			t.Errorf("exported row %s with provider %q, want only alpha", r.ID, r.Provider)
		}
	}

	// Errors-only export follows the canonical IsError predicate.
	var eb bytes.Buffer
	n, err = s.ExportWhere(t.Context(), PurgeFilter{HasError: true}, &eb)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("errors export = %d rows, want 5 (err-2, err-3, ret-5xx, cancel-5xx, cancel-err)", n)
	}

	debugRec := &metrics.Record{
		ID: "dbg-1", Provider: "gamma", Model: "m", Client: "c", KeyHash: "k",
		StatusCode: 200, Start: time.Now(), Debug: true, DebugSessionID: "sess",
	}
	metrics.FinalizeRecord(debugRec)
	if err := s.insertBatch([]*metrics.Record{debugRec}); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"schema":"millivolt.debug/v1","id":"dbg-1"}`)
	if err := s.SaveDebugCapture(t.Context(), "dbg-1", "sess", time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), payload); err != nil {
		t.Fatal(err)
	}
	var db bytes.Buffer
	n, err = s.ExportWhere(t.Context(), PurgeFilter{HasDebug: true}, &db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("debug export = %d rows, want 1", n)
	}
	if !bytes.Contains(db.Bytes(), []byte(`"debug_capture"`)) {
		t.Errorf("debug export missing sidecar: %s", db.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
