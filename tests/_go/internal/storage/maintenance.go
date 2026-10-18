package storage

import (
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"modernc.org/sqlite"
)

// TestFullPurgeUsesMaintenanceBudget pins the budget separation for an
// administrative full purge: the interactive dashboard query timeout must not
// abort a maintenance transaction that rewrites all history.
func TestFullPurgeUsesMaintenanceBudget(t *testing.T) {
	opts := testOpts
	opts.QueryTimeout = time.Nanosecond
	s := openPurgeTestStore(t, opts)
	for i := range 5 {
		s.Record(&metrics.Record{ID: fmt.Sprintf("purge-maintenance-%d", i), Start: time.Now(), StatusCode: 200})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	result, err := s.Clear(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("full purge under a short interactive timeout: %v", err)
	}
	if result.Deleted != 5 {
		t.Fatalf("purge deleted %d rows, want 5", result.Deleted)
	}
	if n := countRows(t, s); n != 0 {
		t.Fatalf("rows after purge = %d, want 0", n)
	}
	if got := s.Totals().Requests; got != 0 {
		t.Fatalf("totals after purge = %d, want 0", got)
	}
}

// TestBootTotalsRebuildUsesMaintenanceBudget pins the boot rebuild on the same
// maintenance budget: a missing persisted totals document forces a full scan,
// which must not inherit the interactive query timeout.
func TestBootTotalsRebuildUsesMaintenanceBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot-maintenance.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		s.Record(&metrics.Record{ID: fmt.Sprintf("boot-maintenance-%d", i), Start: time.Now(), StatusCode: 200})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Drop the persisted document so the next Open must rebuild from rows.
	if _, err := s.db.Exec(`DELETE FROM meta WHERE key = ?`, totalsMetaKey); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	short := testOpts
	short.QueryTimeout = time.Nanosecond
	reopened, err := Open(path, short)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.TotalsDegraded() {
		t.Fatal("boot totals rebuild inherited the interactive query timeout")
	}
	if got := reopened.Totals().Requests; got != 5 {
		t.Fatalf("rebuilt totals = %d, want 5", got)
	}
}

// TestPurgeExpiredMaintenanceDeadlineRollsBack pins fail-closed behavior: the
// maintenance budget itself genuinely expires during the purge transaction
// (the shortened test seam), which still aborts it, and no durable, ring or
// sidecar state is published.
func TestPurgeExpiredMaintenanceDeadlineRollsBack(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	name := fmt.Sprintf("purge_deadline_gate_%d", purgeTestFunction.Add(1))
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	opts := testOpts
	opts.MaintenanceTimeout = 25 * time.Millisecond
	s := openPurgeTestStore(t, opts)
	b := metrics.NewBuffer(4)
	s.Recorder(b).Record(&metrics.Record{ID: "old", Start: time.Now(), StatusCode: 500})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDebugCapture(t.Context(), "old", "session", 0, 0, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER purge_deadline_gate BEFORE DELETE ON requests BEGIN SELECT " + name + "(); END"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.Clear(t.Context(), nil, b)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not reach gate")
	}
	time.Sleep(60 * time.Millisecond) // the maintenance-derived deadline expires
	unblock()
	if err := <-result; err == nil {
		t.Fatal("expired maintenance deadline reported a successful purge")
	}
	// Join the writer before inspecting state: Clear may return on its own
	// deadline while the writer is still rolling the interrupted transaction
	// back.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s); n != 1 {
		t.Fatalf("rows after expired-deadline purge = %d, want 1", n)
	}
	if s.Totals().Requests != 1 {
		t.Fatalf("totals after expired-deadline purge = %+v", s.Totals())
	}
	if b.Len() != 1 || b.Counters().TotalReq != 1 {
		t.Fatalf("ring after expired-deadline purge: len=%d counters=%+v", b.Len(), b.Counters())
	}
	if raw, err := s.LoadDebugCapture(t.Context(), "old"); err != nil || len(raw) == 0 {
		t.Fatalf("sidecar after expired-deadline purge: %s %v", raw, err)
	}
}
