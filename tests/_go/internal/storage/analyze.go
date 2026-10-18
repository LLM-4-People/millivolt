package storage

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestOpenBuildsRequestStatistics pins the planner-statistics lifecycle: a
// grown history whose analyzed row count drifted far enough is re-analyzed at
// open, before the projection preload and any dashboard read. Without this,
// SQLite costs the log and equality indexes from default guesses instead of
// the stored row distribution.
func TestOpenBuildsRequestStatistics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	const n = requestStatisticsDrift + 2000
	now := time.Now()
	records := make([]*metrics.Record, n)
	for i := range records {
		records[i] = &metrics.Record{
			ID:         fmt.Sprintf("stats-%05d", i),
			Provider:   "stats.example",
			Model:      "stats-model",
			Start:      now.Add(time.Duration(i) * time.Millisecond),
			StatusCode: 200,
		}
	}
	if err := s.insertBatch(records); err != nil {
		t.Fatalf("insertBatch: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var stat string
	if err := s.rdb.QueryRow(
		`SELECT stat FROM sqlite_stat1 WHERE tbl = 'requests' LIMIT 1`).Scan(&stat); err != nil {
		t.Fatalf("read sqlite_stat1: %v", err)
	}
	fields := strings.Fields(stat)
	if len(fields) == 0 {
		t.Fatalf("empty sqlite_stat1 row: %q", stat)
	}
	got, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("parse analyzed row count %q: %v", fields[0], err)
	}
	if got != n {
		t.Fatalf("analyzed rows = %d, want %d", got, n)
	}
	stale, err := requestStatisticsStale(t.Context(), s.db)
	if err != nil {
		t.Fatalf("requestStatisticsStale: %v", err)
	}
	if stale {
		t.Fatal("fresh statistics reported stale; every boot would re-analyze")
	}
}
