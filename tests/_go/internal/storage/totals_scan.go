package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestReadTotalsValueLockstep is the value-level twin of the web aggregate's
// TestRowColumnsScanContribLockstep: readTotals' positional SELECT/Scan pair
// feeds every Totals field, but the boot/purge assertions only ever counted
// Requests and Errors, so a future positional edit here would corrupt every
// other field silently. Varied rows are seeded through the real writer, then
// every field of the scan's result is checked against hand-computed expected
// values - including the 429-without-error row (flow control is not an
// error), the absorbed-attempt decode, the absent-TTFT rule (ttft_ms 0 never
// dilutes the mean) and the cost-bearing denominators (an unpriced row adds
// tokens but not blended-rate weight).
func TestReadTotalsValueLockstep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "totals.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.UnixMilli(1_700_000_000_000)
	seed := []*metrics.Record{
		{
			// A clean priced 200.
			ID: "clean", Provider: "p", Model: "m", KeyHash: "k",
			StatusCode: 200, Start: at,
			TTFTMs: 120, OverallTPS: 50, DecodeTPS: 9, Cost: 0.25,
			Usage: metrics.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5},
		},
		{
			// Flow control: a final 429 (with its classified body) whose
			// only absorbed attempt is another 429 - never an error row -
			// and no TTFT sample (0 means absent).
			ID: "flow-control", Provider: "p", Model: "m", KeyHash: "k",
			StatusCode: 429, ErrorType: "rate_limit_error", Start: at.Add(time.Second),
			DecodeTPS: 4,
			Attempts: []metrics.RetryAttempt{{
				StatusCode: 429, ErrorType: "rate_limit_error", RetryAfterMs: 250, At: at.Add(500 * time.Millisecond),
			}},
		},
		{
			// A genuine failure: final 500 with a structured error and a
			// mixed attempt log (429 flow control + an absorbed 503).
			ID: "failed", Provider: "p", Model: "m", KeyHash: "k",
			StatusCode: 500, ErrorType: "provider_error", ErrorMsg: "boom", Start: at.Add(2 * time.Second),
			TTFTMs: 300, OverallTPS: 25, Cost: 1.5,
			Usage: metrics.Usage{InputTokens: 200, OutputTokens: 50, CacheReadTokens: 15, ReasoningTokens: 30},
			Attempts: []metrics.RetryAttempt{
				{StatusCode: 429, ErrorType: "rate_limit_error", RetryAfterMs: 250, At: at.Add(time.Second)},
				{StatusCode: 503, ErrorType: "server_error", ErrorMsg: "overloaded", At: at.Add(1500 * time.Millisecond)},
			},
			Retries:     2,
			RateLimited: true,
		},
		{
			// An unpriced clean 200: tokens count, the blended-rate
			// denominators do not, and no speed sample exists.
			ID: "unpriced", Provider: "p", Model: "m", KeyHash: "k",
			StatusCode: 200, Start: at.Add(3 * time.Second),
			Usage: metrics.Usage{InputTokens: 50, OutputTokens: 10, ReasoningTokens: 10},
		},
	}
	for _, r := range seed {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	got, err := readTotals(t.Context(), s.rdb)
	if err != nil {
		t.Fatal(err)
	}
	if got.Err() != nil {
		t.Fatalf("totals arithmetic failed: %v", got.Err())
	}
	if got.Requests != 4 {
		t.Fatalf("Requests = %d, want 4 (every seeded row scanned)", got.Requests)
	}
	if got.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the 500 only; the 429-without-error row and the clean 200s never count)", got.Errors)
	}
	if got.Cost != 1.75 {
		t.Errorf("Cost = %v, want 1.75 (0.25 + 1.5; the unpriced rows add 0)", got.Cost)
	}
	if got.InputTok != 350 {
		t.Errorf("InputTok = %d, want 350 (100 + 0 + 200 + 50; the 429 row reported no usage)", got.InputTok)
	}
	if got.OutputTok != 80 {
		t.Errorf("OutputTok = %d, want 80 (20 + 0 + 50 + 10)", got.OutputTok)
	}
	if got.CacheRead != 20 {
		t.Errorf("CacheRead = %d, want 20 (5 + 0 + 15 + 0)", got.CacheRead)
	}
	if got.Reasoning != 40 {
		t.Errorf("Reasoning = %d, want 40 (0 + 0 + 30 + 10)", got.Reasoning)
	}
	if got.TTFTSum != 420 || got.TTFTN != 2 {
		t.Errorf("TTFTSum/N = %d/%d, want 420/2 (120 + 300; the two absent samples never dilute)", got.TTFTSum, got.TTFTN)
	}
	if got.TPSSum != 79 || got.TPSN != 3 {
		t.Errorf("TPSSum/N = %v/%d, want 79/3 (overall 50 and 25, decode fallback 4, no sample on the unpriced row)", got.TPSSum, got.TPSN)
	}
	if got.CostReqs != 2 || got.CostInOut != 370 {
		t.Errorf("CostReqs/CostInOut = %d/%v, want 2/370 (the two priced rows' in+out only)", got.CostReqs, got.CostInOut)
	}
}
