package storage

import (
	"database/sql"
	"path/filepath"
	"reflect"
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
		QueryMaxBytes: int(d.StorageQueryMaxBytes),
		QueryMaxRows:  d.StorageQueryMaxRows,
	}
}()

func TestStoreRoundTrip(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	rec := &metrics.Record{
		ID:             "r1",
		Provider:       "alpha",
		Model:          "model-a",
		KeyHash:        "abc",
		UserAgent:      "test",
		Stream:         true,
		StatusCode:     200,
		Start:          now,
		FinalAttemptAt: now,
		End:            now.Add(time.Second),
		FirstTokenAt:   now.Add(100 * time.Millisecond),
		LastTokenAt:    now.Add(900 * time.Millisecond),
		FinishReason:   "stop",
		ToolCalls:      2,
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

// recordReopenLoadOne drives the restart epilogue the record-survival tests
// share: finalize rec, record it, close the store, reopen the database at
// path with testOpts (simulating a restart), and return the single record
// LoadRecent must surface. The caller owns the record's fields and every
// survival assertion.
func recordReopenLoadOne(t *testing.T, s *Store, path string, rec *metrics.Record) *metrics.Record {
	t.Helper()
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
	return got[0]
}

// TestAllFieldsSurviveRestart populates every persistable Record field,
// closes the store, reopens it (simulating a restart), and asserts the fields
// round-trip through LoadRecent - including JSON-encoded tool names and the
// pointer request params.
func TestAllFieldsSurviveRestart(t *testing.T) {
	path := testDatabase(t)
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
		FirstReasoningAt:   now.Add(100 * time.Millisecond).UnixMilli(),
		Chunks:             64,
		RequestBytes:       4096,
		ResponseBytes:      8192,
		UpstreamGzip:       true,
		UpstreamConnectMs:  12,
		UpstreamTLSMs:      34,
		UpstreamTTFBMs:     120,
		UpstreamConnReused: true,
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
	r := recordReopenLoadOne(t, s, path, rec)

	// Representative spot-checks across every field group.
	if r.AnswerTokens != 30 {
		t.Errorf("AnswerTokens = %d, want 30", r.AnswerTokens)
	}
	// GenTokens + HadAnswerContent: the gen_tps numerator and its content-
	// presence gate must survive the restart (the dashboard recomputes nothing).
	if r.GenTokens != 50 {
		t.Errorf("GenTokens = %d, want 50 (reasoning 20 + answer 30, derived by Finalize)", r.GenTokens)
	}
	if !r.HadAnswerContent {
		t.Error("HadAnswerContent = false, want true (the tool-call-only gate)")
	}
	if !r.FirstAnswerAt.Equal(now.Add(800 * time.Millisecond)) {
		t.Errorf("FirstAnswerAt = %v, want %v", r.FirstAnswerAt, now.Add(800*time.Millisecond))
	}
	// The analyzer's per-stream telemetry survives the restart as its own
	// columns: the content-bearing chunk count and the first reasoning-token
	// time (unix ms; 0 = no reasoning block).
	if r.Chunks != 64 {
		t.Errorf("Chunks = %d, want 64", r.Chunks)
	}
	if r.FirstReasoningAt != now.Add(100*time.Millisecond).UnixMilli() {
		t.Errorf("FirstReasoningAt = %d, want %d", r.FirstReasoningAt, now.Add(100*time.Millisecond).UnixMilli())
	}
	// The wire-size trio survives the restart as its own columns: the
	// inbound request body size, the relayed response body size, and the
	// transport's auto-decompress fact.
	if r.RequestBytes != 4096 || r.ResponseBytes != 8192 {
		t.Errorf("RequestBytes/ResponseBytes = %d/%d, want 4096/8192", r.RequestBytes, r.ResponseBytes)
	}
	if !r.UpstreamGzip {
		t.Error("UpstreamGzip = false, want true (the transport auto-decompressed the upstream body)")
	}
	// The adopted attempt's httptrace decomposition survives the restart as
	// its own columns: dial, TLS handshake, header-wait and the pool-hit flag.
	if r.UpstreamConnectMs != 12 || r.UpstreamTLSMs != 34 || r.UpstreamTTFBMs != 120 {
		t.Errorf("UpstreamConnectMs/TLSMs/TTFBMs = %d/%d/%d, want 12/34/120", r.UpstreamConnectMs, r.UpstreamTLSMs, r.UpstreamTTFBMs)
	}
	if !r.UpstreamConnReused {
		t.Error("UpstreamConnReused = false, want true (the adopted attempt rode a pooled connection)")
	}
	if r.DecodeTPS == 0 {
		t.Errorf("TPS field lost: DecodeTPS=%v", r.DecodeTPS)
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
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	rec := &metrics.Record{
		ID: "nil1", Provider: "p", Model: "m", KeyHash: "k", UserAgent: "ua",
		StatusCode: 200, Start: time.Now(),
	}
	r := recordReopenLoadOne(t, s, path, rec)
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

func TestStoreNeverBlocksOnOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	// A tiny channel cap overflows after a handful of records, so the
	// nonblocking guarantee is exercised with a small fixture instead of
	// pushing past the production 8192 cap.
	opts := testOpts
	opts.WriteChanCap = 8
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rec := &metrics.Record{ID: "x", Provider: "p", Model: "m"}
	for i := 0; i < 200; i++ {
		s.Record(rec)
	}
}

func TestRecordAfterCloseDoesNotPanic(t *testing.T) {
	path := testDatabase(t)
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
	path := testDatabase(t)
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
	r := recordReopenLoadOne(t, s, path, rec)
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
	path := testDatabase(t)
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
	r := recordReopenLoadOne(t, s, path, rec)
	atts := r.Attempts
	if len(atts) != 2 {
		t.Fatalf("Attempts = %d, want 2", len(atts))
	}
	if atts[0].StatusCode != 502 || atts[0].ErrorType != "bad_gateway" || atts[0].ErrorCode != "bg1" {
		t.Errorf("attempt[0] = %+v", atts[0])
	}
	if atts[1].StatusCode != 503 || atts[1].RetryAfterMs != 1000 || atts[1].ErrorMsg != "try later" {
		t.Errorf("attempt[1] = %+v", atts[1])
	}
	if r.FinalAttemptAt.IsZero() {
		t.Errorf("FinalAttemptAt lost on restart (zero), want preserved")
	}
}

func TestConversationIDSurvivesRestart(t *testing.T) {
	path := testDatabase(t)
	s, _ := Open(path, testOpts)
	rec := &metrics.Record{ID: "cv1", Provider: "alpha.example", Model: "model-a", KeyHash: "k",
		StatusCode: 200, Start: time.Now(), ConversationID: "c-abc123def4"}
	r := recordReopenLoadOne(t, s, path, rec)
	if r.ConversationID != "c-abc123def4" {
		t.Errorf("ConversationID = %q, want c-abc123def4 (lost on restart)", r.ConversationID)
	}
}

func TestDurationPreservedAcrossRestart(t *testing.T) {
	path := testDatabase(t)
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

// TestBusyTimeoutAbsorbsConcurrentWriter is the regression for the
// restart-handoff straggler window: a second writer briefly holding the
// SQLite write lock (the draining parent committing a late batch while the
// fresh child's Open runs schema/migrate on the same file) must be ABSORBED
// by a bounded busy_timeout, not fail immediately with SQLITE_BUSY. It also
// pins the guardrail's honesty: a lock held PAST the timeout still fails
// closed (the batch drops and is counted), so busy_timeout can never mask a
// permanently stuck writer.
func TestBusyTimeoutAbsorbsConcurrentWriter(t *testing.T) {
	path := testDatabase(t)
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
	// A shortened busy window (the production 5s guardrail would cost 5s
	// per phase): 250ms is far above the ms-scale lock holds below while
	// keeping the honor check meaningful at its rescaled bound.
	opts.BusyTimeout = 250 * time.Millisecond
	busy := opts.BusyTimeout

	// Phase 1: Open (schema/migrate) must survive a held write lock. The
	// store opens in a goroutine because the busy wait blocks inside Open.
	// The 200ms hold has no pollable "Open is now waiting" signal, so it
	// stays a fixed window: it must outlast Open's ms-scale startup (a
	// shorter hold could let the child open after the rollback and pass
	// this phase vacuously) while staying inside the busy window.
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
	case <-time.After(busy + 5*time.Second):
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
	// Four flush windows at the 20ms cadence: the writer's flush attempt
	// starts within one interval of the Record and is still parked on the
	// held lock when the rollback lands 80ms in.
	time.Sleep(80 * time.Millisecond) // a flush attempt fires against the held lock
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
	deadline = start.Add(busy + 5*time.Second)
	for s.Dropped() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("permanently-held lock never failed closed (busy_timeout masked it)")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// The rescaled honor bound keeps the original's 20% slack: the drop can
	// only land after the full busy window expires on the writer's flush
	// attempt, so anything under 200ms means the window was not honored.
	if waited := time.Since(start); waited < busy-50*time.Millisecond {
		t.Errorf("gave up after %s, want >= ~%s (the busy window was not honored)", waited, busy)
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

// TestRenameProvidersMergesHistory pins the provider_aliases repair: rows
// recorded under an old label are rewritten to the canonical one in place,
// other rows are untouched, the operation is idempotent, and a malformed pair
// is a no-op (config Validate rejects those - this is the last-chance deny).
// Labels are neutral fixtures; the mechanism is provider-agnostic.
func TestRenameProvidersMergesHistory(t *testing.T) {
	path := testDatabase(t)
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

// TestRenameProvidersIsAtomicAcrossAliases pins the all-or-nothing contract: a
// trigger that rejects the second alias (sorted by source label) must roll the
// first alias back, so history is never left half-merged while the caller
// reports failure. Dropping the trigger then proves the same pairs still apply
// together.
func TestRenameProvidersIsAtomicAcrossAliases(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(id, provider string) *metrics.Record {
		r := &metrics.Record{ID: id, Provider: provider, Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()}
		metrics.FinalizeRecord(r)
		return r
	}
	s.Record(mk("a-1", "a-old.example"))
	s.Record(mk("b-1", "b-old.example"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_before_rename
		BEFORE UPDATE OF provider ON requests
		WHEN NEW.provider = 'blocked.example'
		BEGIN SELECT RAISE(ABORT, 'rename blocked'); END`); err != nil {
		t.Fatal(err)
	}
	aliases := map[string]string{
		"a-old.example": "first.example",
		"b-old.example": "blocked.example",
	}
	if n, err := s.RenameProviders(t.Context(), aliases); err == nil {
		t.Fatalf("rename with rejecting trigger = %d, nil; want error", n)
	}
	assertProviders := func(want map[string]string) {
		t.Helper()
		recs, err := s.LoadRecent(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]string, len(recs))
		for _, r := range recs {
			got[r.ID] = r.Provider
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("providers after rename = %v, want %v", got, want)
		}
	}
	// The first alias matched rows inside the transaction, but the second
	// trigger abort rolled every rewrite back.
	assertProviders(map[string]string{"a-1": "a-old.example", "b-1": "b-old.example"})
	if _, err := s.db.Exec("DROP TRIGGER reject_before_rename"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RenameProviders(t.Context(), aliases)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("successful rename = %d rows, want 2", n)
	}
	assertProviders(map[string]string{"a-1": "first.example", "b-1": "blocked.example"})
}
