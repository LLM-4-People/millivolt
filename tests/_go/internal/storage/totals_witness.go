package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestTotalsWitnessCountsStragglerCommit reproduces the restart-handoff
// window: the child loads a valid persisted totals document, the still
// draining parent commits one more row, and the child's first batch must
// rebuild from the transaction's committed rows instead of stamping the stale
// base with the parent-advanced witnesses.
func TestTotalsWitnessCountsStragglerCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff.db")
	parent, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	parent.Record(&metrics.Record{ID: "A", Provider: "neutral.example", Model: "m", KeyHash: "k", StatusCode: 200, Start: now})
	if err := parent.Flush(); err != nil {
		t.Fatal(err)
	}

	child, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if got := child.Totals().Requests; got != 1 {
		t.Fatalf("child boot totals = %d, want 1", got)
	}

	parent.Record(&metrics.Record{ID: "B", Provider: "neutral.example", Model: "m", KeyHash: "k", StatusCode: 200, Start: now.Add(time.Millisecond)})
	if err := parent.Flush(); err != nil {
		t.Fatal(err)
	}
	child.Record(&metrics.Record{ID: "C", Provider: "neutral.example", Model: "m", KeyHash: "k", StatusCode: 200, Start: now.Add(2 * time.Millisecond)})
	if err := child.Flush(); err != nil {
		t.Fatal(err)
	}

	var rows int64
	if err := child.rdb.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	persisted, witness, found, err := loadTotals(t.Context(), child.rdb)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("durable rows = %d, want 3", rows)
	}
	if got := child.Totals().Requests; got != 3 {
		t.Fatalf("child totals = %d with 3 durable rows (stale base was blessed)", got)
	}
	if !found || persisted.Requests != 3 {
		t.Fatalf("persisted totals = %+v found=%v, want requests=3", persisted, found)
	}
	if child.TotalsDegraded() {
		t.Fatal("totals marked degraded after a successful in-transaction rebuild")
	}
	// The persisted document and the in-memory aggregate carry the same
	// witness the transaction committed against.
	var epoch, highRowID int64
	if err := child.rdb.QueryRow(`SELECT epoch, high_rowid FROM request_projection_state WHERE singleton = 1`).Scan(&epoch, &highRowID); err != nil {
		t.Fatal(err)
	}
	if witness.epoch != epoch || witness.highRowID != highRowID {
		t.Fatalf("totals witness = %+v, want current %+v", witness, totalsWitness{epoch: epoch, highRowID: highRowID})
	}

	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Totals().Requests; got != 3 {
		t.Fatalf("reopened totals = %d, want 3", got)
	}
}

// TestTotalsBaseMismatchRebuilds pins the witness comparison itself: a base
// whose witness no longer matches the transaction must be rebuilt from rows,
// never extended and blessed with the newer witness.
func TestTotalsBaseMismatchRebuilds(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "witness.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.insertBatch([]*metrics.Record{{ID: "first", Provider: "neutral.example", Start: now, StatusCode: 200}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Totals().Requests; got != 1 {
		t.Fatalf("seed totals = %d, want 1", got)
	}

	// Simulate a stale base from a restart handoff: the aggregate claims 99
	// rows against a witness that no longer describes the durable state.
	s.aggMu.Lock()
	s.totals = Totals{Requests: 99}
	s.totalsBuilt = true
	s.totalsWitness = totalsWitness{}
	s.aggMu.Unlock()

	if err := s.insertBatch([]*metrics.Record{{ID: "second", Provider: "neutral.example", Start: now.Add(time.Millisecond), StatusCode: 200}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Totals().Requests; got != 2 {
		t.Fatalf("totals after mismatch = %d, want the rebuilt 2 (never 100)", got)
	}
	persisted, _, found, err := loadTotals(t.Context(), s.rdb)
	if err != nil {
		t.Fatal(err)
	}
	if !found || persisted.Requests != 2 {
		t.Fatalf("persisted totals after mismatch = %+v found=%v, want requests=2", persisted, found)
	}
}
