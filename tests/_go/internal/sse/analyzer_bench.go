package sse

import (
	"testing"
	"time"
)

// BenchmarkAnalyzerFeed measures the per-frame cost of the analyzer's key
// classification for the ordinary wire shapes: a content delta, a reasoning
// delta, a tool-call delta, a usage-only chunk, and a negative (role-only)
// chunk. Each analyzer is warmed once so one-time identity/usage retention is
// not charged to the steady state; the tool-call case still pays its bounded
// JSON decode, while the content/reasoning/negative paths must stay
// allocation-free.
func BenchmarkAnalyzerFeed(b *testing.B) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name string
		line []byte
	}{
		{
			"content",
			[]byte(`data: {"id":"req-a","model":"model-a","choices":[{"index":0,"delta":{"content":"hello world"},"finish_reason":null}]}`),
		},
		{
			"reasoning",
			[]byte(`data: {"id":"req-a","model":"model-a","choices":[{"index":0,"delta":{"reasoning_content":"considering the request"},"finish_reason":null}]}`),
		},
		{
			"tool_call",
			[]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]}}]}`),
		},
		{
			"usage",
			[]byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
		},
		{
			"negative",
			[]byte(`data: {"id":"req-a","model":"model-a","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`),
		},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var a Analyzer
			a.Feed(tc.line, now)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				a.Feed(tc.line, now)
			}
		})
	}
}
