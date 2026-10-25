package web

import (
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// The cold durable-row path used to build a live contribLabels sidecar in
// scanContrib and drop it in intern one call later. It now passes the label
// tuple by value to internLabels, which is the same interning owner. This test
// pins the two paths against each other over every error shape whose `ent`
// (final error, absorbed 5xx, 429-only flow control, a structured 499, and no
// error) the explorer error dimension and the error scope filter read.
//
// The eager path here reconstructs the previous code exactly: take the scanned
// row, attach the tuple as the sidecar, and call intern. The lazy path is what
// production does now. Both must produce identical dictionary memberships,
// interned error text, and error-entry data - if a future change makes the
// sidecar-free path skip a label or an error signature, an explorer card would
// silently lose a row or an error event, which no existing byte-level fixture
// covers for every shape at once.
func TestInternLabelTupleEquivalence(t *testing.T) {
	start := time.UnixMilli(1_700_000_500_000)
	attemptAt := start.Add(5 * time.Millisecond)
	cases := []struct {
		name string
		rec  *metrics.Record
		// wantEnt is the error-entry count errorEntries must produce for the
		// shape; pinned separately from the cross-path equality so two wrong
		// paths cannot agree with each other.
		wantEnt int
	}{
		{
			name: "final_error",
			rec: &metrics.Record{
				ID: "final-error", Client: "cli", Provider: "prov", Model: "Vendor/Model",
				KeyHash: "key", ConversationID: "conv", ParentConversationID: "parent",
				StatusCode: 500, ErrorType: "upstream_error", ErrorCode: "E500",
				ErrorMsg: "upstream failed", Start: start,
			},
			wantEnt: 1,
		},
		{
			name: "absorbed_5xx",
			rec: &metrics.Record{
				ID: "absorbed-5xx", Client: "cli", Provider: "prov", Model: "Vendor/Model",
				KeyHash: "key", ConversationID: "conv", StatusCode: 200, Start: start,
				Attempts: []metrics.RetryAttempt{{
					StatusCode: 503, ErrorType: "http_503", ErrorCode: "503",
					ErrorMsg: "retried", At: attemptAt,
				}},
			},
			wantEnt: 1,
		},
		{
			name: "429_only",
			rec: &metrics.Record{
				ID: "rate-limit", Client: "cli", Provider: "prov", Model: "Vendor/Model",
				KeyHash: "key", ConversationID: "conv", StatusCode: 200, Start: start,
				Attempts: []metrics.RetryAttempt{{StatusCode: 429, At: attemptAt}},
			},
			wantEnt: 0, // flow control is not an error event and IsError stays false
		},
		{
			name: "499_with_error_type",
			rec: &metrics.Record{
				ID: "client-closed", Client: "cli", Provider: "prov", Model: "Vendor/Model",
				KeyHash: "key", ConversationID: "conv", StatusCode: 499,
				ErrorType: "in_band_timeout", ErrorMsg: "client aborted around an upstream error",
				Start: start,
			},
			wantEnt: 1,
		},
		{
			name: "no_error",
			rec: &metrics.Record{
				ID: "ok", Client: "cli", Provider: "prov", Model: "Vendor/Model",
				KeyHash: "key", ConversationID: "conv", StatusCode: 200, Start: start,
			},
			wantEnt: 0,
		},
	}

	recs := make([]*metrics.Record, len(cases))
	for i, tc := range cases {
		recs[i] = tc.rec
	}
	s := testStoreWithQueryTimeout(t, time.Second)
	for _, rec := range recs {
		s.Record(rec)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if dropped := s.Dropped(); dropped != 0 {
		t.Fatalf("equivalence fixture dropped %d records", dropped)
	}

	// Scan the durable rows exactly like the cold projection builder does.
	var scanned []contrib
	var tuples []contribLabels
	if _, _, err := s.StreamProjection(t.Context(), rowColumns, storage.ProjectionCursor{}, func(rows *sql.Rows) error {
		c, labels, err := scanContrib(rows, nil)
		if err != nil {
			return err
		}
		scanned = append(scanned, c)
		tuples = append(tuples, labels)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(scanned) != len(cases) {
		t.Fatalf("scanned %d rows, want %d", len(scanned), len(cases))
	}
	// scanContrib must not build the sidecar it used to; the tuple is the only
	// carrier and must be complete.
	for i := range scanned {
		if scanned[i].labels != nil {
			t.Fatalf("%s: durable scan retained a live label sidecar", cases[i].name)
		}
		if tuples[i].client == "" || tuples[i].prov == "" || tuples[i].model == "" ||
			tuples[i].key == "" || tuples[i].conv == "" {
			t.Fatalf("%s: scanned label tuple is incomplete: %+v", cases[i].name, tuples[i])
		}
	}

	var eagerDims, lazyDims contribDimensions
	for i := range scanned {
		name := cases[i].name
		tuple := tuples[i]
		eager := scanned[i]
		eager.labels = &tuple // exactly the previous scanContrib shape
		eagerDims.intern(&eager)

		lazy := scanned[i]
		lazyDims.internLabels(&lazy, tuple)

		if len(eager.ent) != cases[i].wantEnt || len(lazy.ent) != cases[i].wantEnt {
			t.Fatalf("%s: ent = %d/%d entries, want %d", name, len(eager.ent), len(lazy.ent), cases[i].wantEnt)
		}
		if !reflect.DeepEqual(eager.ent, lazy.ent) {
			t.Fatalf("%s: error entries differ\neager: %+v\n lazy: %+v", name, eager.ent, lazy.ent)
		}
		for j := range lazy.ent {
			// The exact inputs of the explorer error card's lastMs and code.
			if lazy.ent[j].at != eager.ent[j].at {
				t.Fatalf("%s entry %d: at = %d, want %d", name, j, lazy.ent[j].at, eager.ent[j].at)
			}
			if errorKey(lazy.ent[j]) != errorKey(eager.ent[j]) {
				t.Fatalf("%s entry %d: key = %q, want %q", name, j, errorKey(lazy.ent[j]), errorKey(eager.ent[j]))
			}
		}
		if eager.labels != nil || lazy.labels != nil {
			t.Fatalf("%s: an interned durable row kept its sidecar (eager=%v lazy=%v)", name, eager.labels, lazy.labels)
		}
		if lazy.isErr != eager.isErr || lazy.has429 != eager.has429 {
			t.Fatalf("%s: health predicates differ (isErr %v/%v, has429 %v/%v)",
				name, eager.isErr, lazy.isErr, eager.has429, lazy.has429)
		}
		if !reflect.DeepEqual(lazy.dimensionIDs, eager.dimensionIDs) {
			t.Fatalf("%s: dimensionIDs differ\neager: %v\n lazy: %v", name, eager.dimensionIDs, lazy.dimensionIDs)
		}
		if lazy.parentConvID != eager.parentConvID {
			t.Fatalf("%s: parentConvID = %d, want %d", name, lazy.parentConvID, eager.parentConvID)
		}
		if !reflect.DeepEqual(lazy.toolIDs, eager.toolIDs) || !reflect.DeepEqual(lazy.errorIDs, eager.errorIDs) {
			t.Fatalf("%s: tool/error ids differ\neager: %v %v\n lazy: %v %v",
				name, eager.toolIDs, eager.errorIDs, lazy.toolIDs, eager.errorIDs)
		}
	}
	if !reflect.DeepEqual(lazyDims.dict, eagerDims.dict) {
		t.Fatalf("dictionaries differ\neager: %+v\n lazy: %+v", eagerDims.dict, lazyDims.dict)
	}
	if !reflect.DeepEqual(lazyDims.errorText, eagerDims.errorText) {
		t.Fatalf("error text dictionaries differ\neager: %v\n lazy: %v", eagerDims.errorText, lazyDims.errorText)
	}
	// The 429-only shape must not enter the error dictionary at all, and the
	// final/absorbed/499 shapes must: the rail's error membership follows these
	// counts.
	if got := len(lazyDims.dict[dimError].ids); got != 3 {
		t.Fatalf("error dictionary = %d signatures, want 3 (429 and no-error shapes excluded)", got)
	}
}
