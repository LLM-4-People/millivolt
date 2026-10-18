package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestComputeTotalsRevalidatesEpoch forces a commit between the totals read
// snapshot and the writer-side save. The rebuild must notice the moved
// witness (here the high rowid advanced by an ordinary append), retry, and
// persist/install the rows it actually counted; stamping the old count with
// the new witness would make loadTotals accept an undercount.
func TestComputeTotalsRevalidatesEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "totals-snapshot.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now()
	seed := []*metrics.Record{
		{ID: "totals-a", Provider: "stats.example", Start: now, StatusCode: 200},
		{ID: "totals-b", Provider: "stats.example", Start: now.Add(time.Millisecond), StatusCode: 200},
	}
	if err := s.insertBatch(seed); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	late := &metrics.Record{ID: "totals-late", Provider: "stats.example", Start: now.Add(2 * time.Millisecond), StatusCode: 200}
	inserted := false
	if err := s.computeTotalsWith(t.Context(), func() error {
		if inserted {
			return nil
		}
		inserted = true
		return s.insertBatch([]*metrics.Record{late})
	}); err != nil {
		t.Fatalf("computeTotalsWith: %v", err)
	}
	totals, found, err := loadTotals(t.Context(), s.rdb)
	if err != nil {
		t.Fatalf("loadTotals: %v", err)
	}
	if !found {
		t.Fatal("persisted totals rejected after rebuild")
	}
	if totals.Requests != 3 {
		t.Fatalf("persisted requests = %d, want 3 (the concurrent commit must be counted)", totals.Requests)
	}
}
