package sse

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// extractCostUngated is the analyzer's pre-gate cost path, kept here as the
// differential oracle: decode the whole document generically, then ask the
// metrics owner whether a cost key resolves. The analyzer now routes through
// metrics.ExtractCost, whose key gate skips that decode, and must produce
// exactly this result for every document shape.
func extractCostUngated(raw []byte, keys []string) (float64, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return 0, false
	}
	return metrics.ExtractCostFromValue(v, keys)
}

// TestAnalyzerCostMatchesUngatedDecode is the WS1 differential: a cost-bearing
// usage chunk resolves to the same Record.Cost through the gated path as
// through the unconditional decode it replaced, including the escaped-key
// fallback, a string-valued cost, a provider override path, a top-level path
// beside usage, and the rejected shapes (negative, null, invalid JSON).
func TestAnalyzerCostMatchesUngatedDecode(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		costKeys []string
		want     float64
	}{
		{"plain usage.cost", `{"usage":{"prompt_tokens":3,"cost":0.25}}`, nil, 0.25},
		{"escaped key", `{"usage":{"\u0063ost":0.5}}`, nil, 0.5},
		{"string-valued cost", `{"usage":{"cost":"0.0042"}}`, nil, 0.0042},
		{"override path", `{"usage":{"custom_charge":0.009}}`, []string{"usage.custom_charge"}, 0.009},
		{"override beside usage", `{"usage":{"prompt_tokens":10},"billing":{"amount":0.75}}`, []string{"billing.amount"}, 0.75},
		{"nested usd object", `{"usage":{"cost":{"usd":0.001038,"hypercredits":0.02076}}}`, nil, 0.001038},
		{"integer ticks scale", `{"usage":{"cost_in_usd_ticks":1421200}}`, nil, 0.00014212},
		{"top-level cost", `{"cost":0.007,"usage":{"prompt_tokens":1}}`, nil, 0.007},
		{"negative rejected", `{"usage":{"cost":-1}}`, nil, 0},
		{"null rejected", `{"usage":{"cost":null}}`, nil, 0},
		{"no cost key", `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`, nil, 0},
		{"escaped non-cost value still gated correctly", `{"usage":{"prompt_tokens":10},"note":"a\u0062c"}`, nil, 0},
		{"invalid json", `{"usage":`, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			var a Analyzer
			a.CostKeys = c.costKeys
			a.Feed([]byte("data: "+c.payload), now)
			var rec metrics.Record
			a.Fill(&rec)

			want, _ := extractCostUngated([]byte(c.payload), c.costKeys)
			if want != c.want {
				t.Fatalf("oracle cost = %v, want %v (fixture expectation drifted)", want, c.want)
			}
			if rec.Cost != c.want {
				t.Fatalf("Record.Cost = %v, want %v (gated path differs from the ungated decode)", rec.Cost, c.want)
			}
			if rec.Cost != want {
				t.Fatalf("Record.Cost = %v, ungated decode = %v: differential mismatch", rec.Cost, want)
			}
		})
	}
}

// TestAnalyzerUsageFrameWithoutCostAllocatesNothing pins the gate at the
// analyzer boundary: an ordinary usage chunk (the one every stream ends with)
// carries no cost key, so capturing it must not decode the document. The
// warmup absorbs the analyzer's one-time retention capacity growth.
func TestAnalyzerUsageFrameWithoutCostAllocatesNothing(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	line := []byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)

	var a Analyzer
	a.Feed(line, now)
	if allocs := testing.AllocsPerRun(200, func() { a.Feed(line, now) }); allocs != 0 {
		t.Fatalf("Feed(usage without cost) allocates %v per frame, want 0 (cost decode gate bypassed)", allocs)
	}

	// A cost-bearing sibling still resolves: the gate must not trade the win
	// for a lost charge.
	costLine := []byte(`data: {"choices":[],"usage":{"prompt_tokens":10},"billing":{"amount":0.125}}`)
	var b Analyzer
	b.CostKeys = []string{"billing.amount"}
	b.Feed(costLine, now)
	var rec metrics.Record
	b.Fill(&rec)
	if rec.Cost != 0.125 {
		t.Fatalf("Record.Cost = %v, want 0.125", rec.Cost)
	}
}
