package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// seedRing records n finalized records into buf. Identity is the fixture's
// only concern: the snapshot contract is about which pointers appear in which
// order, so every assertion compares the pointers, never the record contents.
func seedRing(buf *metrics.Buffer, n int) {
	for i := 0; i < n; i++ {
		buf.Record(&metrics.Record{
			ID:         fmt.Sprintf("ring-%d", i),
			Provider:   "fixture.example",
			Model:      "fixture-model",
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
			StatusCode: 200,
		})
	}
}

// TestSnapshotIntoMatchesSnapshot is the WS5a equivalence gate: the reused
// destination returns exactly Snapshot's records, in Snapshot's oldest-to-
// newest order, for an empty ring, a partial ring and a wrapped ring. It also
// pins the two properties the scratch depends on: a large enough destination
// is reused rather than reallocated, and the view is a copy (mutating it never
// touches the ring or a later snapshot).
func TestSnapshotIntoMatchesSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		maxSize int
		filled  int
	}{
		{"empty", 8, 0},
		{"partial", 8, 5},
		{"full", 8, 8},
		{"wrapped", 8, 21},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := metrics.NewBuffer(c.maxSize)
			seedRing(buf, c.filled)

			want := buf.Snapshot()
			scratch := make([]*metrics.Record, c.maxSize)
			// A shortened destination must still be reused from its capacity.
			got := buf.SnapshotInto(scratch[:1])
			if len(got) != len(want) {
				t.Fatalf("SnapshotInto length = %d, Snapshot = %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("record %d = %p, Snapshot = %p (order or identity drifted)", i, got[i], want[i])
				}
			}
			if len(want) > 0 && &got[0] != &scratch[0] {
				t.Fatal("SnapshotInto did not reuse the retained destination")
			}

			// The view is a copy: mutating it must not reach the ring, and it
			// must not leak into the next read through the reused scratch.
			if len(got) > 0 {
				got[0] = nil
				if fresh := buf.Snapshot(); fresh[0] == nil {
					t.Fatal("SnapshotInto aliased the ring: mutating the view changed the buffer")
				}
				if again := buf.SnapshotInto(scratch); again[0] == nil {
					t.Fatal("SnapshotInto left a stale mutation in the reused destination")
				}
			}
		})
	}
}

// TestRingSnapshotBorrowsNotRing pins the aggregate reader's contract after it
// started borrowing a pooled scratch: the view aliases the scratch, never the
// ring, so changing it cannot corrupt the buffer and the next read still sees
// every record. The returned scratch must be released exactly once.
func TestRingSnapshotBorrowsNotRing(t *testing.T) {
	buf := metrics.NewBuffer(64)
	seedRing(buf, 40)
	api := NewAggAPI(buf, nil, time.Second)

	firstScratch, first := api.ringSnapshot(0)
	if len(first) == 0 {
		t.Fatal("ringSnapshot returned no records")
	}
	for i, r := range first {
		if r == nil {
			t.Fatalf("record %d is nil", i)
		}
	}
	first[0] = nil // scribble over the scratch before handing it back
	releaseRing(firstScratch, first)

	secondScratch, second := api.ringSnapshot(0)
	defer releaseRing(secondScratch, second)
	if len(second) != 40 {
		t.Fatalf("second read returned %d records, want 40", len(second))
	}
	for i, r := range second {
		if r == nil {
			t.Fatalf("second read record %d is nil (stale scratch reused without a fresh copy)", i)
		}
	}
	if got := buf.Len(); got != 40 {
		t.Fatalf("ring length = %d, want 40", got)
	}
	if fresh := buf.Snapshot(); fresh[0] == nil {
		t.Fatal("ringSnapshot aliased the ring: mutating the view changed the buffer")
	}
}

// TestRingSnapshotKeepsScratchWhenAllWritten pins the empty-unwritten case: a
// ring whose records are already durable filters down to an empty view, and the
// caller still has to return the borrowed scratch to the pool. Returning a nil
// view dropped the ring-sized buffer instead (releaseRing could not see its
// capacity), so the next aggregate read allocated a fresh one.
func TestRingSnapshotKeepsScratchWhenAllWritten(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(8)
	for i := 0; i < 3; i++ {
		s.Recorder(buf).Record(&metrics.Record{
			ID:         fmt.Sprintf("durable-%d", i),
			Provider:   "fixture.example",
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
			StatusCode: 200,
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(buf, s, time.Second)

	firstScratch, first := api.ringSnapshot(0)
	if len(first) != 0 {
		t.Fatalf("ringSnapshot returned %d records, want 0: every ring record is durable", len(first))
	}
	if cap(first) < 3 {
		t.Fatalf("empty ringSnapshot view capacity = %d, want the 3-record scratch retained (the scratch was dropped)", cap(first))
	}
	releaseRing(firstScratch, first)
	// releaseRing is the pool's only return path: with a nil view it sets the
	// retained scratch to len 0 cap 0, so the next read allocates. A sync.Pool
	// may drop the entry at any GC, so this asserts the scratch release contract
	// on the pointer we own rather than pointer identity across a later Get.
	if cap(*firstScratch) < 3 {
		t.Fatalf("releaseRing dropped the scratch capacity: cap = %d, want >= 3", cap(*firstScratch))
	}
}

// TestRingSnapshotAllocationCeiling pins the pooled borrow at the 250k-slot
// probe size. ringSnapshot copies and filters the whole ring; a local
// make([]*metrics.Record, 0, 64) would grow to the ring size on every read
// (about 2 MB here) instead of reusing the released scratch.
func TestRingSnapshotAllocationCeiling(t *testing.T) {
	const slots = 250_000
	buf := metrics.NewBuffer(slots)
	const distinct = 256
	records := make([]*metrics.Record, distinct)
	for i := range records {
		records[i] = &metrics.Record{
			ID:         fmt.Sprintf("ring-slot-%d", i),
			StatusCode: 200,
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
		}
	}
	for i := 0; i < slots; i++ {
		buf.Record(records[i%distinct])
	}
	api := NewAggAPI(buf, nil, time.Second)
	// Warm the package pool with the exact ring-sized scratch.
	scratch, view := api.ringSnapshot(0)
	if len(view) != slots {
		t.Fatalf("warm ringSnapshot = %d records, want %d", len(view), slots)
	}
	releaseRing(scratch, view)
	for run := 0; run < 5; run++ {
		result := testing.Benchmark(func(b *testing.B) {
			// runN calls runtime.GC before this function, which can clear the
			// package pool. Re-warm after that boundary so the measured loop
			// starts from the reuse steady state rather than a cold miss.
			scratch, view := api.ringSnapshot(0)
			if len(view) != slots {
				b.Fatalf("warm ringSnapshot = %d records, want %d", len(view), slots)
			}
			releaseRing(scratch, view)
			b.ReportAllocs()
			for b.Loop() {
				scratch, view := api.ringSnapshot(0)
				if len(view) != slots {
					b.Fatalf("ringSnapshot = %d records, want %d", len(view), slots)
				}
				releaseRing(scratch, view)
			}
		})
		if got := result.AllocedBytesPerOp(); got > 64 || result.AllocsPerOp() != 0 {
			t.Fatalf("run %d: ringSnapshot = %d B/op %d allocs/op, want <= 64 B and 0 allocs",
				run, got, result.AllocsPerOp())
		}
	}
}

// TestSnapshotIntoAllocationCeiling is the WS5a budget: a retained scratch
// makes a ring snapshot allocation-free. At the one-million-record ceiling the
// displaced copy was 8,005,296 B per KPI fold (BenchmarkKPI/records_1000000);
// the copy is eight bytes per slot, so the per-slot ceiling enforced here is
// the same fact at a size the gate can afford. Snapshot keeps its one
// ring-sized allocation: it is the API for callers that retain the result.
func TestSnapshotIntoAllocationCeiling(t *testing.T) {
	const slots = 250_000
	buf := metrics.NewBuffer(slots)
	// Recycle a small record set: the measured cost is the pointer copy, which
	// is independent of how many distinct records the slots point at.
	const distinct = 256
	records := make([]*metrics.Record, distinct)
	for i := range records {
		records[i] = &metrics.Record{ID: fmt.Sprintf("slot-%d", i), StatusCode: 200}
	}
	for i := 0; i < slots; i++ {
		buf.Record(records[i%distinct])
	}

	scratch := make([]*metrics.Record, 0, slots)
	into := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			scratch = buf.SnapshotInto(scratch[:0])
		}
	})
	if got := into.AllocedBytesPerOp(); got > 64 {
		t.Errorf("SnapshotInto with a retained scratch = %d B/op, want <= 64 (ring-sized copy still happens)", got)
	}
	if got := into.AllocsPerOp(); got != 0 {
		t.Errorf("SnapshotInto with a retained scratch = %d allocs/op, want 0", got)
	}

	plain := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = buf.Snapshot()
		}
	})
	if got := plain.AllocedBytesPerOp(); got < slots*8 {
		t.Errorf("Snapshot = %d B/op, want >= %d: the reference copy no longer allocates per slot", got, slots*8)
	}
	if into.AllocedBytesPerOp()*1000 > plain.AllocedBytesPerOp() {
		t.Errorf("SnapshotInto = %d B/op is not at least 1000x cheaper than Snapshot = %d B/op",
			into.AllocedBytesPerOp(), plain.AllocedBytesPerOp())
	}
	t.Logf("slots=%d: SnapshotInto %d B/op %d allocs/op, Snapshot %d B/op (8 B/slot at 1M would be 8,000,000)",
		slots, into.AllocedBytesPerOp(), into.AllocsPerOp(), plain.AllocedBytesPerOp())
}

// TestKPIFoldReusesRingScratch models the production shape the ring copy
// actually costs: ONE AggAPI serving repeated cold folds because every live
// record advances the memo key (a memo hit is not reachable under traffic),
// at the documented one-million-record ceiling. The retained scratch means the
// fold's ring-sized allocation happens once, not per poll.
// BenchmarkKPI's "miss" case constructs a fresh API per iteration and therefore
// measures a rebuilt API, not the live poll.
func TestKPIFoldReusesRingScratch(t *testing.T) {
	const slots = 1_000_000
	buf := metrics.NewBuffer(slots)
	// Recycle a small record set: the fold adds every slot, so the ring size is
	// what is measured without retaining a million Record values.
	const distinct = 1024
	pool := make([]*metrics.Record, distinct)
	for i := range pool {
		pool[i] = &metrics.Record{
			ID:         fmt.Sprintf("kpi-%d", i),
			Provider:   "fixture.example",
			Model:      "fixture-model",
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
			StatusCode: 200,
			Cost:       0.0001,
			Usage:      metrics.Usage{InputTokens: 100, OutputTokens: 50},
		}
	}
	for i := 0; i < slots; i++ {
		buf.Record(pool[i%distinct])
	}

	api := NewAggAPI(buf, nil, time.Second)
	// One warm fold at full ring size allocates the scratch and publishes the
	// memo entry; steady state must not allocate it again.
	_ = api.kpi()

	next := 0
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			// Advance the finalized revision so every fold is cold, exactly as
			// a live record does between two dashboard polls.
			buf.Record(&metrics.Record{ID: fmt.Sprintf("live-%d", next), StatusCode: 200})
			next++
			_ = api.kpi()
		}
	})
	// The displaced ring copy alone was slots*8 = 8,000,000 B/op; the
	// surviving work is one memo entry per fold.
	if got := res.AllocedBytesPerOp(); got > 64*1024 {
		t.Errorf("repeated cold KPI fold at %d slots = %d B/op, want <= 65536 (ring-sized scratch reallocated per fold)", slots, got)
	}
	t.Logf("repeated cold KPI fold at %d slots: %d B/op %d allocs/op", slots, res.AllocedBytesPerOp(), res.AllocsPerOp())
}

// TestKPIFoldReleasesRetainedScratchRecords pins the ownership boundary of the
// retained scratch: the memo keeps the backing array for reuse but must not
// keep any record pointer after a fold. Buffer.Reset retires the ring's own
// references, so a scratch that still held them would pin every record and its
// strings for the life of the process.
func TestKPIFoldReleasesRetainedScratchRecords(t *testing.T) {
	buf := metrics.NewBuffer(16)
	seed := func(count int) {
		for i := 0; i < count; i++ {
			buf.Record(&metrics.Record{
				ID:         fmt.Sprintf("scratch-%d-%d", count, i),
				Provider:   "fixture.example",
				Model:      "fixture-model",
				Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
				StatusCode: 200,
			})
		}
	}

	api := NewAggAPI(buf, nil, time.Second)
	seed(6)
	_ = api.kpi()
	retained := cap(api.kpiMemo.ring)
	if retained < 6 {
		t.Fatalf("KPI scratch capacity = %d, want >= 6", retained)
	}
	for index, record := range api.kpiMemo.ring[:retained] {
		if record != nil {
			t.Fatalf("after fold, retained KPI scratch slot %d still pins %q", index, record.ID)
		}
	}

	buf.Reset()
	seed(2)
	_ = api.kpi()
	if got := cap(api.kpiMemo.ring); got != retained {
		t.Fatalf("KPI scratch capacity after reset = %d, want retained %d", got, retained)
	}
	for index, record := range api.kpiMemo.ring[:retained] {
		if record != nil {
			t.Fatalf("after reset and fold, retained KPI scratch slot %d still pins %q", index, record.ID)
		}
	}
}
