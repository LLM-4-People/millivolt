package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestConcurrentExportsDoNotExhaustReadPool runs more exporters than the
// bounded read pool while every row carries a debug capture. One export must
// need one pooled connection for its cursor and capture together; a nested
// LoadDebugCapture acquisition self-deadlocked until the query timeout.
func TestConcurrentExportsDoNotExhaustReadPool(t *testing.T) {
	opts := testOpts
	opts.QueryTimeout = 3 * time.Second
	s, err := Open(filepath.Join(t.TempDir(), "export-pool.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	const rows = 20
	for i := range rows {
		id := fmt.Sprintf("dbg-%02d", i)
		s.Record(&metrics.Record{
			ID: id, Provider: "neutral.example", Model: "m", KeyHash: "k",
			StatusCode: 200, Start: now.Add(time.Duration(i) * time.Millisecond), Debug: true,
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		id := fmt.Sprintf("dbg-%02d", i)
		if err := s.SaveDebugCapture(t.Context(), id, "sess", now.UnixMilli(), 0, []byte(`{"probe":true}`)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, sqliteReadConns+3)
	outputs := make([]string, len(errs))
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var sink strings.Builder
			_, errs[i] = s.ExportWhere(context.Background(), PurgeFilter{}, &sink)
			outputs[i] = sink.String()
		}(i)
	}
	// A normal dashboard read must still make progress while every pooled
	// exporter is running; no exporter may own two connections.
	readDone := make(chan error, 1)
	go func() {
		_, err := s.LoadRecent(context.Background(), 1)
		readDone <- err
	}()
	wg.Wait()
	if err := <-readDone; err != nil {
		t.Fatalf("normal read during exports: %v", err)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("export %d failed: %v", i, err)
		}
		if got := strings.Count(outputs[i], `"debug_capture"`); got != rows {
			t.Fatalf("export %d captures = %d, want %d", i, got, rows)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var sink strings.Builder
	if _, err := s.ExportWhere(canceled, PurgeFilter{}, &sink); err == nil {
		t.Fatal("canceled export reported success")
	}
}
