package sse

// Round 13 F1 regression: a pretty-printed Responses terminal is reassembled at
// its SSE boundary, so its nested usage object must be captured exactly like a
// compact terminal. The per-line scan only sees the incomplete "usage": { line.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const prettyTerminalUsage = `{
  "type": "response.completed",
  "response": {
    "id": "r1",
    "status": "completed",
    "usage": {
      "input_tokens": 3,
      "output_tokens": 5,
      "total_tokens": 8,
      "input_tokens_details": {"cached_tokens": 1},
      "output_tokens_details": {"reasoning_tokens": 2}
    }
  }
}`

func analyzePrettyTerminal(t *testing.T, terminal string) (*Analyzer, *metrics.Record) {
	t.Helper()
	lines := []string{`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`, ""}
	lines = append(lines, prettyDataLines(terminal)...)
	return analyzeLines(lines)
}

func TestResponsesPrettyTerminalUsage(t *testing.T) {
	a, rec := analyzePrettyTerminal(t, prettyTerminalUsage)
	want := metrics.Usage{
		InputTokens: 3, OutputTokens: 5, TotalTokens: 8,
		CacheReadTokens: 1, ReasoningTokens: 2,
	}
	if rec.Usage != want {
		t.Fatalf("usage = %+v, want %+v", rec.Usage, want)
	}
	if !a.UsageReported() {
		t.Fatal("pretty terminal usage was not reported")
	}
	if rec.UsageInvalid {
		t.Fatalf("pretty terminal usage invalidated: %+v", rec.Usage)
	}
	if rec.Usage.OutputTokens == int64(rec.Chunks) {
		t.Fatalf("output tokens = %d, want the provider value not the chunk fallback (%d)", rec.Usage.OutputTokens, rec.Chunks)
	}

	// The compact sibling must stay identical.
	compact := `data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}}`
	ca, crec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		compact,
	})
	if crec.Usage != want || !ca.UsageReported() || crec.UsageInvalid {
		t.Fatalf("compact control = usage %+v reported %v invalid %v", crec.Usage, ca.UsageReported(), crec.UsageInvalid)
	}

	// A terminal without usage reports none and does not fabricate an invalid
	// result.
	na, nrec := analyzePrettyTerminal(t, `{
  "type": "response.completed",
  "response": {"id": "r1", "status": "completed"}
}`)
	if na.UsageReported() || nrec.UsageInvalid {
		t.Fatalf("usage-less terminal reported=%v invalid=%v", na.UsageReported(), nrec.UsageInvalid)
	}
	if nrec.Usage != (metrics.Usage{}) {
		t.Fatalf("usage-less terminal usage = %+v, want zero", nrec.Usage)
	}
}
