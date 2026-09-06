package metrics

import (
	"encoding/json"
	"testing"
	"time"
)

// The pending registry is what lets a dashboard that loads MID-REQUEST still
// show the streaming row: snapshot payloads carry the current in-flight
// (begin/update) snapshots alongside the finalized ring. Lifecycle events are
// ephemeral and never replayed, so without this a refresh loses the row until
// the request finalizes.
func TestPendingRecordsInSnapshot(t *testing.T) {
	b := NewBuffer(10)

	rec := &Record{ID: "inflight-1", Provider: "p", Start: time.Now()}
	b.PublishLive("begin", rec)

	checkJSON := func(wantPending, wantFinal int) {
		t.Helper()
		data, err := b.SnapshotJSONSince(0)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Records         []*Record `json:"records"`
			InFlightRecords []*Record `json:"in_flight_records"`
		}
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Records) != wantFinal {
			t.Errorf("finalized ring count = %d, want %d", len(p.Records), wantFinal)
		}
		if len(p.InFlightRecords) != wantPending {
			t.Fatalf("in_flight_records len = %d, want %d", len(p.InFlightRecords), wantPending)
		}
		if wantPending == 1 {
			if p.InFlightRecords[0].ID != "inflight-1" {
				t.Errorf("pending id = %q", p.InFlightRecords[0].ID)
			}
		}
	}
	checkJSON(1, 0)

	// An update refreshes the pending view (mid-flight retry visibility).
	rec.Attempts = append(rec.Attempts, RetryAttempt{StatusCode: 500, ErrorType: "retry"})
	b.PublishLive("update", rec)
	data, _ := b.SnapshotJSONSince(0)
	var p struct {
		InFlightRecords []*Record `json:"in_flight_records"`
	}
	json.Unmarshal(data, &p)
	if len(p.InFlightRecords) != 1 || len(p.InFlightRecords[0].Attempts) != 1 {
		t.Fatalf("update not reflected in pending view: %+v", p.InFlightRecords)
	}

	// Final recording atomically retires pending and publishes lifecycle end.
	b.Record(rec)
	checkJSON(0, 1)

	// The gauge pairing must stay balanced across the lifecycle.
	if got := b.Counters().InFlight; got != 0 {
		t.Errorf("InFlight = %d, want 0 after begin→end", got)
	}
}

// Multiple concurrent in-flight requests are delivered oldest-first.
func TestPendingRecordsOrdered(t *testing.T) {
	b := NewBuffer(10)
	earlier := &Record{ID: "older", Start: time.Now().Add(-2 * time.Second)}
	later := &Record{ID: "newer", Start: time.Now()}
	b.PublishLive("begin", later)
	b.PublishLive("begin", earlier)

	data, err := b.SnapshotJSONSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		InFlightRecords []*Record `json:"in_flight_records"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.InFlightRecords) != 2 || p.InFlightRecords[0].ID != "older" || p.InFlightRecords[1].ID != "newer" {
		t.Fatalf("pending order = %+v, want [older newer]", p.InFlightRecords)
	}
}
