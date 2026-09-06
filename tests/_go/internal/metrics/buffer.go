package metrics

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBufferAggregateRevisions(t *testing.T) {
	b := NewBuffer(2)
	want := func(finalized, pending uint64) {
		t.Helper()
		f, p := b.Revisions()
		if f != finalized || p != pending {
			t.Fatalf("revisions = (%d,%d), want (%d,%d)", f, p, finalized, pending)
		}
	}
	want(0, 0)
	b.Record(nil)
	b.Backfill([]*Record{nil})
	b.PublishLive("ignored", &Record{ID: "pending"})
	want(0, 0)
	for i, phase := range []string{"begin", "update"} {
		b.PublishLive(phase, &Record{ID: "pending", Stream: true})
		want(0, uint64(i+1))
	}
	b.Record(&Record{ID: "pending"}) // atomically retires pending and records final
	want(1, 3)
	b.Backfill([]*Record{{ID: "b"}, {ID: "c"}}) // includes ring eviction
	want(3, 3)
	b.Record(&Record{ID: "d"}) // another eviction must invalidate too
	want(4, 3)
	b.RemoveWhere(func(*Record) bool { return false })
	want(4, 3)
	if n := b.RemoveWhere(func(r *Record) bool { return r.ID == "c" }); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	want(5, 3)
	b.Reset()
	want(6, 3)
	b.Reset() // neither revision nor feed cursor may rewind after a purge
	want(7, 3)
}

func TestBufferSnapshot(t *testing.T) {
	b := NewBuffer(3)
	b.Record(&Record{ID: "a", Provider: "alpha"})
	b.Record(&Record{ID: "b", Provider: "beta"})
	b.Record(&Record{ID: "c", Provider: "gamma"})
	b.Record(&Record{ID: "d", Provider: "alpha"}) // wraps

	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len = %d, want 3", len(snap))
	}
	// Wraps: a is evicted, b c d remain.
	if snap[0].ID != "b" || snap[1].ID != "c" || snap[2].ID != "d" {
		t.Errorf("order: got %s %s %s, want b c d", snap[0].ID, snap[1].ID, snap[2].ID)
	}
}

func TestAggregate(t *testing.T) {
	records := []*Record{
		{Provider: "alpha", Model: "gpt-4", StatusCode: 200, Usage: Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5}, ToolCalls: 1, ErrorType: ""},
		// 429 is flow control, NOT an error - excluded from ErrorCount.
		{Provider: "alpha", Model: "gpt-4", StatusCode: 429, Usage: Usage{InputTokens: 5, OutputTokens: 0}, ErrorType: "rate_limit_error"},
		{Provider: "beta", Model: "llama3", StatusCode: 200, Usage: Usage{InputTokens: 8, OutputTokens: 15}},
		// A genuine upstream failure (500) IS an error.
		{Provider: "beta", Model: "llama3", StatusCode: 500, Usage: Usage{InputTokens: 1, OutputTokens: 0}, ErrorType: "provider_error"},
	}
	agg := Aggregate(records)

	if agg.Total != 4 {
		t.Errorf("Total = %d", agg.Total)
	}
	if agg.TotalInput != 24 {
		t.Errorf("TotalInput = %d", agg.TotalInput)
	}
	if agg.TotalOutput != 35 {
		t.Errorf("TotalOutput = %d", agg.TotalOutput)
	}
	if agg.TotalCache != 5 {
		t.Errorf("TotalCache = %d", agg.TotalCache)
	}
	// Only the 500 counts; the 429 (flow control) does not.
	if agg.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d", agg.ErrorCount)
	}
	if agg.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d", agg.ToolCalls)
	}
	if agg.ByStatus[200] != 2 {
		t.Errorf("ByStatus[200] = %d", agg.ByStatus[200])
	}
	if agg.ByStatus[429] != 1 {
		t.Errorf("ByStatus[429] = %d", agg.ByStatus[429])
	}
	if agg.ByStatus[500] != 1 {
		t.Errorf("ByStatus[500] = %d", agg.ByStatus[500])
	}
}

// TestAggregateIsErrorSemantics pins the error contract: 429 (flow control)
// and 499 (the LOCAL client closed the connection - its own cancellation)
// never count on their own; a genuine failure counts whether it's the final
// outcome OR an absorbed retry attempt the client never saw.
func TestAggregateIsErrorSemantics(t *testing.T) {
	cases := []struct {
		name string
		rec  *Record
		want bool
	}{
		{"final 200, no attempts", &Record{StatusCode: 200}, false},
		{"final 429 rate limit", &Record{StatusCode: 429, ErrorType: "rate_limit_error", RateLimited: true}, false},
		{"final 499 client closed", &Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true}, false},
		{"final 499 after an absorbed 5xx", &Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true},
		{"final 500", &Record{StatusCode: 500}, true},
		{"final 502", &Record{StatusCode: 502}, true},
		{"final 400 (4xx other than 429)", &Record{StatusCode: 400}, true},
		{"structured error, status 0", &Record{StatusCode: 0, ErrorType: "upstream_unreachable"}, true},
		{"recovered after absorbed 5xx", &Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 502}}}, true},
		{"recovered after absorbed 429 only", &Record{StatusCode: 200, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit"}}}, false},
		{"final 429 after an absorbed 5xx", &Record{StatusCode: 429, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true},
	}
	for _, tc := range cases {
		if got := tc.rec.IsError(); got != tc.want {
			t.Errorf("%s: IsError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSnapshotJSON(t *testing.T) {
	b := NewBuffer(100)
	b.Record(&Record{ID: "x", Provider: "test"})
	raw, err := b.SnapshotJSONSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	recs, ok := out["records"].([]any)
	if !ok || len(recs) == 0 {
		t.Error("snapshot records is empty")
	}
}

func TestPercentile(t *testing.T) {
	sorted := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := Percentile(sorted, 50); p != 5 {
		t.Errorf("p50 = %f, want 5", p)
	}
	if p := Percentile(sorted, 95); p != 10 {
		t.Errorf("p95 = %f, want 10", p)
	}
}

func TestFinalizeRecordSubMillisecondTTFT(t *testing.T) {
	// A fast upstream can produce a sub-millisecond TTFT; it must not be
	// truncated to 0 (which would read as "no tokens captured").
	start := time.Now()
	r := &Record{Start: start, FirstTokenAt: start.Add(300 * time.Microsecond), End: start.Add(time.Second)}
	FinalizeRecord(r)
	if r.TTFTMs != 1 {
		t.Errorf("TTFTMs = %d, want 1 (sub-ms TTFT rounded up)", r.TTFTMs)
	}

	// No tokens → FirstTokenAt zero → TTFTMs stays 0.
	r2 := &Record{Start: start, End: start.Add(time.Second)}
	FinalizeRecord(r2)
	if r2.TTFTMs != 0 {
		t.Errorf("TTFTMs = %d, want 0 when no tokens", r2.TTFTMs)
	}
}

// TestPublishLiveTracksInFlight pins the in-flight gauge contract: the counter
// moves on begin/end publishes even with no subscribers connected (it is
// process-wide, not per-subscriber), each event carries the authoritative
// post-transition value, and standalone completions never settle another request.
func TestPublishLiveTracksInFlight(t *testing.T) {
	b := NewBuffer(4)

	b.PublishLive("begin", &Record{ID: "a"})
	b.PublishLive("begin", &Record{ID: "b"})
	if got := b.Counters().InFlight; got != 2 {
		t.Fatalf("InFlight after 2 begins = %d, want 2", got)
	}

	ch := b.SubscribeLive()
	defer b.UnsubscribeLive(ch)

	b.Record(&Record{ID: "a"})
	if ev := (<-ch).InFlight; ev != 1 {
		t.Errorf("end event InFlight = %d, want 1", ev)
	}

	b.PublishLive("begin", &Record{ID: "c"})
	if ev := (<-ch).InFlight; ev != 2 {
		t.Errorf("begin event InFlight = %d, want 2", ev)
	}

	b.Record(&Record{ID: "b"})
	b.Record(&Record{ID: "c"})
	b.Record(&Record{ID: "standalone"})
	if got := b.Counters().InFlight; got != 0 {
		t.Errorf("InFlight after completions = %d, want 0", got)
	}
}

// The "update" phase is a mid-flight progress signal (an absorbed retry
// attempt). It must NOT move the in-flight gauge (the request is still inside
// its begin→end window), and it must deliver a consistent snapshot of the
// attempts even though the proxy may keep appending to the live record.
func TestPublishLiveUpdateDoesNotMoveGauge(t *testing.T) {
	b := NewBuffer(4)
	ch := b.SubscribeLive()
	defer b.UnsubscribeLive(ch)

	live := &Record{ID: "r1"}
	b.PublishLive("begin", live)
	if got := b.Counters().InFlight; got != 1 {
		t.Fatalf("InFlight after begin = %d, want 1", got)
	}
	<-ch // drain begin

	// Absorb a retry attempt mid-flight, then publish an update. The proxy then
	// mutates the SAME live record (appends more attempts) - the subscriber's
	// snapshot must be a stable point-in-time copy, not a torn shared slice.
	live.Attempts = append(live.Attempts, RetryAttempt{StatusCode: 0, ErrorType: "transport", ErrorMsg: "timeout awaiting response headers"})
	live.Retries = 1
	b.PublishLive("update", live)
	// Mutate the live record after publishing: a later attempt must NOT leak
	// into the snapshot the subscriber already received.
	live.Attempts = append(live.Attempts, RetryAttempt{StatusCode: 502, ErrorType: "upstream_error"})
	live.Retries = 2

	ev := <-ch
	if ev.Phase != "update" {
		t.Fatalf("phase = %q, want update", ev.Phase)
	}
	if ev.InFlight != 1 {
		t.Errorf("update moved the in-flight gauge: got %d, want 1 (unchanged)", ev.InFlight)
	}
	if got := b.Counters().InFlight; got != 1 {
		t.Errorf("InFlight after update = %d, want 1 (update must not move the gauge)", got)
	}
	if len(ev.Record.Attempts) != 1 || ev.Record.Retries != 1 {
		t.Errorf("update snapshot saw post-publish mutation: attempts=%d retries=%d, want 1/1",
			len(ev.Record.Attempts), ev.Record.Retries)
	}
	if ev.Record.Attempts[0].ErrorType != "transport" {
		t.Errorf("attempt[0].ErrorType = %q, want transport", ev.Record.Attempts[0].ErrorType)
	}
}

func TestBufferReset(t *testing.T) {
	b := NewBuffer(4)
	for i := 0; i < 3; i++ {
		b.Record(&Record{ID: "x", Provider: "p", Model: "m", StatusCode: 200})
	}
	if b.Len() != 3 {
		t.Fatalf("len = %d, want 3", b.Len())
	}
	if b.Counters().TotalReq != 3 {
		t.Fatalf("totalReq = %d, want 3", b.Counters().TotalReq)
	}
	b.Reset()
	if b.Len() != 0 {
		t.Errorf("len after reset = %d, want 0", b.Len())
	}
	if b.Counters().TotalReq != 0 || b.Counters().TotalErr != 0 {
		t.Errorf("counters after reset = %+v, want zeroed", b.Counters())
	}
	// Buffer still usable after reset.
	b.Record(&Record{ID: "y", Provider: "p", Model: "m", StatusCode: 200})
	if b.Len() != 1 {
		t.Errorf("len after reset+record = %d, want 1", b.Len())
	}
}

// Regression: a nil *Record must never reach Aggregate's dereference (it
// panicked the /metrics/live endpoint with a nil-pointer crash). The buffer
// must refuse to store nil, and Aggregate must tolerate a nil that slips in.
func TestAggregateToleratesNilRecord(t *testing.T) {
	// Aggregate over a slice containing a nil must not panic and must not count it.
	a := Aggregate([]*Record{{Provider: "p", Model: "m", StatusCode: 200}, nil, {Provider: "p"}})
	if a.Total != 2 {
		t.Errorf("Aggregate.Total = %d, want 2 (nil skipped)", a.Total)
	}

	// The buffer must never store a nil via Record or Backfill.
	b := NewBuffer(8)
	b.Record(nil)
	b.Backfill([]*Record{{ID: "ok", Provider: "p"}, nil})
	if b.Len() != 1 {
		t.Errorf("len = %d, want 1 (nil records dropped)", b.Len())
	}
	// SnapshotJSONSince(0) (the live endpoint path) must not panic on the result.
	if _, err := b.SnapshotJSONSince(0); err != nil {
		t.Fatalf("SnapshotJSONSince: %v", err)
	}
}

// Regression: RemoveWhere rebuilt the compacted ring with head=0 while size
// shrank, so every subsequent Record overwrote the OLDEST kept record and
// incremented size past the written slots - each snapshot then carried nil
// slots, /metrics/live emitted `null` records, and the dashboard crashed on
// `r.id` in buildIndexes. After a filtered purge, new records must append at
// the tail (head == size), leave every live slot non-nil, and never steal the
// oldest survivor.
func TestRemoveWhereThenRecordKeepsRingContiguous(t *testing.T) {
	b := NewBuffer(8)
	for i := 1; i <= 5; i++ {
		b.Record(&Record{ID: "r" + strconv.Itoa(i), Provider: "p", StatusCode: 200})
	}
	if removed := b.RemoveWhere(func(r *Record) bool { return r.ID == "r2" }); removed != 1 {
		t.Fatalf("RemoveWhere removed %d, want 1", removed)
	}
	for i := 6; i <= 8; i++ {
		b.Record(&Record{ID: "r" + strconv.Itoa(i), Provider: "p", StatusCode: 200})
	}

	snap := b.Snapshot()
	if len(snap) != 7 {
		t.Fatalf("len = %d, want 7", len(snap))
	}
	var ids strings.Builder
	for i, r := range snap {
		if r == nil {
			t.Fatalf("snapshot[%d] is nil (so far: %q) - a null record crashes the dashboard's buildIndexes", i, ids.String())
		}
		if i > 0 {
			ids.WriteByte(',')
		}
		ids.WriteString(r.ID)
	}
	want := "r1,r3,r4,r5,r6,r7,r8"
	if ids.String() != want {
		t.Errorf("order = %q, want %q (the bug overwrote the oldest kept records)", ids.String(), want)
	}
}

// Regression: Backfill restored history without incrementing the cumulative
// counters, but RemoveWhere subtracts every purged record from them - a
// filtered purge after a restart-backfill drove total_requests/total_errors
// NEGATIVE. Backfill must count restored records (and their error class) so
// purge arithmetic stays symmetric and the tallies never go below zero.
func TestPurgeCountersNeverNegativeAfterBackfill(t *testing.T) {
	b := NewBuffer(8)
	b.Backfill([]*Record{
		{ID: "a", Provider: "p", StatusCode: 500, ErrorType: "provider_error"},
		{ID: "b", Provider: "p", StatusCode: 200},
		{ID: "c", Provider: "p", StatusCode: 200},
	})
	if got := b.Counters(); got.TotalReq != 3 || got.TotalErr != 1 {
		t.Fatalf("counters after backfill = %+v, want TotalReq=3 TotalErr=1", got)
	}

	// Purge everything the backfill restored; the tallies must hit exactly
	// zero, never below (pre-fix this landed at TotalReq=-3, TotalErr=-1).
	if removed := b.RemoveWhere(func(r *Record) bool { return true }); removed != 3 {
		t.Fatalf("RemoveWhere removed %d, want 3", removed)
	}
	if got := b.Counters(); got.TotalReq != 0 || got.TotalErr != 0 {
		t.Errorf("counters after purge = %+v, want zeroed (never negative)", got)
	}

	// The buffer remains usable: live records still count symmetrically.
	b.Record(&Record{ID: "d", Provider: "p", Model: "m", StatusCode: 500, ErrorType: "provider_error"})
	if removed := b.RemoveWhere(func(r *Record) bool { return r.ID == "d" }); removed != 1 {
		t.Fatalf("RemoveWhere removed %d, want 1", removed)
	}
	if got := b.Counters(); got.TotalReq != 0 || got.TotalErr != 0 {
		t.Errorf("counters after live record purge = %+v, want zeroed", got)
	}
}
