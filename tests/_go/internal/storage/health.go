package storage

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// wedgedStore returns a store whose writer is blocked inside a batch commit,
// with an hourly flush so nothing recovers on its own: one record is in flight,
// one fills the one-slot queue and the rest overflow. The caller closes the
// held connection to release the writer.
func wedgedStore(t *testing.T) (*Store, metrics.Recorder, *sql.Conn) {
	t.Helper()
	opts := testOpts
	opts.WriteChanCap = 1
	opts.BatchCap = 1
	opts.FlushInterval = time.Hour
	s := openPurgeTestStore(t, opts)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup runs LIFO ahead of the store's own Close, so a fatal on any
	// assertion cannot strand the writer on the reserved connection.
	t.Cleanup(func() { conn.Close() })
	rec := s.Recorder(metrics.NewBuffer(8))
	before := s.db.Stats().WaitCount
	// The first record is picked up and blocks in insertBatch on the held
	// connection; the second waits in the one-slot channel.
	rec.Record(&metrics.Record{ID: "inflight", Start: time.Now()})
	waitPurgeCondition(t, func() bool { return s.db.Stats().WaitCount > before })
	rec.Record(&metrics.Record{ID: "queued", Start: time.Now()})
	// One cadence plus the window is the conviction age, so backdate past it
	// explicitly instead of waiting an hour.
	s.health.lastFlushUnixNano.Store(time.Now().Add(-5 * time.Hour).UnixNano())
	return s, rec, conn
}

// A multi-hour flush interval must not postpone the wedge verdict by a
// multiple of that interval: the window tail stays clamped, so one cadence plus
// three minutes already convicts instead of one cadence plus sixteen.
func TestHealthClampsTheWedgeWindowAgainstALongFlushInterval(t *testing.T) {
	opts := testOpts
	opts.WriteChanCap = 1
	opts.BatchCap = 1
	opts.FlushInterval = 4 * time.Hour
	s := openPurgeTestStore(t, opts)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	rec := s.Recorder(metrics.NewBuffer(8))
	before := s.db.Stats().WaitCount
	rec.Record(&metrics.Record{ID: "inflight", Start: time.Now()})
	waitPurgeCondition(t, func() bool { return s.db.Stats().WaitCount > before })
	rec.Record(&metrics.Record{ID: "queued", Start: time.Now()})
	// One cadence and one clamped-window minute: still inside.
	s.health.lastFlushUnixNano.Store(time.Now().Add(-(4*time.Hour + time.Minute)).UnixNano())
	if h := s.Health(); h.Wedged {
		t.Fatalf("Health().Wedged = true one cadence and a minute after the last flush (depth %d)", h.QueueDepth)
	}
	// One cadence plus the full clamped window: convicted, even though four
	// unclamped intervals would have been sixteen hours.
	s.health.lastFlushUnixNano.Store(time.Now().Add(-(4*time.Hour + 3*time.Minute)).UnixNano())

	if h := s.Health(); !h.Wedged {
		t.Fatalf("Health().Wedged = false with %d queued after a 4h interval and a cadence-plus-window old flush", h.QueueDepth)
	}
	if ready, reason := s.Ready(); ready {
		t.Fatalf("Ready() = true, want a wedge refusal, got %q", reason)
	}
}

// TestHealthAllowsOneCadenceBeforeConvicting pins CR-3: the window clamp
// convicted a record that was only waiting for its own flush tick, so with the
// documented hourly storage_flush_interval readiness was red for almost the
// whole cycle. A record inside one cadence plus the window never convicts.
func TestHealthAllowsOneCadenceBeforeConvicting(t *testing.T) {
	opts := testOpts
	opts.FlushInterval = time.Hour
	s := openPurgeTestStore(t, opts)
	s.Record(&metrics.Record{ID: "queued", Start: time.Now(), StatusCode: 200})
	waitPurgeCondition(t, func() bool { return s.health.inFlight.Load() == 1 })
	// Fifty minutes since the last commit is far outside the two-minute clamp
	// but inside the record's own cadence, so it is not yet a wedge.
	s.health.lastFlushUnixNano.Store(time.Now().Add(-50 * time.Minute).UnixNano())
	if h := s.Health(); h.Wedged {
		t.Fatalf("Health().Wedged = true with one record waiting inside its cadence (depth %d, last flush %v)", h.QueueDepth, h.LastFlush)
	}
	if ready, reason := s.Ready(); !ready {
		t.Fatalf("Ready() = false inside the flush cadence: %q", reason)
	}
	// One cadence plus the progress window later the ticker should have taken
	// it, so the same record convicts.
	s.health.oldestQueuedUnixNano.Store(time.Now().Add(-(time.Hour + 3*time.Minute)).UnixNano())
	if h := s.Health(); !h.Wedged {
		t.Fatalf("Health().Wedged = false with a record older than a cadence and the window (depth %d)", h.QueueDepth)
	}
	if ready, _ := s.Ready(); ready {
		t.Fatal("Ready() = true with a record older than a cadence and the window")
	}
}

// The queue clock must start when the record arrives, not only when the writer
// picks it up: a writer that dies before its first commit still has to convict.
func TestHealthQueueClockStartsOnArrival(t *testing.T) {
	opts := testOpts
	opts.FlushInterval = time.Hour
	s := openPurgeTestStore(t, opts)
	if ns := s.health.oldestQueuedUnixNano.Load(); ns != 0 {
		t.Fatalf("queue clock started before any record: %v", time.Unix(0, ns))
	}
	s.Record(&metrics.Record{ID: "queued", Start: time.Now(), StatusCode: 200})
	if ns := s.health.oldestQueuedUnixNano.Load(); ns == 0 {
		t.Fatal("arrival did not start the queue clock")
	}
	waitPurgeCondition(t, func() bool { return s.health.inFlight.Load() == 1 })
	// Nothing was ever committed, so only the record's own age can convict.
	s.health.lastFlushUnixNano.Store(0)
	s.health.oldestQueuedUnixNano.Store(time.Now().Add(-(time.Hour + 3*time.Minute)).UnixNano())
	if h := s.Health(); !h.Wedged {
		t.Fatalf("Health().Wedged = false for an unpicked record past its cadence (depth %d outstanding %d last flush %v)", h.QueueDepth, h.Outstanding, h.LastFlush)
	}
}

// A writer that keeps committing must not hide a record it never picks up: the
// oldest queued record ages on its own, independent of the last flush.
func TestHealthWedgesOnTheOldestQueuedRecord(t *testing.T) {
	s, rec, conn := wedgedStore(t)
	// A commit just succeeded, so only the record's own age can convict; the
	// store runs an hourly cadence, so the age must pass a cadence and the
	// window.
	s.health.lastFlushUnixNano.Store(time.Now().UnixNano())
	s.health.oldestQueuedUnixNano.Store(time.Now().Add(-(time.Hour + 3*time.Minute)).UnixNano())
	if h := s.Health(); !h.Wedged {
		t.Fatalf("Health().Wedged = false with %d queued after a fresh flush", h.QueueDepth)
	}

	// Release the writer: the drained queue must clear the age clock again.
	rec.Record(&metrics.Record{ID: "queued-2", Start: time.Now()})
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ready, reason := s.Ready(); ready {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("store stayed unready after the writer drained: %s", reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ns := s.health.oldestQueuedUnixNano.Load(); ns != 0 {
		h := s.Health()
		t.Fatalf("oldest queued stamp survived the drain: %v (depth %d, last flush %v, failed %d)", time.Unix(0, ns), h.QueueDepth, h.LastFlush, h.FailedFlushes)
	}
}

func TestHealthCountsDropsByCause(t *testing.T) {
	s, rec, conn := wedgedStore(t)
	// Overflow: the queue is full, so this one and the next are refused.
	rec.Record(&metrics.Record{ID: "overflow-1", Start: time.Now()})
	rec.Record(&metrics.Record{ID: "overflow-2", Start: time.Now()})

	h := s.Health()
	if got, want := h.Drops[DropOverflow], uint64(2); got != want {
		t.Fatalf("overflow drops = %d, want %d (all causes %v)", got, want, h.Drops)
	}
	if h.Drops[DropAfterClose] != 0 || h.Drops[DropPanic] != 0 || h.Drops[DropFlushFailed] != 0 {
		t.Fatalf("unrelated causes counted: %v", h.Drops)
	}
	// The cause split must not change the total the dashboard reads.
	if got, want := s.Dropped(), uint64(2); got != want {
		t.Fatalf("Dropped() = %d, want the same %d", got, want)
	}

	// Drain, so Close costs no budget timeout, then record after it.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Health().QueueDepth > 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer did not drain the queue")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A post-Close arrival lands in its own bucket, not in overflow.
	rec.Record(&metrics.Record{ID: "late", Start: time.Now()})
	h = s.Health()
	if got, want := h.Drops[DropAfterClose], uint64(1); got != want {
		t.Fatalf("after-close drops = %d, want %d (all causes %v)", got, want, h.Drops)
	}
	if got, want := h.Drops[DropOverflow], uint64(2); got != want {
		t.Fatalf("overflow drops moved to %d, want %d", got, want)
	}
}

func TestReadyFailsWhileWedgedAndClearsAfterAFlush(t *testing.T) {
	s, rec, conn := wedgedStore(t)
	h := s.Health()
	if !h.Wedged {
		t.Fatalf("Health().Wedged = false with %d queued and a stale flush (%v)", h.QueueDepth, h.LastFlush)
	}
	if ready, reason := s.Ready(); ready || !strings.Contains(reason, "2 durable record(s) outstanding (1 queued of 1)") {
		t.Fatalf("Ready() = %t, %q, want a wedge refusal naming the outstanding records", ready, reason)
	}
	// The exposition carries the same verdict.
	var buf bytes.Buffer
	s.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), "llm_proxy_storage_wedged 1\n") {
		t.Fatalf("exposition did not report the wedge:\n%s", buf.String())
	}

	// Release the writer: the queued record commits and readiness clears.
	rec.Record(&metrics.Record{ID: "queued-2", Start: time.Now()})
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ready, reason := s.Ready(); ready {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("store stayed unready after the writer drained: %s", reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h := s.Health(); h.Wedged || h.FailedFlushes != 0 || h.LastFlush.IsZero() {
		t.Fatalf("health after recovery = %+v, want a fresh successful flush", h)
	}
}

func TestStorageHealthRendersEveryCauseBeforeAnyDrop(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	var buf bytes.Buffer
	s.WritePrometheus(&buf)
	out := buf.String()
	for _, cause := range dropCauses {
		if !strings.Contains(out, `llm_proxy_storage_dropped_total{cause="`+cause+`"} 0`+"\n") {
			t.Errorf("missing zero series for %s:\n%s", cause, out)
		}
	}
	for _, want := range []string{
		"llm_proxy_storage_enabled 1\n",
		"llm_proxy_storage_queue_capacity ",
		"llm_proxy_storage_queue_depth 0\n",
		"llm_proxy_storage_flush_failed_total 0\n",
		"llm_proxy_storage_wedged 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// A disabled store is ready and still renders one document with enabled=0, so
// a scrape and a probe never depend on the storage toggle.
func TestNilStoreHealthAndExposition(t *testing.T) {
	var s *Store
	if ready, reason := s.Ready(); !ready || reason != "" {
		t.Fatalf("nil store Ready() = %t, %q, want ready", ready, reason)
	}
	if h := s.Health(); h.Enabled || h.Wedged || h.QueueDepth != 0 {
		t.Fatalf("nil store Health() = %+v, want a disabled zero answer", h)
	}
	var buf bytes.Buffer
	s.WritePrometheus(&buf)
	if got, want := buf.String(), "llm_proxy_storage_enabled 0\n"; !strings.HasPrefix(got, "# HELP llm_proxy_storage_enabled") || !strings.Contains(got, want) {
		t.Fatalf("nil store exposition = %q", got)
	}
	if strings.Contains(buf.String(), "llm_proxy_storage_queue_depth") {
		t.Fatalf("disabled store rendered live pipeline series:\n%s", buf.String())
	}
}
