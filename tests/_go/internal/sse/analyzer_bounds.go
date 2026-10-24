package sse

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// toolNameFeedLines builds chat-completion chunks carrying distinct tool calls.
// Each line holds up to perChunk calls, so a large unique-name set parses in
// bounded chunks instead of one oversized SSE frame.
func toolNameFeedLines(unique, perChunk int) [][]byte {
	var lines [][]byte
	for start := 0; start < unique; start += perChunk {
		end := min(start+perChunk, unique)
		var b strings.Builder
		b.WriteString(`data: {"choices":[{"delta":{"tool_calls":[`)
		for i := start; i < end; i++ {
			if i > start {
				b.WriteByte(',')
			}
			// Indexes are per-response, so the synthetic stream must keep
			// them unique across chunks; reusing an index would describe a
			// continuation of the same call under the index-counting contract.
			fmt.Fprintf(&b, `{"index":%d,"id":"call-%d","type":"function","function":{"name":"tool-%06d","arguments":""}}`, i, i, i)
		}
		b.WriteString(`]}}]}`)
		lines = append(lines, []byte(b.String()))
	}
	return lines
}

// TestAnalyzerToolNameRetentionBounded pins the producer cap: a stream with
// 100k distinct tool names keeps only the first metrics.RetainedToolNamesMax
// names in arrival order, while every tool call is still counted. The retained
// map/set never grows past that cap, so a hostile stream cannot retain a
// names-sized allocation until Fill.
func TestAnalyzerToolNameRetentionBounded(t *testing.T) {
	const unique = 100000
	lines := toolNameFeedLines(unique, 1000)
	now := time.Now()
	var a Analyzer
	for _, line := range lines {
		a.Feed(line, now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ToolCalls != unique {
		t.Fatalf("ToolCalls = %d, want every one of %d calls", rec.ToolCalls, unique)
	}
	if len(rec.ToolNames) != metrics.RetainedToolNamesMax {
		t.Fatalf("ToolNames = %d, want the shared cap %d", len(rec.ToolNames), metrics.RetainedToolNamesMax)
	}
	for i, name := range rec.ToolNames {
		if want := fmt.Sprintf("tool-%06d", i); name != want {
			t.Fatalf("ToolNames[%d] = %q, want %q (first-seen order changed)", i, name, want)
		}
	}
}

// TestAnalyzerToolNamesDeduplicateAndCount pins the behavior the set replaces
// the linear scan for: a repeated name contributes one retained entry while
// every tool call still increments the count.
func TestAnalyzerToolNamesDeduplicateAndCount(t *testing.T) {
	now := time.Now()
	var a Analyzer
	for i := 0; i < 3; i++ {
		a.Feed([]byte(fmt.Sprintf(`data: {"choices":[{"delta":{"tool_calls":[{"index":%d,"id":"call-%d","function":{"name":"search"}}]}}]}`, i, i)), now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ToolCalls != 3 {
		t.Fatalf("ToolCalls = %d, want 3", rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "search" {
		t.Fatalf("ToolNames = %v, want [search]", rec.ToolNames)
	}
}

// BenchmarkAnalyzerToolNameDedupe demonstrates the set's linear scaling: the
// per-name work is one map probe, independent of how many names were retained
// before it.
func BenchmarkAnalyzerToolNameDedupe(b *testing.B) {
	for _, unique := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("%d", unique), func(b *testing.B) {
			lines := toolNameFeedLines(unique, 1000)
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var a Analyzer
				for _, line := range lines {
					a.Feed(line, now)
				}
				var rec metrics.Record
				a.Fill(&rec)
				if rec.ToolCalls != unique {
					b.Fatalf("ToolCalls = %d, want %d", rec.ToolCalls, unique)
				}
			}
		})
	}
}
