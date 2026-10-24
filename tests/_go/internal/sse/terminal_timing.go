package sse

// Round 2 TC6-4/TC6-8 regressions: streamed answer timing is never replaced by
// a later terminal unit, and terminal-only reasoning records the same chunk,
// token and timing evidence as streamed reasoning.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func fillRecord(t *testing.T, a *Analyzer, first time.Time) metrics.Record {
	t.Helper()
	var rec metrics.Record
	rec.FinalAttemptAt = first.Add(-time.Millisecond)
	a.Fill(&rec)
	metrics.FinalizeRecord(&rec)
	return rec
}

func TestTerminalAnswerKeepsStreamedFirstAnswerTime(t *testing.T) {
	first := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	terminalAt := first.Add(time.Second)
	var a Analyzer
	a.Feed([]byte(`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"a"}`), first)
	a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}]}}`), terminalAt)
	a.Feed([]byte(""), terminalAt)
	rec := fillRecord(t, &a, first)
	if !rec.FirstAnswerAt.Equal(first) {
		t.Fatalf("FirstAnswerAt = %v, want the streamed time %v", rec.FirstAnswerAt, first)
	}
	if !a.lastTokenAt.Equal(terminalAt) || rec.Chunks != 2 {
		t.Fatalf("last token/chunks = %v/%d, want %v/2", a.lastTokenAt, rec.Chunks, terminalAt)
	}
	if rec.TTFTMs != 1 {
		t.Fatalf("TTFT = %dms, want 1ms from the streamed token", rec.TTFTMs)
	}
}

func TestTerminalOnlyReasoningMatchesStreamedAccounting(t *testing.T) {
	first := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	terminalAt := first.Add(time.Second)
	const reasoningOutput = `[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}]`

	var terminal Analyzer
	terminal.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":`+reasoningOutput+`}}`), terminalAt)
	terminal.Feed([]byte(""), terminalAt)
	terminalRec := fillRecord(t, &terminal, terminalAt)

	var streamed Analyzer
	streamed.Feed([]byte(`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"think"}`), first)
	streamed.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":`+reasoningOutput+`}}`), terminalAt)
	streamed.Feed([]byte(""), terminalAt)
	streamedRec := fillRecord(t, &streamed, first)

	if terminalRec.Chunks != 1 || streamedRec.Chunks != 1 {
		t.Fatalf("chunks = %d/%d, want 1/1", terminalRec.Chunks, streamedRec.Chunks)
	}
	if terminalRec.GenTokens != 1 || streamedRec.GenTokens != 1 {
		t.Fatalf("gen tokens = %d/%d, want 1/1", terminalRec.GenTokens, streamedRec.GenTokens)
	}
	if terminalRec.TTFTMs != 1 || streamedRec.TTFTMs != 1 {
		t.Fatalf("TTFT = %d/%d ms, want 1/1", terminalRec.TTFTMs, streamedRec.TTFTMs)
	}
	if terminalRec.FirstReasoningAt != terminalAt.UnixMilli() {
		t.Fatalf("terminal FirstReasoningAt = %d, want %d", terminalRec.FirstReasoningAt, terminalAt.UnixMilli())
	}
	if streamedRec.FirstReasoningAt != first.UnixMilli() {
		t.Fatalf("streamed FirstReasoningAt = %d, want %d", streamedRec.FirstReasoningAt, first.UnixMilli())
	}
}

func TestTerminalReasoningPartsCountAsUnits(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	var a Analyzer
	a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"}],"content":[{"type":"reasoning_text","text":"three"}]}]}}`), now)
	a.Feed([]byte(""), now)
	rec := fillRecord(t, &a, now)
	if rec.Chunks != 3 || rec.GenTokens != 3 {
		t.Fatalf("chunks/gen = %d/%d, want 3/3", rec.Chunks, rec.GenTokens)
	}
	if rec.FirstReasoningAt != now.UnixMilli() {
		t.Fatalf("FirstReasoningAt = %d, want %d", rec.FirstReasoningAt, now.UnixMilli())
	}
}
