package metrics

import (
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Use an independent logical history to check every possible cursor, including
// gaps left by purge, both sides of a physical wrap, and the tip's empty delta.
func TestSnapshotSinceWrappedRangesAndGaps(t *testing.T) {
	const capacity = 7
	b := NewBuffer(capacity)
	type entry struct {
		seq int64
		rec *Record
	}
	var history []entry
	var next int64
	appendRecords := func(n int, backfill bool) {
		t.Helper()
		var batch []*Record
		for range n {
			next++
			r := &Record{ID: strconv.FormatInt(next, 10), StatusCode: 200}
			batch = append(batch, r)
			history = append(history, entry{next, r})
			if len(history) > capacity {
				history = history[1:]
			}
		}
		if backfill {
			b.Backfill(batch)
		} else {
			for _, r := range batch {
				b.Record(r)
			}
		}
	}
	check := func(stage string) {
		t.Helper()
		var oldest, latest int64
		all := make([]*Record, len(history))
		for i, e := range history {
			all[i] = e.rec
		}
		if len(history) > 0 {
			oldest, latest = history[0].seq, history[len(history)-1].seq
		}
		for _, limit := range []int{0, 1, 3, capacity + 1} {
			b.SetSnapshotLimit(limit)
			if got := b.Snapshot(); got == nil || !slices.Equal(got, all) {
				t.Fatalf("%s limit %d: uncapped snapshot IDs %v, want %v", stage, limit, ids(got), ids(all))
			}
			for since := int64(-1); since <= next+1; since++ {
				full := since <= 0 || since < oldest || since > latest
				want := all
				if full {
					if limit > 0 && len(want) > limit {
						want = want[len(want)-limit:]
					}
				} else {
					want = nil
					for _, e := range history {
						if e.seq > since {
							want = append(want, e.rec)
						}
					}
				}
				got := b.SnapshotSince(since)
				if got.Records == nil || !slices.Equal(got.Records, want) || got.Incremental != !full ||
					got.BufferSize != len(history) || got.OldestSeq != oldest || got.Seq != latest || got.FeedID != b.FeedID() {
					t.Fatalf("%s limit %d since %d: IDs %v, metadata %+v; want IDs %v, full %v, oldest %d, latest %d",
						stage, limit, since, ids(got.Records), got, ids(want), full, oldest, latest)
				}
				// The returned window must not alias the ring, even when it fits
				// entirely within one physical half of the ring.
				if len(got.Records) > 0 {
					got.Records[0] = nil
					if again := b.SnapshotSince(since); !slices.Equal(again.Records, want) {
						t.Fatalf("%s: caller mutation changed ring", stage)
					}
				}
			}
		}
	}
	check("empty")
	appendRecords(4, true)
	check("partial backfill")
	appendRecords(9, false)
	check("wrapped ring")
	match := func(r *Record) bool {
		n, _ := strconv.Atoi(r.ID)
		return n%3 == 0
	}
	removed := b.RemoveWhere(match)
	before := len(history)
	history = slices.DeleteFunc(history, func(e entry) bool { return match(e.rec) })
	if removed != before-len(history) {
		t.Fatalf("removed %d records, want %d", removed, before-len(history))
	}
	check("purged sequence gaps")
	appendRecords(2, false)
	check("full with sequence gaps")
	appendRecords(5, true)
	check("rewrapped backfill")
	b.Reset()
	history = nil
	check("reset")
	appendRecords(3, false)
	check("after reset")
	b.RemoveWhere(func(*Record) bool { return true })
	history = nil
	check("purged all")
	appendRecords(2, true)
	check("backfill after purge")
}

func TestPendingSnapshotOrderingAndLifecycleIsolation(t *testing.T) {
	b := NewBuffer(2)
	start := time.Unix(100, 0)
	for _, r := range []*Record{
		{ID: "later", Start: start.Add(time.Second)},
		{ID: "tie-b", Start: start},
		{ID: "tie-a", Start: start},
	} {
		b.PublishLive("begin", r)
	}
	want := []string{"tie-a", "tie-b", "later"}
	for _, got := range [][]*Record{b.PendingRecords(), b.SnapshotSince(0).InFlightRecords} {
		if !slices.Equal(ids(got), want) {
			t.Fatalf("pending IDs %v, want %v", ids(got), want)
		}
		got[0] = nil // returned slices do not alias the pending registry
	}
	old := b.SnapshotSince(0)
	r := &Record{ID: "tie-a", Start: start, Attempts: make([]RetryAttempt, 1, 2)}
	r.Attempts[0].StatusCode = 500
	b.PublishLive("update", r)
	updated := b.PendingRecords()
	r.Attempts[0].StatusCode = 503
	r.Attempts = append(r.Attempts, RetryAttempt{StatusCode: 502})
	if len(old.InFlightRecords[0].Attempts) != 0 || len(updated[0].Attempts) != 1 || updated[0].Attempts[0].StatusCode != 500 {
		t.Fatal("begin/update snapshots changed after publishing a later lifecycle state")
	}
	// Final recording and lifecycle end are one transition. A snapshot must
	// never carry a just-finalized ID in the pending registry.
	b.Record(r)
	between := b.SnapshotSince(0)
	if len(between.Records) != 1 || between.Records[0].ID != r.ID || !slices.Equal(ids(between.InFlightRecords), want[1:]) {
		t.Fatal("finalization did not atomically retire pending")
	}
	if got := ids(b.PendingRecords()); !slices.Equal(got, want[1:]) {
		t.Fatalf("pending IDs after end %v, want %v", got, want[1:])
	}
	if !slices.Equal(ids(old.InFlightRecords), want) || !slices.Equal(ids(between.InFlightRecords), want[1:]) {
		t.Fatal("end changed an already captured snapshot")
	}
	var nilBuffer *Buffer
	if nilBuffer.PendingRecords() != nil {
		t.Fatal("nil buffer must have no pending records")
	}
}

func TestSnapshotsConcurrentLifecycle(t *testing.T) {
	b := NewBuffer(17)
	b.SetSnapshotLimit(3)
	// Keep multiple requests pending so every reader sorts a real collection
	// while the writer replaces and removes other registry entries.
	anchors := []*Record{
		{ID: "anchor-b", Start: time.Unix(3, 0)},
		{ID: "anchor-a", Start: time.Unix(3, 0)},
		{ID: "anchor-older", Start: time.Unix(-1, 0)},
	}
	for _, r := range anchors {
		b.PublishLive("begin", r)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 500 {
			r := &Record{ID: strconv.Itoa(i), Start: time.Unix(int64(i%11), 0), StatusCode: 200}
			b.PublishLive("begin", r)
			r.Attempts = []RetryAttempt{{StatusCode: 500}}
			b.PublishLive("update", r)
			b.Record(r)
		}
	})
	for range 3 {
		wg.Go(func() {
			var cursor int64
			for range 500 {
				snapshot := b.SnapshotSince(cursor)
				if snapshot.Counters.InFlight != int64(len(snapshot.InFlightRecords)) {
					t.Errorf("snapshot gauge %d != pending count %d", snapshot.Counters.InFlight, len(snapshot.InFlightRecords))
					return
				}
				cursor = snapshot.Seq
				for _, pending := range [][]*Record{snapshot.InFlightRecords, b.PendingRecords()} {
					for i, r := range pending {
						if r == nil || (i > 0 && (r.Start.Before(pending[i-1].Start) ||
							(r.Start.Equal(pending[i-1].Start) && r.ID < pending[i-1].ID))) {
							t.Error("concurrent pending snapshot is not ordered")
							return
						}
					}
				}
			}
		})
	}
	wg.Wait()
	for _, r := range anchors {
		b.Record(r)
	}
	if got := b.Counters(); got.InFlight != 0 || got.TotalReq != 500+int64(len(anchors)) {
		t.Fatalf("lifecycle counters after snapshots: %+v", got)
	}
	if len(b.PendingRecords()) != 0 {
		t.Fatal("ended requests remained in the pending registry")
	}
}

var benchmarkBufferSnapshot Snapshot

// The default-size ring is deliberately wrapped at a nonzero physical slot.
// Dashboard boot needs only 480 rows; a poll usually needs none or a few.
func BenchmarkBufferSnapshot(b *testing.B) {
	const historySize = 10_000
	for _, tc := range []struct {
		name  string
		delta int64
	}{
		{"Full480From10K", -1},
		{"EmptyDelta", 0},
		{"SmallDelta8", 8},
	} {
		b.Run(tc.name, func(b *testing.B) {
			buf := NewBuffer(historySize)
			for i := range historySize + 137 {
				buf.Record(&Record{ID: strconv.Itoa(i), StatusCode: 200})
			}
			buf.SetSnapshotLimit(480)
			since := buf.SnapshotSince(0).Seq - tc.delta
			if tc.delta < 0 {
				since = 0
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkBufferSnapshot = buf.SnapshotSince(since)
			}
		})
	}
}
