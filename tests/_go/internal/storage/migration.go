package storage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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
	// Seed history BEFORE the migration Open, the exact production shape an
	// ALTER TABLE ADD COLUMN ... DEFAULT 0 populated: the row below must
	// read every newer column as its zero default after Open migrates it.
	insertLegacyRow(t, db0, "legacy-zero", 200, 1_700_000_000_000)
	db0.Close()

	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open after old schema: %v", err)
	}
	defer s.Close()

	// The pre-migration row reads the nine columns the migrations added as
	// 0: chunks, first_reasoning_at, request_bytes, response_bytes,
	// upstream_gzip, upstream_connect_ms, upstream_tls_ms,
	// upstream_ttfb_ms and upstream_conn_reused (upstream_gzip and
	// upstream_conn_reused are booleans stored as integers, so 0 is the
	// false default the same way).
	legacy, err := s.Query(t.Context(), "SELECT chunks, first_reasoning_at, request_bytes, response_bytes, upstream_gzip, upstream_connect_ms, upstream_tls_ms, upstream_ttfb_ms, upstream_conn_reused FROM requests WHERE id='legacy-zero'")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 {
		t.Fatalf("legacy rows = %d, want 1", len(legacy))
	}
	for _, column := range []string{"chunks", "first_reasoning_at", "request_bytes", "response_bytes",
		"upstream_gzip", "upstream_connect_ms", "upstream_tls_ms", "upstream_ttfb_ms", "upstream_conn_reused"} {
		if legacy[0][column] != int64(0) {
			t.Errorf("legacy row %s = %v, want 0 (the migration's column default)", column, legacy[0][column])
		}
	}

	// UpstreamGzip and UpstreamConnReused deliberately DIFFER (true/false):
	// they are the only two boolean columns, so a fixture that stores the
	// same value in both cannot notice a by-name write/read swap of the
	// pair; distinct values make the swap fail loudly below.
	rec := &metrics.Record{
		ID: "m1", Provider: "p", Model: "m", KeyHash: "k", UserAgent: "ua",
		StatusCode: 200, Start: time.Now(), End: time.Now(),
		ToolCalls: 1, ErrorCode: "invalid_api_key", RateLimitRemaining: 42, Cost: 0.01,
		Chunks: 7, FirstReasoningAt: 1_700_000_000_000,
		RequestBytes: 1234, ResponseBytes: 5678, UpstreamGzip: true,
		UpstreamConnectMs: 3, UpstreamTLSMs: 0, UpstreamTTFBMs: 44, UpstreamConnReused: false,
	}
	metrics.FinalizeRecord(rec)
	s.Record(rec)
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err := s2.Query(t.Context(), "SELECT error_code, rate_limit_remaining, cost, chunks, first_reasoning_at, request_bytes, response_bytes, upstream_gzip, upstream_connect_ms, upstream_tls_ms, upstream_ttfb_ms, upstream_conn_reused FROM requests WHERE id='m1'")
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
	if rows[0]["chunks"] != int64(7) {
		t.Errorf("chunks = %v, want 7 (migrated column round-trips)", rows[0]["chunks"])
	}
	if rows[0]["first_reasoning_at"] != int64(1_700_000_000_000) {
		t.Errorf("first_reasoning_at = %v, want 1700000000000 (migrated column round-trips)", rows[0]["first_reasoning_at"])
	}
	if rows[0]["request_bytes"] != int64(1234) {
		t.Errorf("request_bytes = %v, want 1234 (migrated column round-trips)", rows[0]["request_bytes"])
	}
	if rows[0]["response_bytes"] != int64(5678) {
		t.Errorf("response_bytes = %v, want 5678 (migrated column round-trips)", rows[0]["response_bytes"])
	}
	if rows[0]["upstream_gzip"] != int64(1) {
		t.Errorf("upstream_gzip = %v, want 1 (migrated column round-trips)", rows[0]["upstream_gzip"])
	}
	if rows[0]["upstream_connect_ms"] != int64(3) {
		t.Errorf("upstream_connect_ms = %v, want 3 (migrated column round-trips)", rows[0]["upstream_connect_ms"])
	}
	if rows[0]["upstream_tls_ms"] != int64(0) {
		t.Errorf("upstream_tls_ms = %v, want 0 (plaintext upstream: the hook never fired)", rows[0]["upstream_tls_ms"])
	}
	if rows[0]["upstream_ttfb_ms"] != int64(44) {
		t.Errorf("upstream_ttfb_ms = %v, want 44 (migrated column round-trips)", rows[0]["upstream_ttfb_ms"])
	}
	if rows[0]["upstream_conn_reused"] != int64(0) {
		t.Errorf("upstream_conn_reused = %v, want 0 (the false half of the distinct bool pair, coherent with the fresh dial: a by-name swap with upstream_gzip now fails here)", rows[0]["upstream_conn_reused"])
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
	result, err := s.Clear(t.Context(), &PurgeFilter{HasError: true}, nil)
	if err != nil {
		t.Fatalf("errors-only purge on pre-attempts rows: %v", err)
	}
	if result.Deleted != 1 {
		t.Errorf("errors-only purge deleted %d rows, want 1", result.Deleted)
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
