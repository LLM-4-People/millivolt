package sse

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestToolCallTTFT(t *testing.T) {
	var a Analyzer
	start := time.Now()
	firstChunk := start.Add(500 * time.Millisecond) // request → first token gap

	a.Feed([]byte(`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"search_web","arguments":""}}]}}]}`), firstChunk)
	a.Feed([]byte(`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]}}]}`), firstChunk.Add(50*time.Millisecond))
	a.Feed([]byte(`data: {"id":"1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`), firstChunk.Add(100*time.Millisecond))
	a.Feed([]byte(`data: [DONE]`), firstChunk.Add(150*time.Millisecond))

	rec := &metrics.Record{Start: start}
	a.Fill(rec)

	if rec.TTFT() != 500*time.Millisecond {
		t.Errorf("TTFT = %v, want 500ms", rec.TTFT())
	}
	if rec.ToolCalls != 1 {
		t.Errorf("ToolCalls = %d, want 1", rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "search_web" {
		t.Errorf("ToolNames = %v, want [search_web]", rec.ToolNames)
	}
	if rec.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", rec.FinishReason)
	}
}
