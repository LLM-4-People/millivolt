package web

import (
	"context"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// openRestartStore opens one process's view of a fixed database path. The
// restart test owns the lifetime explicitly because it closes and reopens the
// same file.
func openRestartStore(t *testing.T, path string) *storage.Store {
	t.Helper()
	d := config.Default()
	s, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond,
		QueryTimeout:  30 * time.Second,
		WriteTrackCap: 128,
	})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return s
}

// TestKPIUnwrittenRingExactAcrossRestart pins the conservative source of
// "possibly not yet durable" that the KPI fold merges against the durable
// totals: the ring is the candidate universe and the written-id set is the
// exact dedupe. The test mirrors production boot (LoadRecent -> Backfill ->
// MarkWritten) and then asserts the KPI equals durable-plus-ring-extras
// exactly for a ring-only record, a durable-only record, a record on both
// sides, and a record purged between polls - no double count, no drop.
//
// Dropped and failed writes are the reason this must stay ring-derived: a
// record whose enqueue overflowed (store.record's default branch) or whose
// batch insert failed (flush) is never durable, never in the written set and
// never in the writer queue, but it IS in the ring and must be counted.
func TestKPIUnwrittenRingExactAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	now := time.Now()
	ctx := context.Background()

	// --- first process: two survivors and one record purged durably. ---
	first := openRestartStore(t, path)
	first.Record(mkRec("durable-a", now.Add(-4*time.Hour), 200, "", nil, 10, 100, 10, 5, 0, 0.001))
	first.Record(mkRec("durable-b", now.Add(-3*time.Hour), 200, "", nil, 20, 100, 20, 6, 0, 0.002))
	purgedEarly := mkRec("purged-early", now.Add(-2*time.Hour), 200, "", nil, 30, 100, 100, 100, 0, 1)
	purgedEarly.Client = "purge-me"
	first.Record(purgedEarly)
	waitTotals(t, first, 3)
	if _, err := first.Clear(ctx, &storage.PurgeFilter{Client: "purge-me"}, nil); err != nil {
		t.Fatalf("purge before restart: %v", err)
	}
	if got := first.Totals().Requests; got != 2 {
		t.Fatalf("totals after purge = %d, want 2", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// --- restart: backfill the ring and prime the written set exactly as
	// cmd/proxy does, then keep the store live for new traffic. ---
	second := openRestartStore(t, path)
	t.Cleanup(func() { _ = second.Close() })
	backfilled, err := second.LoadRecent(ctx, 100)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	buf := metrics.NewBuffer(64)
	buf.Backfill(backfilled)
	if len(backfilled) != 2 {
		t.Fatalf("backfilled %d records, want 2", len(backfilled))
	}
	ids := make([]string, 0, len(backfilled))
	for _, r := range backfilled {
		ids = append(ids, r.ID)
	}
	second.MarkWritten(ids)
	rec := second.Recorder(buf) // production fan-out: ring plus durable
	api := NewAggAPI(buf, second, time.Second)
	handler := http.HandlerFunc(api.HandleBootstrap)

	// Nothing is unwritten yet: the backfilled ring must dedupe exactly.
	p := getKPI(t, handler, "/metrics/bootstrap")
	if p["requests"] != float64(2) {
		t.Fatalf("after backfill: requests = %v, want 2 (no double count)", p["requests"])
	}
	if p["input_tokens"] != float64(30) {
		t.Fatalf("after backfill: input = %v, want 30", p["input_tokens"])
	}

	// One record on each side of the merge plus one on both.
	buf.Record(mkRec("ring-only", now.Add(-time.Minute), 200, "", nil, 1, 100, 1, 1, 0, 0.0001))
	second.Record(mkRec("durable-only", now.Add(-2*time.Minute), 200, "", nil, 2, 100, 2, 2, 0, 0.0002))
	waitTotals(t, second, 3)
	rec.Record(mkRec("both", now, 200, "", nil, 3, 100, 3, 3, 0, 0.0003))
	waitTotals(t, second, 4)

	p = getKPI(t, handler, "/metrics/bootstrap")
	if p["requests"] != float64(5) { // durable-a, durable-b, durable-only, both, ring-only
		t.Fatalf("mixed merge: requests = %v, want 5", p["requests"])
	}
	if p["input_tokens"] != float64(36) {
		t.Fatalf("mixed merge: input = %v, want 36", p["input_tokens"])
	}
	if p["output_tokens"] != float64(17) {
		t.Fatalf("mixed merge: output = %v, want 17", p["output_tokens"])
	}

	// A record that is durable and in the ring, purged between polls: both
	// sides must lose it and nothing may resurrect it from the written set.
	livePurged := mkRec("purged-live", now.Add(-30*time.Second), 200, "", nil, 4, 100, 500, 500, 0, 5)
	livePurged.Client = "purge-me"
	rec.Record(livePurged)
	waitTotals(t, second, 5)
	res, err := second.Clear(ctx, &storage.PurgeFilter{Client: "purge-me"}, buf)
	if err != nil {
		t.Fatalf("purge between polls: %v", err)
	}
	if res.Deleted != 1 || res.BufferRemoved != 1 {
		t.Fatalf("purge result = %+v, want one durable and one ring removal", res)
	}
	if got := second.Totals().Requests; got != 4 {
		t.Fatalf("durable totals after purge = %d, want 4", got)
	}

	want := struct{ requests, in, out float64 }{5, 36, 17}
	for poll := 0; poll < 2; poll++ {
		p = getKPI(t, handler, "/metrics/bootstrap")
		if p["requests"] != want.requests {
			t.Fatalf("poll %d: requests = %v, want %v (ring-only counted once, purged record gone)",
				poll, p["requests"], want.requests)
		}
		if p["input_tokens"] != want.in || p["output_tokens"] != want.out {
			t.Fatalf("poll %d: input/output = %v/%v, want %v/%v",
				poll, p["input_tokens"], p["output_tokens"], want.in, want.out)
		}
		if got := p["cost"].(float64); math.Abs(got-0.0036) > 1e-12 {
			t.Fatalf("poll %d: cost = %v, want 0.0036 (never the purged 5.0)", poll, got)
		}
	}
}
