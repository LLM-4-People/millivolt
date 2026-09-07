package storage

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestSnapshotIncludesFlushedRows(t *testing.T) {
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: "snap-1", Provider: "neutral.example", Model: "neutral", Start: time.Now(), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.CheckDatabase(data); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Encode(backup.Archive{Database: data}); err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir()+"/snap.db", testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
