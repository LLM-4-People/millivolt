package sse

// Round 2 TC6-3/TC6-7 regressions: a present usage object whose output count
// is zero must not erase the chunk-derived output and generation fallbacks.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestZeroUsageKeepsToolChunkExclusion(t *testing.T) {
	var a Analyzer
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, line := range []string{
		`data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]}}]}`,
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
	} {
		a.Feed([]byte(line), now)
	}
	rec := &metrics.Record{}
	a.Fill(rec)
	if rec.Chunks != 2 || rec.Usage.OutputTokens != 2 || rec.GenTokens != 1 {
		t.Fatalf("chunks/output/gen = %d/%d/%d, want 2/2/1 (tool argument chunk excluded): %+v",
			rec.Chunks, rec.Usage.OutputTokens, rec.GenTokens, rec)
	}
	metrics.FinalizeRecord(rec)
	if rec.GenTokens != 1 {
		t.Fatalf("finalized gen tokens = %d, want 1 (tool argument chunk excluded): %+v", rec.GenTokens, rec)
	}
}

func TestTruncatedUsageKeepsChunkFallback(t *testing.T) {
	var a Analyzer
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, line := range []string{
		`data: {"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":100,"output_tokens":0}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	} {
		a.Feed([]byte(line), now)
	}
	rec := &metrics.Record{}
	a.Fill(rec)
	if rec.Chunks != 1 || rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 1 || rec.GenTokens != 1 {
		t.Fatalf("chunks/input/output/gen = %d/%d/%d/%d, want 1/100/1/1: %+v",
			rec.Chunks, rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.GenTokens, rec)
	}
	metrics.FinalizeRecord(rec)
	if rec.GenTokens != 1 || rec.AnswerTokens != 1 {
		t.Fatalf("finalized answer/gen = %d/%d, want 1/1: %+v", rec.AnswerTokens, rec.GenTokens, rec)
	}
}
