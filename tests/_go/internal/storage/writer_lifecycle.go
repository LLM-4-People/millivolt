package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestStoreNeverBlocksOnOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	// A tiny channel cap overflows after a handful of records, so the
	// nonblocking guarantee is exercised with a small fixture instead of
	// pushing past the production 8192 cap.
	opts := testOpts
	opts.WriteChanCap = 8
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rec := &metrics.Record{ID: "x", Provider: "p", Model: "m"}
	for i := 0; i < 200; i++ {
		s.Record(rec)
	}
}

func TestRecordAfterCloseDoesNotPanic(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Recording after Close must not panic; it's counted as a drop.
	s.Record(&metrics.Record{ID: "late", Provider: "p", Model: "m"})
	if s.Dropped() == 0 {
		t.Errorf("Dropped = 0, want >= 1 for a record sent after Close")
	}
}

// TestBusyTimeoutAbsorbsConcurrentWriter is the regression for the
// restart-handoff straggler window: a second writer briefly holding the
// SQLite write lock (the draining parent committing a late batch while the
// fresh child's Open runs schema/migrate on the same file) must be ABSORBED
// by a bounded busy_timeout, not fail immediately with SQLITE_BUSY. It also
// pins the guardrail's honesty: a lock held PAST the timeout still fails
// closed (the batch drops and is counted), so busy_timeout can never mask a
// permanently stuck writer.
func TestBusyTimeoutAbsorbsConcurrentWriter(t *testing.T) {
	path := testDatabase(t)
	// Bootstrap the file the way the draining parent leaves it (schema + WAL).
	boot, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	boot.Close()

	// The straggler writer: a raw connection holding the SQLite write lock
	// (BEGIN IMMEDIATE + a dirty INSERT, not committed).
	hold, err := sql.Open("sqlite", path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	acquire := func() *sql.Tx {
		t.Helper()
		tx, err := hold.Begin() // BEGIN IMMEDIATE via _txlock
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('hold', '1')`); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	opts := testOpts
	opts.FlushInterval = 20 * time.Millisecond // flush attempts hit the held lock deterministically
	// A shortened busy window (the production 5s guardrail would cost 5s
	// per phase): 250ms is far above the ms-scale lock holds below while
	// keeping the honor check meaningful at its rescaled bound.
	opts.BusyTimeout = 250 * time.Millisecond
	busy := opts.BusyTimeout

	// Phase 1: Open (schema/migrate) must survive a held write lock. The
	// store opens in a goroutine because the busy wait blocks inside Open.
	// The 200ms hold has no pollable "Open is now waiting" signal, so it
	// stays a fixed window: it must outlast Open's ms-scale startup (a
	// shorter hold could let the child open after the rollback and pass
	// this phase vacuously) while staying inside the busy window.
	tx := acquire()
	type openRes struct {
		s   *Store
		err error
	}
	opened := make(chan openRes, 1)
	go func() {
		s, err := Open(path, opts)
		opened <- openRes{s, err}
	}()
	time.Sleep(200 * time.Millisecond) // lock stays held while the child opens
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var res openRes
	select {
	case res = <-opened:
	case <-time.After(busy + 5*time.Second):
		t.Fatal("Open never returned after the lock was released")
	}
	if res.err != nil {
		t.Fatalf("open under a held write lock: %v", res.err)
	}
	s := res.s
	defer s.Close()

	// Phase 2: a Record flushed WHILE the lock is held must commit once
	// the lock frees, with zero durable-loss drops.
	rec := &metrics.Record{ID: "straggler-1", Provider: "p", Model: "m",
		KeyHash: "k", StatusCode: 200, Start: time.Now()}
	metrics.FinalizeRecord(rec)
	tx = acquire()
	s.Record(rec)
	// Four flush windows at the 20ms cadence: the writer's flush attempt
	// starts within one interval of the Record and is still parked on the
	// held lock when the rollback lands 80ms in.
	time.Sleep(80 * time.Millisecond) // a flush attempt fires against the held lock
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		recs, err := s.LoadRecent(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range recs {
			if r.ID == "straggler-1" {
				found = true
			}
		}
		if found && s.Dropped() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record not durable after lock release: found=%v dropped=%d (want committed with 0 drops)", found, s.Dropped())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Phase 3: a lock held PAST the timeout must still fail closed - the
	// batch drops and is counted; busy_timeout delays SQLITE_BUSY, never
	// masks it. The failed batch is durable loss by design: the record
	// never lands.
	rec2 := &metrics.Record{ID: "straggler-2", Provider: "p", Model: "m",
		KeyHash: "k", StatusCode: 200, Start: time.Now()}
	metrics.FinalizeRecord(rec2)
	tx = acquire()
	start := time.Now()
	s.Record(rec2)
	deadline = start.Add(busy + 5*time.Second)
	for s.Dropped() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("permanently-held lock never failed closed (busy_timeout masked it)")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// The rescaled honor bound keeps the original's 20% slack: the drop can
	// only land after the full busy window expires on the writer's flush
	// attempt, so anything under 200ms means the window was not honored.
	if waited := time.Since(start); waited < busy-50*time.Millisecond {
		t.Errorf("gave up after %s, want >= ~%s (the busy window was not honored)", waited, busy)
	}
	recs, err := s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == "straggler-2" {
			t.Error("dropped record straggler-2 is somehow durable")
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}
