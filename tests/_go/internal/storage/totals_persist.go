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
	if doc["high_rowid"] != float64(2) {
		t.Fatalf("persisted high_rowid = %v, want 2 (the append witness)", doc["high_rowid"])
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

// rawInsertRequest appends one complete request row through an independent
// connection, exactly like an external writer: a fresh rowid with no epoch
// change. Only the columns without a SQL default are named.
func rawInsertRequest(t *testing.T, db *sql.DB, id string, startedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO requests (
		id, provider, model, key_hash, user_agent, stream, status_code,
		started_at, duration_ms, ttft_ms, first_token_at, last_token_at,
		finish_reason, error_type, error_msg, error_code, tool_calls,
		input_tokens, output_tokens, total_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, client_disconnected,
		rate_limit_remaining, rate_limit_limit, cost,
		turns_user, turns_assistant, turns_tool, response_headers
	) VALUES (?, 'external.example', 'm', 'k', 'ua', 0, 200,
		?, 0, 0, 0, 0, '', '', '', '', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, '')`, id, startedAt.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

// TestTotalsReloadDetectsExternalAppend pins the append witness: an ordinary
// fresh-rowid INSERT from another process leaves the projection epoch
// unchanged, so the persisted document must also record high_rowid or a stale
// aggregate is accepted at boot.
func TestTotalsReloadDetectsExternalAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "totals-append.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Record(&metrics.Record{ID: "in-process", Provider: "neutral.example", Model: "m", StatusCode: 200, Start: start})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	rawInsertRequest(t, other, "external", start.Add(time.Millisecond))
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Totals(); got.Requests != 2 {
		t.Fatalf("totals after external append = %+v, want 2 (rebuilt, not the stale document)", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	// Control: a normal in-process append still round-trips through the
	// persisted document and reloads the exact aggregate.
	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(&metrics.Record{ID: "control", Provider: "neutral.example", Model: "m", StatusCode: 200, Start: start.Add(2 * time.Millisecond)})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	want := s.Totals()
	if want.Requests != 3 {
		t.Fatalf("control totals before close = %+v, want 3", want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Totals(); got != want {
		t.Fatalf("reloaded control totals = %+v, want %+v", got, want)
	}
}
