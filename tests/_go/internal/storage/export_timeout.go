package storage

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// slowExportWriter simulates a consumer that reads slower than the export
// budget: each write completes, so this is the "slow reader" half of the
// stalled-reader regression rather than an uninterruptible syscall.
type slowExportWriter struct {
	delay time.Duration
}

func (w slowExportWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return len(p), nil
}

// TestExportWhereTotalTimeoutIsBoundedAndDistinct pins the export guardrail:
// a slow writer and a stalled row source both end with ErrExportTimeout (not a
// silent success and not a caller cancellation), a canceled caller keeps
// context.Canceled, and a shorter caller deadline is not reported as the
// server's own timeout. After every failure the read snapshot is released, so
// a normal export immediately succeeds.
func TestExportWhereTotalTimeoutIsBoundedAndDistinct(t *testing.T) {
	opts := testOpts
	opts.ExportTimeout = 20 * time.Millisecond
	s, err := Open(filepath.Join(t.TempDir(), "export-timeout.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Record(&metrics.Record{ID: "slow-1", Provider: "neutral.example", Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	assertReleased := func(t *testing.T) {
		t.Helper()
		var out bytes.Buffer
		if n, err := s.ExportWhere(context.Background(), PurgeFilter{}, &out); err != nil || n != 1 {
			t.Fatalf("export after timeout = %d, %v; want 1, nil (read snapshot not released)", n, err)
		}
	}

	t.Run("slow writer", func(t *testing.T) {
		n, err := s.ExportWhere(context.Background(), PurgeFilter{}, slowExportWriter{delay: 100 * time.Millisecond})
		if !errors.Is(err, ErrExportTimeout) {
			t.Fatalf("slow-writer export error = %v, want ErrExportTimeout", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("server timeout reported as caller cancellation: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("server timeout lost its deadline cause: %v", err)
		}
		if n != 0 {
			t.Fatalf("slow-writer export wrote %d rows after the budget expired, want 0", n)
		}
		assertReleased(t)
	})

	t.Run("stalled row source", func(t *testing.T) {
		previous := exportRowHook
		exportRowHook = func(ctx context.Context) { <-ctx.Done() }
		t.Cleanup(func() { exportRowHook = previous })
		n, err := s.ExportWhere(context.Background(), PurgeFilter{}, &bytes.Buffer{})
		exportRowHook = previous
		if !errors.Is(err, ErrExportTimeout) {
			t.Fatalf("stalled-row export error = %v, want ErrExportTimeout", err)
		}
		if n != 0 {
			t.Fatalf("stalled-row export wrote %d rows, want 0", n)
		}
		assertReleased(t)
	})

	t.Run("caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := s.ExportWhere(ctx, PurgeFilter{}, &bytes.Buffer{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled export error = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrExportTimeout) {
			t.Fatalf("caller cancellation reported as server timeout: %v", err)
		}
		assertReleased(t)
	})

	t.Run("caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
		defer cancel()
		_, err := s.ExportWhere(ctx, PurgeFilter{}, slowExportWriter{delay: 50 * time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller-deadline export error = %v, want context.DeadlineExceeded", err)
		}
		if errors.Is(err, ErrExportTimeout) {
			t.Fatalf("caller deadline reported as server export timeout: %v", err)
		}
		assertReleased(t)
	})
}
