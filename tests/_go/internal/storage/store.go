package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// testOpts derives from the canonical defaults (config.Default) so tests
// exercise the real production pipeline tuning and can never drift from it.
var testOpts = func() Options {
	d := config.Default()
	return Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
		QueryMaxBytes: d.StorageQueryMaxBytes,
		QueryMaxRows:  d.StorageQueryMaxRows,
	}
}()

func TestDataVersionTracksAllCommittedWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "versions.db")
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
	if _, err := s.PurgeWhere(t.Context(), PurgeFilter{Provider: "new.example"}); err != nil {
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

func TestStreamRowsOldestFirstEarlyStop(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, c := range []struct {
		id  string
		age time.Duration
	}{{"newest", time.Minute}, {"oldest", time.Hour}, {"middle", 30 * time.Minute}} {
		s.Record(&metrics.Record{ID: c.id, Start: now.Add(-c.age), StatusCode: 200})
	}
	waitRows(t, s, 3)
	stop := errors.New("first row found")
	count := 0
	err = s.StreamRows(t.Context(), "id", "", nil, true, func(rows *sql.Rows) error {
		count++
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if id != "oldest" {
			t.Fatalf("first row = %q, want oldest", id)
		}
		return stop
	})
	if !errors.Is(err, stop) || count != 1 {
		t.Fatalf("early stop = %v after %d rows, want callback error after one", err, count)
	}
	var last int64
	err = s.StreamRows(t.Context(), "started_at", "", nil, true, func(rows *sql.Rows) error {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return err
		}
		if at < last {
			t.Fatalf("timestamps out of order: %d < %d", at, last)
		}
		last = at
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPurgeRebuildNeverLosesConcurrentCommits is the regression test for the
// fixed races: a purge + totals rebuild running WHILE the writer keeps
// committing must leave totals exactly equal to the final row count. Before
// RebuildTotals serialized through the writer goroutine, a rebuild racing
// account() permanently dropped commits that landed mid-scan.
func TestPurgeRebuildNeverLosesConcurrentCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
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
	if err := s.Purge(context.Background()); err != nil {
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

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	rec := &metrics.Record{
		ID:           "r1",
		Provider:     "alpha",
		Model:        "model-a",
		KeyHash:      "abc",
		UserAgent:    "test",
		Stream:       true,
		StatusCode:   200,
		Start:        now,
		End:          now.Add(time.Second),
		FirstTokenAt: now.Add(100 * time.Millisecond),
		LastTokenAt:  now.Add(900 * time.Millisecond),
		FinishReason: "stop",
		ToolCalls:    2,
		Usage: metrics.Usage{
			InputTokens:     10,
			OutputTokens:    20,
			CacheReadTokens: 5,
		},
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	rows, err := s2.Query(t.Context(), `SELECT * FROM requests WHERE id = 'r1'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["provider"] != "alpha" {
		t.Errorf("provider = %v", row["provider"])
	}
	if row["input_tokens"] != int64(10) {
		t.Errorf("input_tokens = %v (%T)", row["input_tokens"], row["input_tokens"])
	}
	if row["output_tokens"] != int64(20) {
		t.Errorf("output_tokens = %v", row["output_tokens"])
	}
	if row["cache_read_tokens"] != int64(5) {
		t.Errorf("cache_read_tokens = %v", row["cache_read_tokens"])
	}
	if row["tool_calls"] != int64(2) {
		t.Errorf("tool_calls = %v", row["tool_calls"])
	}
	if row["ttft_ms"] != int64(100) {
		t.Errorf("ttft_ms = %v", row["ttft_ms"])
	}
}

// TestAllFieldsSurviveRestart populates every persistable Record field,
// closes the store, reopens it (simulating a restart), and asserts the fields
// round-trip through LoadRecent - including JSON-encoded tool names and the
// pointer request params.
func TestAllFieldsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Millisecond)
	rec := &metrics.Record{
		ID:                "full1",
		Provider:          "alpha",
		Model:             "model-a",
		KeyHash:           "kh",
		UserAgent:         "ua",
		Client:            "example-client/1.0",
		ClientIP:          "203.0.113.7",
		ClientLang:        "go",
		Stream:            true,
		Method:            "POST",
		Path:              "/v1/chat/completions",
		ProviderRequestID: "req_abc123",
		ProviderServer:    "alpha",
		ProviderModel:     "model-a-2026-08-06",
		ProcessingMs:      321,
		RetryAfterMs:      1500,
		StatusCode:        200,
		Start:             now,
		End:               now.Add(2 * time.Second),
		FirstTokenAt:      now.Add(200 * time.Millisecond),
		LastTokenAt:       now.Add(1900 * time.Millisecond),
		FinishReason:      "stop",
		ToolCalls:         2,
		ToolNames:         []string{"read", "bash"},
		Usage: metrics.Usage{
			InputTokens:     100,
			OutputTokens:    50,
			TotalTokens:     150,
			CacheReadTokens: 10,
			CacheWrite:      3,
			ReasoningTokens: 20,
		},
		QueueWaitMs:        7,
		Retries:            2,
		RateLimited:        true,
		RateLimitRemaining: 99,
		RateLimitLimit:     100,
		Cost:               0.0123,
		AnswerTokens:       30,
		FirstAnswerAt:      now.Add(800 * time.Millisecond),
		HadAnswerContent:   true,
		ReqMaxTokens:       ptr(1024),
		ReqTemperature:     ptr(0.7),
		ReqTopP:            ptr(0.9),
		ReqToolsCount:      2,
		ReqToolChoice:      "auto",
		ReqN:               ptr(1),
		ReqStop:            2,
		ReqLogprobs:        true,
		ReqPresencePen:     ptr(0.5),
		ReqFrequencyPen:    ptr(0.25),
		ReqResponseFormat:  "json_object",
		ReqSeed:            ptr(int64(42)),
		ReqParallelTools:   ptr(true),
		ReqLogitBias:       3,
		ReqTopLogprobs:     ptr(5),
		ReqServiceTier:     "flex",
		ReqThinking:        true,
		ReqMetadataKeys:    2,
		ReqStreamOpts:      true,
		PromptPreview:      "hello world",
		ResponsePreview:    "hi there",
		TurnsUser:          1,
		TurnsAssistant:     2,
		TurnsTool:          1,
		CharsSystem:        512,
		CharsUser:          2048,
		CharsAssistant:     1024,
		CharsTool:          256,
		Images:             2,
		Attachments:        1,
		LastTurnRole:       "user",
		ReqReasoningEffort: "high",
		ReqVerbosity:       "medium",
		ClientMeta: metrics.ClientMeta{
			OS: "Linux", Arch: "x64", Runtime: "CPython", RuntimeVer: "3.12.8",
			PkgVer: "1.55.0", Format: "cursor", TimeoutMs: 120000, MaxConc: 2,
		},
		ResponseHeaders: map[string][]string{"x-request-id": {"req_abc123"}},
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadRecent returned %d records, want 1", len(got))
	}
	r := got[0]

	// Representative spot-checks across every field group.
	if r.AnswerTokens != 30 {
		t.Errorf("AnswerTokens = %d, want 30", r.AnswerTokens)
	}
	if r.GenTokens != 50 {
		t.Errorf("GenTokens = %d, want 50 (reasoning 20 + answer 30, derived by Finalize)", r.GenTokens)
	}
	if !r.HadAnswerContent {
		t.Error("HadAnswerContent = false, want true (the tool-call-only gate)")
	}
	if !r.FirstAnswerAt.Equal(now.Add(800 * time.Millisecond)) {
		t.Errorf("FirstAnswerAt = %v, want %v", r.FirstAnswerAt, now.Add(800*time.Millisecond))
	}
	// GenTokens + HadAnswerContent: the gen_tps numerator and its content-
	// presence gate must survive the restart (the dashboard recomputes nothing).
	if r.GenTokens != 50 {
		t.Errorf("GenTokens = %d, want 50 (reasoning 20 + answer 30, derived by Finalize)", r.GenTokens)
	}
	if !r.HadAnswerContent {
		t.Error("HadAnswerContent lost across restart")
	}
	if r.DecodeTPS == 0 || r.OverallTPS == 0 {
		t.Errorf("TPS fields lost: DecodeTPS=%v OverallTPS=%v", r.DecodeTPS, r.OverallTPS)
	}
	if r.GenTokens != 50 {
		t.Errorf("GenTokens = %d, want 50 (reasoning 20 + answer 30, derived by Finalize)", r.GenTokens)
	}
	if !r.HadAnswerContent {
		t.Error("HadAnswerContent lost across restart")
	}
	if r.Method != "POST" || r.Path != "/v1/chat/completions" {
		t.Errorf("Method/Path = %q %q", r.Method, r.Path)
	}
	if r.ProcessingMs != 321 || r.RetryAfterMs != 1500 {
		t.Errorf("ProcessingMs/RetryAfterMs = %d/%d", r.ProcessingMs, r.RetryAfterMs)
	}
	if r.ProviderRequestID != "req_abc123" || r.ProviderServer != "alpha" || r.ProviderModel != "model-a-2026-08-06" {
		t.Errorf("provider fields = %q %q %q", r.ProviderRequestID, r.ProviderServer, r.ProviderModel)
	}
	if r.QueueWaitMs != 7 || r.Retries != 2 || !r.RateLimited {
		t.Errorf("queue fields = %d/%d/%v", r.QueueWaitMs, r.Retries, r.RateLimited)
	}
	if r.PromptPreview != "hello world" || r.ResponsePreview != "hi there" {
		t.Errorf("previews = %q %q", r.PromptPreview, r.ResponsePreview)
	}
	if len(r.ToolNames) != 2 || r.ToolNames[0] != "read" || r.ToolNames[1] != "bash" {
		t.Errorf("ToolNames = %v, want [read bash]", r.ToolNames)
	}
	if r.ReqToolsCount != 2 || r.ReqToolChoice != "auto" {
		t.Errorf("ReqToolsCount/ReqToolChoice = %d %q", r.ReqToolsCount, r.ReqToolChoice)
	}

	// Pointer request params must come back set with their original values.
	if r.ReqTemperature == nil || *r.ReqTemperature != 0.7 {
		t.Errorf("ReqTemperature = %v, want 0.7", r.ReqTemperature)
	}
	if r.ReqMaxTokens == nil || *r.ReqMaxTokens != 1024 {
		t.Errorf("ReqMaxTokens = %v, want 1024", r.ReqMaxTokens)
	}
	if r.ReqSeed == nil || *r.ReqSeed != 42 {
		t.Errorf("ReqSeed = %v, want 42", r.ReqSeed)
	}
	if r.ReqParallelTools == nil || !*r.ReqParallelTools {
		t.Errorf("ReqParallelTools = %v, want true", r.ReqParallelTools)
	}
	if r.ReqTopP == nil || *r.ReqTopP != 0.9 {
		t.Errorf("ReqTopP = %v, want 0.9", r.ReqTopP)
	}
	if r.ReqN == nil || *r.ReqN != 1 {
		t.Errorf("ReqN = %v, want 1", r.ReqN)
	}
	if r.ReqPresencePen == nil || *r.ReqPresencePen != 0.5 {
		t.Errorf("ReqPresencePen = %v, want 0.5", r.ReqPresencePen)
	}
	if r.ReqFrequencyPen == nil || *r.ReqFrequencyPen != 0.25 {
		t.Errorf("ReqFrequencyPen = %v, want 0.25", r.ReqFrequencyPen)
	}
	if r.ReqTopLogprobs == nil || *r.ReqTopLogprobs != 5 {
		t.Errorf("ReqTopLogprobs = %v, want 5", r.ReqTopLogprobs)
	}

	// Prompt-composition fields must survive the restart intact.
	if r.CharsSystem != 512 || r.CharsUser != 2048 || r.CharsAssistant != 1024 || r.CharsTool != 256 {
		t.Errorf("Chars* = %d/%d/%d/%d, want 512/2048/1024/256",
			r.CharsSystem, r.CharsUser, r.CharsAssistant, r.CharsTool)
	}
	if r.Images != 2 || r.Attachments != 1 {
		t.Errorf("Images/Attachments = %d/%d, want 2/1", r.Images, r.Attachments)
	}
	if r.LastTurnRole != "user" {
		t.Errorf("LastTurnRole = %q, want user", r.LastTurnRole)
	}
	if r.ReqReasoningEffort != "high" || r.ReqVerbosity != "medium" {
		t.Errorf("effort/verbosity = %q %q", r.ReqReasoningEffort, r.ReqVerbosity)
	}
	if r.ClientMeta.OS != "Linux" || r.ClientMeta.PkgVer != "1.55.0" || r.ClientMeta.Format != "cursor" || r.ClientMeta.MaxConc != 2 {
		t.Errorf("ClientMeta = %+v", r.ClientMeta)
	}
}

// TestNilPointersStayNil verifies that unset (nil) pointer params round-trip
// as nil, so the dashboard JSON (omitempty) keeps the pre-restart shape.
func TestNilPointersStayNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nil.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	rec := &metrics.Record{
		ID: "nil1", Provider: "p", Model: "m", KeyHash: "k", UserAgent: "ua",
		StatusCode: 200, Start: time.Now(),
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadRecent returned %d records, want 1", len(got))
	}
	r := got[0]
	if r.ReqTemperature != nil || r.ReqMaxTokens != nil || r.ReqSeed != nil ||
		r.ReqParallelTools != nil || r.ReqTopP != nil || r.ReqN != nil ||
		r.ReqPresencePen != nil || r.ReqFrequencyPen != nil || r.ReqTopLogprobs != nil {
		t.Errorf("nil pointer params resurrected: %+v", r)
	}
	if len(r.ToolNames) != 0 {
		t.Errorf("ToolNames = %v, want empty", r.ToolNames)
	}
}

func ptr[T any](v T) *T { return &v }

// oldRequestsSchema is the requests table as an early binary created it:
// every column added since (attempts among them) must come from migrate's
// ALTER TABLE pass. Shared by the migration fixtures so they exercise the
// same pre-migration shape a production database with history presents.
const oldRequestsSchema = `
CREATE TABLE requests (
	id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL,
	key_hash TEXT NOT NULL, user_agent TEXT NOT NULL, stream INTEGER NOT NULL,
	status_code INTEGER NOT NULL, started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL,
	ttft_ms INTEGER NOT NULL, first_token_at INTEGER NOT NULL, last_token_at INTEGER NOT NULL,
	finish_reason TEXT NOT NULL, error_type TEXT NOT NULL, error_msg TEXT NOT NULL,
	tool_calls INTEGER NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
	total_tokens INTEGER NOT NULL, cache_read_tokens INTEGER NOT NULL,
	cache_write_tokens INTEGER NOT NULL, reasoning_tokens INTEGER NOT NULL,
	client_disconnected INTEGER NOT NULL
);`

// insertLegacyRow seeds one row against oldRequestsSchema (before any
// migrate pass); every NOT NULL column of that era is named explicitly. The
// status trio used by the migration tests mirrors the canonical IsError
// contract - clean 200, flow-control 429, failed 500 - so an errors-only
// filter must match exactly the 500.
func insertLegacyRow(t *testing.T, db *sql.DB, id string, status int, startedAtMs int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO requests (
		id, provider, model, key_hash, user_agent, stream, status_code,
		started_at, duration_ms, ttft_ms, first_token_at, last_token_at,
		finish_reason, error_type, error_msg, tool_calls, input_tokens,
		output_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
		reasoning_tokens, client_disconnected
	) VALUES (?, 'legacy.example', 'm', 'k', 'ua', 0, ?, ?, 0, 0, 0, 0, '', '', '',
		0, 0, 0, 0, 0, 0, 0, 0)`, id, status, startedAtMs); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAddsNewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(oldRequestsSchema); err != nil {
		t.Fatal(err)
	}
	db0.Close()

	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open after old schema: %v", err)
	}
	defer s.Close()

	rec := &metrics.Record{
		ID: "m1", Provider: "p", Model: "m", KeyHash: "k", UserAgent: "ua",
		StatusCode: 200, Start: time.Now(), End: time.Now(),
		ToolCalls: 1, ErrorCode: "invalid_api_key", RateLimitRemaining: 42, Cost: 0.01,
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err := s2.Query(t.Context(), "SELECT error_code, rate_limit_remaining, cost FROM requests WHERE id='m1'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0]["error_code"] != "invalid_api_key" {
		t.Errorf("error_code = %v", rows[0]["error_code"])
	}
	if rows[0]["rate_limit_remaining"] != int64(42) {
		t.Errorf("rate_limit_remaining = %v", rows[0]["rate_limit_remaining"])
	}
}

// TestMigratePreAttemptsRowsKeepErrorPurgeWorking is the regression for the
// malformed-JSON denial: rows that predate the attempts column receive it
// from migrate's ALTER TABLE, and the HasError purge/count/export predicate
// runs json_each over that column. When the added column defaulted to the empty string
// (not valid JSON), SQLite raised "malformed JSON" and every errors-only
// count, purge, and export failed on any DB with pre-attempts history. The
// migration default must be 'null' - the writer's own encoding of "no
// attempts" (json.Marshal of a nil slice) - and migrated rows must behave
// exactly like current-binary rows.
func TestMigratePreAttemptsRowsKeepErrorPurgeWorking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-attempts.db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(oldRequestsSchema); err != nil {
		t.Fatal(err)
	}
	// Seed history BEFORE the attempts column exists - the exact production
	// shape the old ADD COLUMN populated with its default.
	insertLegacyRow(t, db0, "old-200", 200, 1_700_000_000_000)
	insertLegacyRow(t, db0, "old-429", 429, 1_700_000_001_000)
	insertLegacyRow(t, db0, "old-500", 500, 1_700_000_002_000)
	db0.Close()

	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open after old schema: %v", err)
	}
	defer s.Close()

	// The migrated column must hold valid JSON on every pre-existing row.
	rows, err := s.Query(t.Context(), `SELECT attempts FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("migrated rows = %d, want 3", len(rows))
	}
	for _, row := range rows {
		if row["attempts"] != "null" {
			t.Errorf("attempts = %v on a migrated row, want 'null'", row["attempts"])
		}
	}

	// The repairing boot is one-shot: the marker must now exist so later
	// boots skip the unindexed full-table probe entirely.
	if v, err := s.LoadMeta(t.Context(), attemptsRepairMetaKey); err != nil || v == "" {
		t.Fatalf("attempts repair marker = %q, %v; want written after the repairing boot", v, err)
	}

	// The errors-only count must succeed and match exactly the 500 row
	// (429 is flow control, 200 with no error_type is clean - IsError).
	n, err := s.CountWhere(t.Context(), PurgeFilter{HasError: true})
	if err != nil {
		t.Fatalf("CountWhere(HasError) on pre-attempts rows: %v", err)
	}
	if n != 1 {
		t.Errorf("errors-only count = %d, want 1 (the 500 row)", n)
	}

	// The errors-only export must succeed and stream exactly the 500 row.
	var eb bytes.Buffer
	en, err := s.ExportWhere(t.Context(), PurgeFilter{HasError: true}, &eb)
	if err != nil {
		t.Fatalf("errors-only export on pre-attempts rows: %v", err)
	}
	if en != 1 {
		t.Errorf("errors-only export = %d rows, want 1", en)
	}
	var exported []metrics.Record
	if err := json.Unmarshal(eb.Bytes(), &exported); err != nil {
		t.Fatalf("errors-only export is not a JSON array of records: %v", err)
	}
	if len(exported) != 1 || exported[0].ID != "old-500" {
		t.Errorf("errors-only export = %v, want exactly [old-500]", exported)
	}

	// Migrated rows must read back with nil Attempts via scanRecord.
	recs, err := s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("LoadRecent = %d records, want 3", len(recs))
	}
	for _, r := range recs {
		if r.Attempts != nil {
			t.Errorf("record %s: Attempts = %v, want nil (no attempts)", r.ID, r.Attempts)
		}
	}

	// The errors-only purge must delete exactly the 500 row.
	deleted, err := s.PurgeWhere(t.Context(), PurgeFilter{HasError: true})
	if err != nil {
		t.Fatalf("errors-only purge on pre-attempts rows: %v", err)
	}
	if deleted != 1 {
		t.Errorf("errors-only purge deleted %d rows, want 1", deleted)
	}
	remaining := map[string]bool{}
	recs, err = s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		remaining[r.ID] = true
	}
	if remaining["old-500"] || !remaining["old-200"] || !remaining["old-429"] {
		t.Errorf("after errors-only purge: remaining = %v, want old-200 + old-429 only", remaining)
	}

	// Idempotent: a second Open must be a no-op (the marker makes it skip
	// the probe outright) and the HasError count must still work over the
	// repaired rows.
	s.Close()
	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if v, err := s2.LoadMeta(t.Context(), attemptsRepairMetaKey); err != nil || v == "" {
		t.Fatalf("attempts repair marker after reopen = %q, %v; want it durable", v, err)
	}
	n, err = s2.CountWhere(t.Context(), PurgeFilter{HasError: true})
	if err != nil {
		t.Fatalf("CountWhere(HasError) after reopen: %v", err)
	}
	if n != 0 {
		t.Errorf("errors-only count after purge + reopen = %d, want 0", n)
	}
}

// TestMigrateRepairsLegacyEmptyAttempts pins the one-time data repair for
// databases that already ran the OLD migration: their attempts column
// exists with the empty-string default, so the ADD COLUMN fix alone cannot reach the
// pre-existing rows - only the guarded UPDATE can. It also pins that a
// clean current-schema database passes through the repair untouched.
func TestMigrateRepairsLegacyEmptyAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-empty.db")
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db0.Exec(oldRequestsSchema); err != nil {
		t.Fatal(err)
	}
	// Rows first, then the OLD binary's ADD COLUMN - pre-attempts rows are
	// backfilled with the column default, which was ''.
	insertLegacyRow(t, db0, "old-200", 200, 1_700_000_000_000)
	insertLegacyRow(t, db0, "old-500", 500, 1_700_000_001_000)
	if _, err := db0.Exec(`ALTER TABLE requests ADD COLUMN attempts TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db0.Close()

	s, err := Open(path, testOpts) // migrate must repair '' → 'null'
	if err != nil {
		t.Fatalf("open after legacy-migrated schema: %v", err)
	}
	defer s.Close()

	rows, err := s.Query(t.Context(), `SELECT attempts FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("repaired rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row["attempts"] != "null" {
			t.Errorf("attempts = %v after repair, want 'null'", row["attempts"])
		}
	}
	// The repairing boot writes the one-shot marker.
	if v, err := s.LoadMeta(t.Context(), attemptsRepairMetaKey); err != nil || v == "" {
		t.Fatalf("attempts repair marker = %q, %v; want written after the repairing boot", v, err)
	}
	n, err := s.CountWhere(t.Context(), PurgeFilter{HasError: true})
	if err != nil {
		t.Fatalf("CountWhere(HasError) after repair: %v", err)
	}
	if n != 1 {
		t.Errorf("errors-only count after repair = %d, want 1 (the 500 row)", n)
	}

	// Idempotent: the second Open sees the marker and skips the probe; the
	// repaired rows must stay exactly as the first boot left them.
	s.Close()
	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	rows, err = s2.Query(t.Context(), `SELECT attempts FROM requests`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["attempts"] != "null" {
			t.Errorf("attempts = %v after reopen, want 'null' (repair not stable)", row["attempts"])
		}
	}

	// A clean current-schema database is untouched by the repair: rows keep
	// exactly the attempts JSON the writer produced - a real absorbed-attempt
	// array and the nil 'null' alike.
	cleanPath := filepath.Join(t.TempDir(), "clean.db")
	cs, err := Open(cleanPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	withAttempts := &metrics.Record{
		ID: "att-1", Provider: "alpha.example", Model: "m", KeyHash: "k",
		StatusCode: 200, Start: now, Retries: 1,
		Attempts: []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: now}},
	}
	metrics.FinalizeRecord(withAttempts)
	cs.Record(withAttempts)
	plain := &metrics.Record{ID: "att-2", Provider: "alpha.example", Model: "m", KeyHash: "k", StatusCode: 200, Start: now}
	metrics.FinalizeRecord(plain)
	cs.Record(plain)
	cs.Flush()
	// A clean probe writes the marker too: the first Open pays the one
	// scan, every later boot reads one meta row.
	if v, err := cs.LoadMeta(t.Context(), attemptsRepairMetaKey); err != nil || v == "" {
		t.Fatalf("attempts repair marker on a clean DB = %q, %v; want written after the clean probe", v, err)
	}
	cs.Close()

	cs2, err := Open(cleanPath, testOpts) // repair probe must skip this DB
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	recs, err := cs2.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*metrics.Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	if len(byID) != 2 {
		t.Fatalf("clean DB rows = %d, want 2", len(byID))
	}
	if r := byID["att-1"]; r == nil || len(r.Attempts) != 1 || r.Attempts[0].StatusCode != 502 {
		t.Errorf("att-1 Attempts = %+v, want the untouched 502 attempt (repair clobbered real attempts)", r)
	}
	if r := byID["att-2"]; r == nil || r.Attempts != nil {
		t.Errorf("att-2 Attempts = %+v, want nil", r)
	}
}

// TestMigrateRepairProbeIsOneShot unit-pins the marker semantics of the
// attempts repair, straight against migrate: the unindexed full-table probe
// runs only while the marker is absent. Once written, a ” row that appears
// afterwards is deliberately NOT re-repaired (the probe is skipped); a
// database restored from a pre-marker backup (marker absent) probes and
// repairs again.
func TestMigrateRepairProbeIsOneShot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(oldRequestsSchema); err != nil {
		t.Fatal(err)
	}
	insertLegacyRow(t, db, "legacy-500", 500, 1_700_000_000_000)
	if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN attempts TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	// migrate's meta read assumes the schema pass already ran (Open order).
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	attempts := func() string {
		var v string
		if err := db.QueryRow(`SELECT attempts FROM requests WHERE id = 'legacy-500'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	marker := func() string {
		var v string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, attemptsRepairMetaKey).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return v
	}

	// First migrate: probe finds the '' row, repairs it, writes the marker.
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if got := attempts(); got != "null" {
		t.Fatalf("attempts after first migrate = %q, want 'null'", got)
	}
	if got := marker(); got == "" {
		t.Fatal("repair marker absent after the repairing migrate")
	}

	// A '' row that appears AFTER the marker stays untouched: the probe is
	// one-shot, later boots skip it entirely.
	if _, err := db.Exec(`UPDATE requests SET attempts = ''`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if got := attempts(); got != "" {
		t.Fatalf("attempts after a post-marker migrate = %q, want '' (probe must be skipped)", got)
	}

	// The restored-backup edge: a backup predating the marker carries no
	// marker row, so the probe re-arms and repairs again.
	if _, err := db.Exec(`DELETE FROM meta WHERE key = ?`, attemptsRepairMetaKey); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if got := attempts(); got != "null" {
		t.Fatalf("attempts after marker removal = %q, want 'null' (probe must re-arm)", got)
	}
	if got := marker(); got == "" {
		t.Fatal("repair marker absent after the re-armed probe")
	}
}

func TestStoreNeverBlocksOnOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rec := &metrics.Record{ID: "x", Provider: "p", Model: "m"}
	for i := 0; i < 20000; i++ {
		s.Record(rec)
	}
}

func TestHandleQueryRejectsNonSelect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	reject := []string{
		"PRAGMA table_info(requests)",
		"DROP TABLE requests",
		"  delete from requests",
		"INSERT INTO requests (id) VALUES ('x')",
		"SELECT * FROM requests; DROP TABLE requests", // multi-statement
		"WITH x AS (SELECT 1) SELECT * FROM x",        // not SELECT-leading
		"UPDATE requests SET model='x'",
	}
	for _, q := range reject {
		req := httptest.NewRequest("GET", "/metrics/query?q="+url.QueryEscape(q), nil)
		w := httptest.NewRecorder()
		s.HandleQuery(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("q=%q: status = %d, want 400", q, w.Code)
		}
	}

	// A legit SELECT works and returns JSON.
	req := httptest.NewRequest("GET", "/metrics/query?q="+url.QueryEscape("SELECT COUNT(*) AS n FROM requests"), nil)
	w := httptest.NewRecorder()
	s.HandleQuery(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("valid SELECT: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	// No permissive CORS header on the query endpoint.
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want unset", got)
	}

	post := httptest.NewRequest(http.MethodPost, "/metrics/query?q="+url.QueryEscape("SELECT 1"), nil)
	pw := httptest.NewRecorder()
	s.HandleQuery(pw, post)
	if pw.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST query: status = %d, want 405", pw.Code)
	}
	if got := pw.Header().Get("Allow"); got != "GET" {
		t.Errorf("POST query Allow = %q, want GET", got)
	}
}

func TestRecordAfterCloseDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Recording after Close must not panic; it's counted as a drop.
	s.Record(&metrics.Record{ID: "late", Provider: "p", Model: "m"})
	if s.Dropped() == 0 {
		t.Errorf("Dropped = 0, want >= 1 for a record sent after Close")
	}
}

func TestClientIdentitySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	rec := &metrics.Record{
		ID:         "c1",
		Provider:   "alpha",
		Model:      "model-a",
		KeyHash:    "k",
		UserAgent:  "client-a/1.0.0",
		Client:     "client-a/1.0.0",
		ClientIP:   "203.0.113.7",
		ClientLang: "go",
		StatusCode: 200,
		Start:      time.Now(),
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	// Reopen (simulates a restart) and backfill via the real path.
	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadRecent returned %d records, want 1", len(got))
	}
	r := got[0]
	if r.Client != "client-a/1.0.0" {
		t.Errorf("Client = %q, want client-a/1.0.0 (client identity lost on restart)", r.Client)
	}
	if r.ClientIP != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want 203.0.113.7", r.ClientIP)
	}
	if r.ClientLang != "go" {
		t.Errorf("ClientLang = %q, want go", r.ClientLang)
	}
}

func TestAttemptsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "att.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	rec := &metrics.Record{
		ID: "att1", Provider: "alpha.example", Model: "model-a", KeyHash: "k",
		StatusCode: 200, Start: time.Now(), Retries: 2,
		FinalAttemptAt: time.Now().Add(2 * time.Second),
		Attempts: []metrics.RetryAttempt{
			{StatusCode: 502, ErrorType: "bad_gateway", ErrorCode: "bg1", ErrorMsg: "upstream overloaded", At: time.Now()},
			{StatusCode: 503, ErrorType: "service_unavailable", ErrorMsg: "try later", RetryAfterMs: 1000, At: time.Now()},
		},
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.LoadRecent(t.Context(), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %v n=%d", err, len(got))
	}
	atts := got[0].Attempts
	if len(atts) != 2 {
		t.Fatalf("Attempts = %d, want 2", len(atts))
	}
	if atts[0].StatusCode != 502 || atts[0].ErrorType != "bad_gateway" || atts[0].ErrorCode != "bg1" {
		t.Errorf("attempt[0] = %+v", atts[0])
	}
	if atts[1].StatusCode != 503 || atts[1].RetryAfterMs != 1000 || atts[1].ErrorMsg != "try later" {
		t.Errorf("attempt[1] = %+v", atts[1])
	}
	if got[0].FinalAttemptAt.IsZero() {
		t.Errorf("FinalAttemptAt lost on restart (zero), want preserved")
	}
}

func TestConversationIDSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	s, _ := Open(path, testOpts)
	rec := &metrics.Record{ID: "cv1", Provider: "alpha.example", Model: "model-a", KeyHash: "k",
		StatusCode: 200, Start: time.Now(), ConversationID: "c-abc123def4"}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, _ := Open(path, testOpts)
	defer s2.Close()
	got, err := s2.LoadRecent(t.Context(), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %v n=%d", err, len(got))
	}
	if got[0].ConversationID != "c-abc123def4" {
		t.Errorf("ConversationID = %q, want c-abc123def4 (lost on restart)", got[0].ConversationID)
	}
}

func TestDurationPreservedAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dur.db")
	s, _ := Open(path, testOpts)
	start := time.Now()
	rec := &metrics.Record{ID: "d1", Provider: "p", Model: "m", KeyHash: "k",
		StatusCode: 200, Start: start, End: start.Add(7 * time.Second)}
	metrics.FinalizeRecord(rec)
	if rec.DurationMs != 7000 {
		t.Fatalf("DurationMs before store = %d, want 7000", rec.DurationMs)
	}
	s.Record(rec)
	s.Close()

	s2, _ := Open(path, testOpts)
	defer s2.Close()
	got, err := s2.LoadRecent(t.Context(), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %v n=%d", err, len(got))
	}
	// DurationMs must be restored (not 0) so the dashboard shows it after restart.
	if got[0].DurationMs != 7000 {
		t.Errorf("DurationMs after restart = %d, want 7000", got[0].DurationMs)
	}
	if d := got[0].End.Sub(got[0].Start).Milliseconds(); d != 7000 {
		t.Errorf("End-Start after restart = %d, want 7000", d)
	}
}

// TestReadPoolIsReadOnly pins the defense-in-depth invariant: the dashboard
// read pool must reject writes at the DB layer (mode=ro), so a mutation that
// somehow passed validateQuery still fails. Regression test for the modernc
// driver ignoring "path?mode=ro" without the "file:" URI scheme.
func TestReadPoolIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.rdb.Exec("DELETE FROM requests"); err == nil {
		t.Error("read pool allowed a DELETE; want mode=ro to reject writes")
	}
}

// TestBusyTimeoutAbsorbsConcurrentWriter is the regression for the
// restart-handoff straggler window: a second writer briefly holding the
// SQLite write lock (the draining parent committing a late batch while the
// fresh child's Open runs schema/migrate on the same file) must be ABSORBED
// by a bounded busy_timeout, not fail immediately with SQLITE_BUSY. It also
// pins the guardrail's honesty: a lock held PAST the timeout still fails
// closed (the batch drops and is counted), so busy_timeout can never mask a
// permanently stuck writer.
func TestBusyTimeoutAbsorbsConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	// Bootstrap the file the way the draining parent leaves it (schema + WAL).
	boot, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	boot.Close()

	// The straggler writer: a raw connection holding the SQLite write lock
	// (BEGIN IMMEDIATE + a dirty INSERT, not committed).
	hold, err := sql.Open("sqlite", path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	acquire := func() *sql.Tx {
		t.Helper()
		tx, err := hold.Begin() // BEGIN IMMEDIATE via _txlock
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('hold', '1')`); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	opts := testOpts
	opts.FlushInterval = 20 * time.Millisecond // flush attempts hit the held lock deterministically

	// Phase 1: Open (schema/migrate) must survive a held write lock. The
	// store opens in a goroutine because the busy wait blocks inside Open.
	tx := acquire()
	type openRes struct {
		s   *Store
		err error
	}
	opened := make(chan openRes, 1)
	go func() {
		s, err := Open(path, opts)
		opened <- openRes{s, err}
	}()
	time.Sleep(200 * time.Millisecond) // lock stays held while the child opens
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var res openRes
	select {
	case res = <-opened:
	case <-time.After(sqliteBusyTimeout + 5*time.Second):
		t.Fatal("Open never returned after the lock was released")
	}
	if res.err != nil {
		t.Fatalf("open under a held write lock: %v", res.err)
	}
	s := res.s
	defer s.Close()

	// Phase 2: a Record flushed WHILE the lock is held must commit once
	// the lock frees, with zero durable-loss drops.
	rec := &metrics.Record{ID: "straggler-1", Provider: "p", Model: "m",
		KeyHash: "k", StatusCode: 200, Start: time.Now()}
	metrics.FinalizeRecord(rec)
	tx = acquire()
	s.Record(rec)
	time.Sleep(150 * time.Millisecond) // a flush attempt fires against the held lock
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		recs, err := s.LoadRecent(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range recs {
			if r.ID == "straggler-1" {
				found = true
			}
		}
		if found && s.Dropped() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record not durable after lock release: found=%v dropped=%d (want committed with 0 drops)", found, s.Dropped())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Phase 3: a lock held PAST the timeout must still fail closed - the
	// batch drops and is counted; busy_timeout delays SQLITE_BUSY, never
	// masks it. The failed batch is durable loss by design: the record
	// never lands.
	rec2 := &metrics.Record{ID: "straggler-2", Provider: "p", Model: "m",
		KeyHash: "k", StatusCode: 200, Start: time.Now()}
	metrics.FinalizeRecord(rec2)
	tx = acquire()
	start := time.Now()
	s.Record(rec2)
	deadline = start.Add(sqliteBusyTimeout + 5*time.Second)
	for s.Dropped() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("permanently-held lock never failed closed (busy_timeout masked it)")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if waited := time.Since(start); waited < sqliteBusyTimeout-1*time.Second {
		t.Errorf("gave up after %s, want >= ~%s (the busy window was not honored)", waited, sqliteBusyTimeout)
	}
	recs, err := s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == "straggler-2" {
			t.Error("dropped record straggler-2 is somehow durable")
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// TestValidateQueryDeniesAttach pins the fail-closed choke point: ATTACH (which
// SQLite honors even on a mode=ro connection, opening the attached DB writable)
// and other side-effecting keywords are rejected at the query boundary.
func TestValidateQueryDeniesAttach(t *testing.T) {
	for _, q := range []string{
		"SELECT 1; ATTACH DATABASE '/tmp/x.db' AS aux",
		"SELECT * FROM requests WHERE id='x' ATTACH DATABASE '/tmp/x.db' AS aux",
		"SELECT load_extension('/tmp/evil')",
		"SELECT * FROM requests; DROP TABLE requests",
		"PRAGMA writable_schema=ON",
		"ATTACH DATABASE '/tmp/x.db' AS aux",
	} {
		if err := validateQuery(q); err == nil {
			t.Errorf("validateQuery(%q) = nil, want rejection", q)
		}
	}
	// A legitimate SELECT still passes; a column named "attachment" is not a
	// false positive for the ATTACH keyword.
	for _, q := range []string{
		"SELECT provider, COUNT(*) FROM requests GROUP BY provider",
		"SELECT attachment FROM requests",
	} {
		if err := validateQuery(q); err != nil {
			t.Errorf("validateQuery(%q) = %v, want nil", q, err)
		}
	}
}

func TestPurgeEmptiesStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purge.db")
	s, _ := Open(path, testOpts)
	for _, id := range []string{"p1", "p2", "p3"} {
		r := &metrics.Record{ID: id, Provider: "p", Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()}
		metrics.FinalizeRecord(r)
		s.Record(r)
	}
	s.Close()

	s2, _ := Open(path, testOpts)
	defer s2.Close()
	before, _ := s2.LoadRecent(t.Context(), 100)
	if len(before) != 3 {
		t.Fatalf("before purge: %d records, want 3", len(before))
	}
	if err := s2.Purge(t.Context()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	after, _ := s2.LoadRecent(t.Context(), 100)
	if len(after) != 0 {
		t.Errorf("after purge: %d records, want 0", len(after))
	}
}

// seedPurgeStore writes a fixed set of records synchronously (bypassing the
// async write channel) so purge tests are deterministic.
func seedPurgeStore(t *testing.T, s *Store) {
	t.Helper()
	mk := func(id, provider string, status int, errType string) *metrics.Record {
		r := &metrics.Record{
			ID: id, Provider: provider, Model: "m", Client: "c", KeyHash: "k",
			StatusCode: status, ErrorType: errType, Start: time.Now(),
		}
		metrics.FinalizeRecord(r)
		return r
	}
	recs := []*metrics.Record{
		mk("ok-1", "alpha", 200, ""),
		mk("ok-2", "alpha", 200, ""),
		mk("rl-1", "alpha", 429, "rate_limit"), // 429 = flow control, NOT an error
		mk("err-2", "beta", 500, ""),
		mk("err-3", "beta", 200, "transport"), // status 0-style: error_type set, code < 400
		// Recovered after an absorbed 5xx attempt: final 200 but the upstream
		// genuinely failed once - counts as an error under the new semantics.
		func() *metrics.Record {
			r := mk("ret-5xx", "alpha", 200, "")
			r.Retries = 1
			r.Attempts = []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: time.Now()}}
			return r
		}(),
		// Recovered after an absorbed 429: flow control only, NOT an error.
		func() *metrics.Record {
			r := mk("ret-429", "alpha", 200, "")
			r.Retries = 1
			r.Attempts = []metrics.RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit", At: time.Now()}}
			return r
		}(),
		// 499 = local client closed the connection (its own cancellation):
		// flow event, NOT an error.
		func() *metrics.Record {
			r := mk("cancel-1", "alpha", metrics.StatusClientClosedRequest, "")
			r.ClientDisconnected = true
			return r
		}(),
		// 499 AFTER an absorbed 5xx: the upstream genuinely failed once, so it
		// still counts as an error (the cancel doesn't mask the failure).
		func() *metrics.Record {
			r := mk("cancel-5xx", "alpha", metrics.StatusClientClosedRequest, "")
			r.ClientDisconnected = true
			r.Retries = 1
			r.Attempts = []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: time.Now()}}
			return r
		}(),
	}
	if err := s.insertBatch(recs); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestPurgeWhereEmptyFilterRejected pins the deny-by-default invariant: an
// empty filter must error and delete nothing, so a malformed filter can never
// silently become a full wipe.
func TestPurgeWhereEmptyFilterRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purge-empty.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPurgeStore(t, s)

	if _, err := s.PurgeWhere(t.Context(), PurgeFilter{}); err == nil {
		t.Fatal("PurgeWhere(empty) = nil error, want rejection")
	}
	n, err := s.CountWhere(t.Context(), PurgeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Errorf("after rejected purge: %d rows, want 9 (nothing deleted)", n)
	}
}

// TestPurgeWhereFiltersRows verifies a filtered purge deletes exactly the
// matching rows and leaves the rest.
func TestPurgeWhereFiltersRows(t *testing.T) {
	cases := []struct {
		name     string
		filter   PurgeFilter
		wantGone []string
		wantKept []string
	}{
		{
			name:     "by provider",
			filter:   PurgeFilter{Provider: "alpha"},
			wantGone: []string{"ok-1", "ok-2", "rl-1", "ret-5xx", "ret-429", "cancel-1", "cancel-5xx"},
			wantKept: []string{"err-2", "err-3"},
		},
		{
			name:     "by status code",
			filter:   PurgeFilter{StatusCode: 200},
			wantGone: []string{"ok-1", "ok-2", "err-3", "ret-5xx", "ret-429"},
			wantKept: []string{"rl-1", "err-2", "cancel-1", "cancel-5xx"},
		},
		{
			// HasError must match exactly the records IsError() flags - 429
			// (rl-1), an absorbed 429 (ret-429), and a plain 499 client-close
			// (cancel-1) are flow events, NOT errors; 500 (err-2), a structured
			// error_type (err-3), an absorbed 5xx (ret-5xx), and a 499 that
			// came after an absorbed 5xx (cancel-5xx - the upstream still
			// failed) ARE. This pins the SQL predicate to the canonical Go one.
			name:     "has error matches IsError",
			filter:   PurgeFilter{HasError: true},
			wantGone: []string{"err-2", "err-3", "ret-5xx", "cancel-5xx"},
			wantKept: []string{"ok-1", "ok-2", "rl-1", "ret-429", "cancel-1"},
		},
		{
			name:     "combined provider+status",
			filter:   PurgeFilter{Provider: "alpha", StatusCode: 200},
			wantGone: []string{"ok-1", "ok-2", "ret-5xx", "ret-429"},
			wantKept: []string{"rl-1", "err-2", "err-3", "cancel-1", "cancel-5xx"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "purge-filter.db")
			s, err := Open(path, testOpts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			seedPurgeStore(t, s)

			deleted, err := s.PurgeWhere(t.Context(), tc.filter)
			if err != nil {
				t.Fatalf("PurgeWhere: %v", err)
			}
			if int(deleted) != len(tc.wantGone) {
				t.Errorf("deleted = %d, want %d", deleted, len(tc.wantGone))
			}
			remaining, err := s.LoadRecent(t.Context(), 100)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, r := range remaining {
				got[r.ID] = true
			}
			for _, id := range tc.wantGone {
				if got[id] {
					t.Errorf("row %q survived purge, want deleted", id)
				}
			}
			for _, id := range tc.wantKept {
				if !got[id] {
					t.Errorf("row %q deleted, want kept", id)
				}
			}
		})
	}
}

// TestPurgeWhereMatchRecordParity pins SQL and in-memory purge predicates to
// the same result set, so the live ring and the database can never disagree
// about what a filter deletes.
func TestPurgeWhereMatchRecordParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purge-parity.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPurgeStore(t, s)

	filters := []PurgeFilter{
		{Provider: "alpha"},
		{StatusCode: 200},
		{HasError: true},
		{Provider: "alpha", StatusCode: 200},
		{ErrorType: "transport"},
		{StatusCode: metrics.StatusClientClosedRequest},
	}
	all, err := s.LoadRecent(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range filters {
		// In-memory expectation.
		var wantMatch []string
		for _, r := range all {
			if f.MatchRecord(r) {
				wantMatch = append(wantMatch, r.ID)
			}
		}
		// SQL count must equal the in-memory match count.
		n, err := s.CountWhere(t.Context(), f)
		if err != nil {
			t.Fatalf("CountWhere(%+v): %v", f, err)
		}
		if int(n) != len(wantMatch) {
			t.Errorf("filter %+v: SQL matches %d rows, MatchRecord matches %d", f, n, len(wantMatch))
		}
	}
}

// TestExportWhereFiltersAndStreams pins the log-export contract: an empty
// filter exports EVERYTHING, a filtered export streams exactly the rows the
// purge/count predicate matches (never fewer - the dashboard's download
// preview and the download itself share this code path), and the output is a
// valid JSON array of complete records.
func TestExportWhereFiltersAndStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPurgeStore(t, s) // 9 rows: see the purge tests for the fixture's shape

	// Full export: all 9 rows, valid JSON, records round-trip.
	var all bytes.Buffer
	n, err := s.ExportWhere(t.Context(), PurgeFilter{}, &all)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Errorf("full export = %d rows, want 9", n)
	}
	var recs []metrics.Record
	if err := json.Unmarshal(all.Bytes(), &recs); err != nil {
		t.Fatalf("export is not a JSON array of records: %v\n%s", err, all.String()[:min(len(all.String()), 200)])
	}
	if len(recs) != 9 {
		t.Errorf("full export decoded %d records, want 9", len(recs))
	}

	// Filtered export: same predicate as CountWhere/MatchRecord.
	var fb bytes.Buffer
	n, err = s.ExportWhere(t.Context(), PurgeFilter{Provider: "alpha"}, &fb)
	if err != nil {
		t.Fatal(err)
	}
	var got []metrics.Record
	if err := json.Unmarshal(fb.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantN, _ := s.CountWhere(t.Context(), PurgeFilter{Provider: "alpha"})
	if n != wantN || len(got) != int(wantN) {
		t.Errorf("filtered export = %d rows (%d decoded), CountWhere = %d - must agree", n, len(got), wantN)
	}
	for _, r := range got {
		if r.Provider != "alpha" {
			t.Errorf("exported row %s with provider %q, want only alpha", r.ID, r.Provider)
		}
	}

	// Errors-only export follows the canonical IsError predicate.
	var eb bytes.Buffer
	n, err = s.ExportWhere(t.Context(), PurgeFilter{HasError: true}, &eb)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("errors export = %d rows, want 4 (err-2, err-3, ret-5xx, cancel-5xx)", n)
	}

	debugRec := &metrics.Record{
		ID: "dbg-1", Provider: "gamma", Model: "m", Client: "c", KeyHash: "k",
		StatusCode: 200, Start: time.Now(), Debug: true, DebugSessionID: "sess",
	}
	metrics.FinalizeRecord(debugRec)
	if err := s.insertBatch([]*metrics.Record{debugRec}); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"schema":"millivolt.debug/v1","id":"dbg-1"}`)
	if err := s.SaveDebugCapture(t.Context(), "dbg-1", "sess", time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), payload); err != nil {
		t.Fatal(err)
	}
	var db bytes.Buffer
	n, err = s.ExportWhere(t.Context(), PurgeFilter{HasDebug: true}, &db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("debug export = %d rows, want 1", n)
	}
	if !bytes.Contains(db.Bytes(), []byte(`"debug_capture"`)) {
		t.Errorf("debug export missing sidecar: %s", db.String())
	}
}

func TestQueryBeforeNewestFirstExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qbefore.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.UnixMilli(1_700_000_000_000)
	recs := make([]*metrics.Record, 5)
	for i := 0; i < 5; i++ {
		r := &metrics.Record{
			ID: "q" + string(rune('0'+i)), Provider: "p", Model: "m", KeyHash: "k",
			StatusCode: 200, Start: base.Add(time.Duration(i) * time.Second),
		}
		metrics.FinalizeRecord(r)
		recs[i] = r
	}
	if err := s.insertBatch(recs); err != nil {
		t.Fatal(err)
	}

	got, err := s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[4].Start.UnixMilli()}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("len=%d, want 4 (exclusive of the cursor row)", len(got))
	}
	want := []string{"q3", "q2", "q1", "q0"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("got[%d]=%s, want %s", i, got[i].ID, id)
		}
	}

	got, err = s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[4].Start.UnixMilli()}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "q3" || got[1].ID != "q2" {
		t.Fatalf("limit 2 = [%s %s], want [q3 q2]", got[0].ID, got[1].ID)
	}

	none, err := s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[0].Start.UnixMilli()}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("before oldest = %d, want 0", len(none))
	}

	z, err := s.QueryBefore(t.Context(), &LogCursor{StartMs: recs[4].Start.UnixMilli()}, 0)
	if err != nil || z != nil {
		t.Fatalf("limit 0 = %v, %v; want nil, nil", z, err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRenameProvidersMergesHistory pins the provider_aliases repair: rows
// recorded under an old label are rewritten to the canonical one in place,
// other rows are untouched, the operation is idempotent, and a malformed pair
// is a no-op (config Validate rejects those - this is the last-chance deny).
// Labels are neutral fixtures; the mechanism is provider-agnostic.
func TestRenameProvidersMergesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alias.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, provider string) *metrics.Record {
		r := &metrics.Record{ID: id, Provider: provider, Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()}
		metrics.FinalizeRecord(r)
		return r
	}
	for _, r := range []*metrics.Record{mk("a-1", "old.example"), mk("a-2", "old.example"), mk("b-1", "new.example")} {
		s.Record(r)
	}
	s.Flush()
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, err := s2.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("renamed %d rows, want 2", n)
	}
	recs, err := s2.LoadRecent(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	byProvider := map[string]int{}
	for _, r := range recs {
		byProvider[r.Provider]++
	}
	if byProvider["new.example"] != 3 || len(byProvider) != 1 {
		t.Fatalf("post-rename providers = %v, want all 3 rows under new.example", byProvider)
	}
	// Idempotent: re-running the same merge renames nothing.
	again, err := s2.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"})
	if err != nil || again != 0 {
		t.Fatalf("second rename = %d, %v; want 0, nil", again, err)
	}
	// A malformed pair is skipped, never applied.
	bad, err := s2.RenameProviders(t.Context(), map[string]string{"": "new.example", "new.example": "new.example"})
	if err != nil || bad != 0 {
		t.Fatalf("malformed aliases = %d, %v; want 0, nil", bad, err)
	}
}
