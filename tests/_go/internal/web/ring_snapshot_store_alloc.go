package web

// Round 2 D3 regression: with a durable store attached, ringSnapshot built a
// fresh ring-sized []string for every read (about 16 MB at one million slots).
// The id scratch is pooled separately, so a steady-state read allocates zero.

import (
	"fmt"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func TestRingSnapshotStoreAllocationCeiling(t *testing.T) {
	if raceInstrumented {
		// sync.Pool.Put drops one in four items when the race detector is
		// enabled (src/sync/pool.go), so a pool-backed read cannot assert a
		// zero-allocation steady state under -race. The plain pass owns this
		// ceiling; race coverage of the aggregate lives in the other tests.
		t.Skip("race builds drop one in four sync.Pool puts by design")
	}
	const slots = 100_000
	buf := metrics.NewBuffer(slots)
	ids := make([]string, slots)
	for i := 0; i < slots; i++ {
		record := &metrics.Record{
			ID:         fmt.Sprintf("ring-store-%d", i),
			StatusCode: 200,
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
		}
		buf.Record(record)
		ids[i] = record.ID
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "ring-store.db"), storage.Options{
		WriteChanCap: 8, BatchCap: 8, FlushInterval: time.Hour,
		QueryTimeout: time.Second, WriteTrackCap: slots,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	store.MarkWritten(ids)
	api := NewAggAPI(buf, store, time.Second)
	// Warm both the record and id pools.
	scratch, view := api.ringSnapshot(0)
	if len(view) != 0 {
		t.Fatalf("warm ringSnapshot returned %d written records, want none", len(view))
	}
	releaseRing(scratch, view)
	// sync.Pool drops both scratches at GC. Pin reuse for the measurement; a
	// real per-read allocation still counts.
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	result := testing.Benchmark(func(b *testing.B) {
		scratch, view := api.ringSnapshot(0)
		if len(view) != 0 {
			b.Fatalf("warm ringSnapshot returned %d written records, want none", len(view))
		}
		releaseRing(scratch, view)
		b.ReportAllocs()
		for b.Loop() {
			scratch, view := api.ringSnapshot(0)
			if len(view) != 0 {
				b.Fatalf("ringSnapshot returned %d written records, want none", len(view))
			}
			releaseRing(scratch, view)
		}
	})
	bytes, allocs := result.AllocedBytesPerOp(), result.AllocsPerOp()
	t.Logf("store ringSnapshot = %d B/op %d allocs/op (N=%d)", bytes, allocs, result.N)
	if allocs != 0 {
		t.Fatalf("store ringSnapshot = %d allocs/op, want 0", allocs)
	}
}
