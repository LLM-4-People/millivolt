package proxy

import (
	"runtime"
	"testing"
)

// proxyContentAllocationMax is the hard byte ceiling for one bounded content
// decode. Materializing a million empty parts costs hundreds of MiB; the
// shared lexical bound keeps these readers within the body's own footprint.
const proxyContentAllocationMax = 16 << 20

// proxyContentParts builds one JSON content array of n empty objects.
func proxyContentParts(n int) []byte {
	body := make([]byte, 0, n*3+1)
	body = append(body, '[')
	for i := 0; i < n; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '{', '}')
	}
	body = append(body, ']')
	return body
}

// proxyContentAllocDelta measures bytes allocated by fn without timing it.
func proxyContentAllocDelta(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestCursorContentReadersBoundPartArrays covers the two cursor request
// readers that walk message content: extractToolResults and
// estimateInputTokens. Both route through metrics.FlattenContent, so an
// over-bound part array is skipped without expanding into a typed slice.
func TestCursorContentReadersBoundPartArrays(t *testing.T) {
	small := extractToolResults([]byte(`{"messages":[{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"ok"}]}]}`))
	if len(small) != 1 || small[0].content != "ok" {
		t.Fatalf("normal tool-result parts = %#v, want one ok result", small)
	}
	if got, want := estimateInputTokens([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"12345678"}]}]}`)), int64(cursorAgentContextBaseTokens+1+2); got != want {
		t.Fatalf("normal input estimate = %d, want %d", got, want)
	}

	heavy := proxyContentParts(1_000_000)
	toolBody := append([]byte(`{"messages":[{"role":"tool","tool_call_id":"c1","content":`), heavy...)
	toolBody = append(toolBody, []byte(`}]}`)...)

	var results []cursorToolResult
	alloc := proxyContentAllocDelta(func() { results = extractToolResults(toolBody) })
	if len(results) != 1 || results[0].toolCallID != "c1" || results[0].content != "" {
		t.Fatalf("over-bound tool results = %#v, want the tool result retained with empty text", results)
	}
	if alloc > proxyContentAllocationMax {
		t.Fatalf("extractToolResults allocated %d bytes for a %d-byte over-bound array, want <= %d",
			alloc, len(heavy), proxyContentAllocationMax)
	}
	t.Logf("extractToolResults over-bound: input=%d alloc=%d", len(toolBody), alloc)

	userBody := append([]byte(`{"messages":[{"role":"user","content":`), heavy...)
	userBody = append(userBody, []byte(`}]}`)...)

	var tokens int64
	alloc = proxyContentAllocDelta(func() { tokens = estimateInputTokens(userBody) })
	if want := int64(cursorAgentContextBaseTokens + 1); tokens != want {
		t.Fatalf("over-bound input estimate = %d, want %d (text skipped, message counted)", tokens, want)
	}
	if alloc > proxyContentAllocationMax {
		t.Fatalf("estimateInputTokens allocated %d bytes for a %d-byte over-bound array, want <= %d",
			alloc, len(heavy), proxyContentAllocationMax)
	}
	t.Logf("estimateInputTokens over-bound: input=%d alloc=%d", len(userBody), alloc)
}
