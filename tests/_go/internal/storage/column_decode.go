package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestCorruptAttemptsColumnAbsorbedPerRow pins the boot-scan contract of the
// shared attempts-column decoder: a corrupted JSON column on ONE row must be
// absorbed for that row (logged by the reader, attempts read back nil), never
// fail Open or the whole totals scan, and never reclassify other rows. The
// corrupted row is a final-429 whose absorbed 502 attempt would otherwise
// make it an error (metrics.IsError); with the column unreadable it loses the
// attempt and classifies as flow control, while the control row - the same
// absorbed-502 shape with a VALID column - still counts as an error.
func TestCorruptAttemptsColumnAbsorbedPerRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt-attempts.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed := func(id string, status int, attempts []metrics.RetryAttempt) {
		t.Helper()
		r := &metrics.Record{
			ID: id, Provider: "alpha.example", Model: "m", KeyHash: "k",
			StatusCode: status, Start: now, Attempts: attempts,
		}
		metrics.FinalizeRecord(r)
		s.Record(r)
	}
	// Seed order matters: the middle row is the corruption target.
	seed("err-500", 500, nil)
	seed("rate-429-att", 429, []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: now}})
	seed("control-200-att", 200, []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: now}})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Corrupt the middle row's attempts column through a second handle -
	// the shape an external writer or partial disk corruption leaves.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`UPDATE requests SET attempts = '{' WHERE id = ?`, "rate-429-att"); err != nil {
		t.Fatal(err)
	}
	other.Close()

	// Reopen: the boot scan must succeed, absorb the one bad row, and keep
	// every other row's classification.
	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open with a corrupted attempts column: %v", err)
	}
	defer s2.Close()
	tot := s2.Totals()
	if tot.Err() != nil {
		t.Fatalf("totals carry an arithmetic error: %v", tot.Err())
	}
	if tot.Requests != 3 {
		t.Fatalf("totals requests = %d, want 3 (per-row absorption, not a failed scan)", tot.Requests)
	}
	// err-500 (final error) + control-200-att (valid absorbed 502); the
	// corrupted 429 row lost its absorbed attempt and is flow control.
	if tot.Errors != 2 {
		t.Fatalf("totals errors = %d, want 2", tot.Errors)
	}

	// The corrupted row must still load through the record path: nil
	// attempts, never a scan failure.
	recs, err := s2.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatalf("LoadRecent with a corrupted attempts column: %v", err)
	}
	byID := map[string]*metrics.Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	if r := byID["rate-429-att"]; r == nil || r.Attempts != nil {
		t.Errorf("corrupted row Attempts = %v, want nil (absorbed per row)", r)
	}
	if r := byID["control-200-att"]; r == nil || len(r.Attempts) != 1 || r.Attempts[0].StatusCode != 502 {
		t.Errorf("control row Attempts = %v, want the untouched 502 attempt", r)
	}
}

// TestDecodeJSONColumnContract pins the shared column decoder's empty and
// error spellings directly: empty and "null" are the canonical no-value
// spellings (nil, nil), and any other unmarshal failure is returned to the
// caller - never swallowed, never fatal to the scan.
func TestDecodeJSONColumnContract(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("null")} {
		if atts, err := DecodeAttemptsColumn(raw); atts != nil || err != nil {
			t.Errorf("DecodeAttemptsColumn(%q) = %v, %v; want nil, nil", raw, atts, err)
		}
		if names, err := DecodeToolNamesColumn(raw); names != nil || err != nil {
			t.Errorf("DecodeToolNamesColumn(%q) = %v, %v; want nil, nil", raw, names, err)
		}
	}
	for _, raw := range [][]byte{[]byte("{"), []byte(`{"not":"an array"}`), []byte(`[{"bad":`)} {
		if atts, err := DecodeAttemptsColumn(raw); err == nil {
			t.Errorf("DecodeAttemptsColumn(%q) = %v, nil; want a decode error", raw, atts)
		}
		if names, err := DecodeToolNamesColumn(raw); err == nil {
			t.Errorf("DecodeToolNamesColumn(%q) = %v, nil; want a decode error", raw, names)
		}
	}
	atts, err := DecodeAttemptsColumn([]byte(`[{"status_code":503}]`))
	if err != nil || len(atts) != 1 || atts[0].StatusCode != 503 {
		t.Fatalf("DecodeAttemptsColumn(valid) = %v, %v; want one 503 attempt", atts, err)
	}
	names, err := DecodeToolNamesColumn([]byte(`["read","bash"]`))
	if err != nil || len(names) != 2 || names[0] != "read" || names[1] != "bash" {
		t.Fatalf("DecodeToolNamesColumn(valid) = %v, %v; want [read bash]", names, err)
	}
}

// TestTotalsDegradedFlag pins the degraded-mode contract of a failed boot
// scan: the flag is set when the initial totals scan cannot run, the
// aggregate then counts only rows committed since (a truthful subset, never
// a silent since-inception claim), and a healthy boot clears the flag. The
// deterministic failure injection is the query timeout: an already-expired
// deadline fails the scan's SELECT without touching the data.
func TestTotalsDegradedFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "degraded.db")
	degradedOpts := testOpts
	degradedOpts.QueryTimeout = time.Nanosecond
	s, err := Open(path, degradedOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.TotalsDegraded() {
		t.Fatal("failed boot scan: TotalsDegraded = false, want true")
	}
	// A committed row after the failed scan counts (counting from zero).
	r := &metrics.Record{ID: "post-boot", Provider: "p", Model: "m", KeyHash: "k",
		StatusCode: 200, Start: time.Now()}
	metrics.FinalizeRecord(r)
	s.Record(r)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := s.Totals().Requests; got != 1 {
		t.Fatalf("degraded totals requests = %d, want 1 (counting from zero)", got)
	}
	if !s.TotalsDegraded() {
		t.Fatal("degraded flag cleared by post-boot accounting, want it to persist")
	}

	// A healthy boot clears the flag and sees every durable row.
	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.TotalsDegraded() {
		t.Fatal("healthy boot: TotalsDegraded = true, want false")
	}
	if got := s2.Totals().Requests; got != 1 {
		t.Fatalf("healthy totals requests = %d, want 1", got)
	}
}
