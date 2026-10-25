package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestDataVersionTracksAllCommittedWriters(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	version := func() int64 {
		t.Helper()
		v, err := s.DataVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()
	if got := version(); got != before {
		t.Fatalf("unchanged database revision = %d, want %d", got, before)
	}
	changed := func() {
		t.Helper()
		after := version()
		if after == before {
			t.Fatalf("committed mutation left data_version at %d", before)
		}
		before = after
	}
	s.Record(&metrics.Record{ID: "a", Provider: "old.example", Start: time.Now(), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	changed()
	if _, err := s.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"}); err != nil {
		t.Fatal(err)
	}
	changed()

	// A different connection represents a draining parent process: no method
	// on this Store observes the mutation, but the database revision must.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec("UPDATE requests SET cost = ? WHERE id = ?", 0.75, "a"); err != nil {
		t.Fatal(err)
	}
	changed()
	if _, err := s.Query(t.Context(), "SELECT id FROM requests"); err != nil {
		t.Fatal(err)
	}
	if got := version(); got != before {
		t.Fatalf("read-only query changed revision: %d -> %d", before, got)
	}
	if _, err := s.Clear(t.Context(), &PurgeFilter{Provider: "new.example"}, nil); err != nil {
		t.Fatal(err)
	}
	changed()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DataVersion(t.Context()); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("DataVersion after Close = %v, want ErrConnDone", err)
	}
}

func TestDataVersionConnectionAndCancellation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "versions.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.DataVersion(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled initial revision check = %v", err)
	}
	v, err := s.DataVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn := s.versionConn
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 10 {
				got, err := s.DataVersion(t.Context())
				if err != nil || got != v {
					t.Errorf("concurrent revision = %d, %v; want %d", got, err, v)
				}
			}
		})
	}
	wg.Wait()
	if conn != s.versionConn {
		t.Fatal("revision connection was replaced")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DataVersion(t.Context()); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("failed fixed connection = %v, want ErrConnDone", err)
	}
	if conn != s.versionConn {
		t.Fatal("failed connection was replaced: connection-local revisions could collide")
	}
	if _, err := s.Query(t.Context(), "SELECT COUNT(*) FROM requests"); err != nil {
		t.Fatalf("failed cache connection must not disable ordinary reads: %v", err)
	}
}

// TestPurgeRebuildNeverLosesConcurrentCommits is the regression test for the
// fixed races: a purge + totals rebuild running WHILE the writer keeps
// committing must leave totals exactly equal to the final row count. Before
// RebuildTotals serialized through the writer goroutine, a rebuild racing
// account() permanently dropped commits that landed mid-scan.
func TestPurgeRebuildNeverLosesConcurrentCommits(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Seed some history.
	for i := 0; i < 20; i++ {
		s.Record(&metrics.Record{ID: "pre" + string(rune('a'+i)), Start: time.Now(), StatusCode: 200})
	}
	waitRows(t, s, 20)

	// Concurrent producers keep inserting while a purge+rebuild runs. (A full
	// purge deletes the seeded rows AND any that committed before the delete;
	// after it, only post-delete commits may remain.)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 60; i++ {
			s.Record(&metrics.Record{ID: "live" + string(rune(i)), Start: time.Now(), StatusCode: 200})
		}
		close(done)
	}()
	if _, err := s.Clear(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	// The purge transaction is the rebuild owner. Draining later completions
	// must preserve exact totals without a second corrective rescan.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Totals().Requests, countRows(t, s); got != want {
		t.Fatalf("totals %d != rows %d", got, want)
	}
}

func waitRows(t *testing.T, s *Store, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Totals().Requests == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("rows = %d, want %d", s.Totals().Requests, n)
}

func countRows(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.rdb.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM requests").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
