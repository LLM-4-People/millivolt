package web

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestProjectionPreloadUsesMaintenanceBudget pins the startup preload on the
// storage maintenance budget: a short interactive query timeout must still
// fail ordinary reads while both the boot totals rebuild and the full
// projection preload complete.
func TestProjectionPreloadUsesMaintenanceBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preload-maintenance.db")
	d := config.Default()
	opts := storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  time.Second,
		WriteTrackCap: 128,
	}
	seeded, err := storage.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	const rows = 64
	now := time.Now()
	for i := range rows {
		seeded.Record(&metrics.Record{
			ID:         fmt.Sprintf("seed-%03d", i),
			Provider:   "preload.example",
			Model:      "model-a",
			Start:      now.Add(time.Duration(i) * time.Millisecond),
			StatusCode: 200,
		})
	}
	if err := seeded.Flush(); err != nil {
		t.Fatal(err)
	}
	// Advance the projection epoch without changing the row count so the next
	// Open must rebuild the persisted totals document from rows.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`UPDATE requests SET status_code = status_code WHERE id = 'seed-000'`); err != nil {
		other.Close()
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := seeded.Close(); err != nil {
		t.Fatal(err)
	}

	short := opts
	short.QueryTimeout = time.Nanosecond
	store, err := storage.Open(path, short)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.TotalsDegraded() {
		t.Fatal("boot totals rebuild inherited the interactive query timeout")
	}
	if got := store.Totals().Requests; got != rows {
		t.Fatalf("rebuilt totals = %d, want %d", got, rows)
	}

	api := NewAggAPI(metrics.NewBuffer(rows), store, time.Second)
	if err := api.PreloadHistory(t.Context()); err != nil {
		t.Fatalf("preload under a short interactive timeout: %v", err)
	}
	api.projection.mu.RLock()
	got := len(api.projection.data.rows)
	api.projection.mu.RUnlock()
	if got != rows {
		t.Fatalf("preloaded rows = %d, want %d", got, rows)
	}

	// The interactive budget on this same store is genuinely expired: an
	// ordinary projection read must fail closed rather than silently using the
	// maintenance budget.
	if next, _, err := store.StreamProjection(t.Context(), "id", storage.ProjectionCursor{}, func(*sql.Rows) error { return nil }); err == nil || next.Ready {
		t.Fatalf("interactive projection under a 1ns timeout = %+v, %v; want a fail-closed error", next, err)
	}
}
