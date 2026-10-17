package storage

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func seedTotalsRows(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	rows := []*metrics.Record{
		{ID: "totals-ok", Provider: "p", Model: "m", StatusCode: 200, Start: now,
			TTFTMs: 100, OverallTPS: 20, Cost: 0.5, Usage: metrics.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, ReasoningTokens: 1}},
		{ID: "totals-err", Provider: "p", Model: "m", StatusCode: 500, Start: now.Add(time.Millisecond),
			Usage: metrics.Usage{InputTokens: 3, OutputTokens: 4, ReasoningTokens: 2}},
	}
	for _, r := range rows {
		metrics.FinalizeRecord(r)
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}

// TestPersistedTotalsSurviveReopenAndDetectExternalMutation pins the durable
// aggregate contract: a healthy reopen loads the document written in the same
// transaction as the rows, and any out-of-band request mutation advances the
// projection epoch, forcing a rebuild instead of serving a stale claim.
func TestPersistedTotalsSurviveReopenAndDetectExternalMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "totals.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	seedTotalsRows(t, s)
	want := s.Totals()
	if want.Requests != 2 || want.Errors != 1 || want.InputTok != 13 || want.OutputTok != 9 {
		t.Fatalf("seed totals = %+v", want)
	}
	raw, err := s.LoadMeta(t.Context(), totalsMetaKey)
	if err != nil || raw == "" {
		t.Fatalf("persisted totals = %q, %v", raw, err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("persisted totals JSON: %v", err)
	}
	if doc["requests"] != float64(2) {
		t.Fatalf("persisted requests = %v, want 2", doc["requests"])
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A second handle mutates authoritative rows while the store is closed.
	// The request trigger advances the epoch; boot must not trust the cache.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`UPDATE requests SET status_code = 502 WHERE id = 'totals-ok'`); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Totals()
	if got.Requests != 2 || got.Errors != 2 {
		t.Fatalf("totals after external mutation = %+v, want 2 requests / 2 errors (rebuilt, not stale)", got)
	}
}

// TestFullPurgePersistsZeroTotals pins the full-clear fast path: the durable
// document is zeroed in the delete transaction, so a reopen never scans (or
// resurrects) pre-purge aggregates.
func TestFullPurgePersistsZeroTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purge-totals.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	seedTotalsRows(t, s)
	if _, err := s.Clear(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Totals(); got.Requests != 0 || got.InputTok != 0 || got.Cost != 0 {
		t.Fatalf("post-purge totals = %+v, want zero", got)
	}
}
