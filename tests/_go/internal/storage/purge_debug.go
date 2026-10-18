package storage

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func blockDebugAfterCommit(t *testing.T) (entered <-chan struct{}, release chan<- struct{}) {
	t.Helper()
	in := make(chan struct{})
	out := make(chan struct{})
	previous := debugAfterCommitHook
	debugAfterCommitHook = func() {
		close(in)
		<-out
	}
	t.Cleanup(func() { debugAfterCommitHook = previous })
	return in, out
}

func unresolvedDebugBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	err := s.rdb.QueryRow(`SELECT COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug
		WHERE expires_at = 0 OR expires_at > ?`, time.Now().UnixMilli()).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPurgePreservesConcurrentDebugSaveAtCap drives a 22-byte save into the
// post-commit window of a purge that freed the same store's only room. The
// counter must end at the durable unexpired sum, not at the purge's old
// absolute total, and the save must be admitted after the purge freed space.
func TestPurgePreservesConcurrentDebugSaveAtCap(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 100
	opts.FlushInterval = time.Hour
	s, err := Open(filepath.Join(t.TempDir(), "purge-debug.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.Record(&metrics.Record{ID: "old", Provider: "neutral.example", Model: "m", StatusCode: 200, Start: now})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDebugCapture(t.Context(), "old", "sess", now.UnixMilli(), 0, bytes.Repeat([]byte("o"), 80)); err != nil {
		t.Fatal(err)
	}
	if got := debugAccounting(t, s); got != 80 {
		t.Fatalf("seeded debug bytes = %d, want 80", got)
	}

	entered, release := blockDebugAfterCommit(t)
	purged := make(chan error, 1)
	go func() {
		_, err := s.Clear(context.Background(), nil, nil)
		purged <- err
	}()
	<-entered

	saved := make(chan error, 1)
	payload := bytes.Repeat([]byte("n"), 22)
	go func() {
		saved <- s.SaveDebugCapture(context.Background(), "new", "sess", now.UnixMilli(), 0, payload)
	}()
	time.Sleep(20 * time.Millisecond) // the save is parked on debugMu behind the commit
	close(release)
	if err := <-purged; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatalf("capture after the purge freed capacity: %v", err)
	}
	if got, want := debugAccounting(t, s), int64(len(payload)); got != want {
		t.Fatalf("debug byte counter = %d, want %d", got, want)
	}
	if got, want := unresolvedDebugBytes(t, s), int64(len(payload)); got != want {
		t.Fatalf("durable unexpired sidecars = %d, want %d", got, want)
	}
}

// TestMergePreservesConcurrentDebugSave applies the same shared install
// contract to merge: its committed delta must not overwrite a save that
// arrives while the merge publishes.
func TestMergePreservesConcurrentDebugSave(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	live, err := Open(filepath.Join(t.TempDir(), "merge-debug.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	now := time.Now()
	live.Record(&metrics.Record{ID: "live", Provider: "live.example", Model: "m", StatusCode: 200, Start: now})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := live.SaveDebugCapture(t.Context(), "live-debug", "sess", now.UnixMilli(), 0, []byte("live-payload")); err != nil {
		t.Fatal(err)
	}

	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "backup", Provider: "backup.example", Model: "m", StatusCode: 200, Start: now.Add(time.Millisecond)})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", now.UnixMilli(), 0, []byte("backup-payload")); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	entered, release := blockDebugAfterCommit(t)
	merged := make(chan error, 1)
	go func() {
		_, _, err := live.MergeSnapshot(context.Background(), data)
		merged <- err
	}()
	<-entered
	saved := make(chan error, 1)
	go func() {
		saved <- live.SaveDebugCapture(context.Background(), "new-debug", "sess", now.UnixMilli(), 0, []byte("new-payload"))
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-merged; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if got, want := debugAccounting(t, live), unresolvedDebugBytes(t, live); got != want {
		t.Fatalf("debug counter after merge = %d, durable unexpired = %d", got, want)
	}
}

// TestClearCancellationRetiresBeforeSecondCommand reproduces M3: the first
// Clear is blocked on an external write lock and times out. It must keep
// purgeMu until the writer retires its command, so the second Clear is not
// overwritten and still deletes rows and ring entries.
func TestClearCancellationRetiresBeforeSecondCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purge-cancel.db")
	opts := testOpts
	opts.WriteChanCap = 8
	opts.FlushInterval = time.Hour
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := metrics.NewBuffer(8)
	s.Recorder(b).Record(&metrics.Record{ID: "old", Provider: "neutral.example", Model: "m", StatusCode: 200, Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	hold, err := sql.Open("sqlite", path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	tx, err := hold.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('hold', '1')`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	first := make(chan error, 1)
	go func() {
		_, err := s.Clear(ctx, nil, b)
		first <- err
	}()
	waitPurgeCondition(t, func() bool { return s.pendingPurge.Load() != nil })
	time.Sleep(30 * time.Millisecond) // the writer is inside the blocked transaction

	second := make(chan purgeOutcome, 1)
	go func() {
		secondCtx, secondCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer secondCancel()
		got, err := s.Clear(secondCtx, nil, b)
		second <- purgeOutcome{result: got, err: err}
	}()
	time.Sleep(200 * time.Millisecond) // first deadline passes while the lock is held
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err == nil {
		t.Fatal("first Clear reported success after its deadline while the write lock was held")
	}
	out := <-second
	if out.err != nil {
		t.Fatalf("second Clear: %v", out.err)
	}
	if out.result.Deleted != 1 || out.result.BufferRemoved != 1 {
		t.Fatalf("second Clear result = %+v, want deleted=1 removed=1", out.result)
	}
	if n := countRows(t, s); n != 0 {
		t.Fatalf("rows after second Clear = %d, want 0", n)
	}
	if b.Len() != 0 {
		t.Fatalf("ring rows after second Clear = %d, want 0", b.Len())
	}
	if s.pendingPurge.Load() != nil {
		t.Fatal("retired command left pendingPurge set")
	}
}

// TestClearCommittedBeforeLateCancellationReportsSuccess pins the other side
// of removing the ctx.Done early return: once the command has committed, a
// caller cancellation must not turn success into a false cancellation error.
func TestClearCommittedBeforeLateCancellationReportsSuccess(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	s.Record(&metrics.Record{ID: "old", Provider: "neutral.example", Model: "m", StatusCode: 200, Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	entered, release := blockDebugAfterCommit(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan purgeOutcome, 1)
	go func() {
		got, err := s.Clear(ctx, nil, nil)
		result <- purgeOutcome{result: got, err: err}
	}()
	<-entered
	cancel() // the caller deadline/cancellation arrives after commit
	close(release)
	out := <-result
	if out.err != nil {
		t.Fatalf("committed Clear reported %v after the caller canceled", out.err)
	}
	if out.result.Deleted != 1 {
		t.Fatalf("committed Clear deleted=%d, want 1", out.result.Deleted)
	}
}
