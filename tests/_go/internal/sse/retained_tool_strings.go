package sse

// Round 5 F2 regression: retained tool names and Responses call identities are
// count-bounded but must also be byte-bounded. Names are display labels cut by
// the shared 128-byte truncator; over-long ids use a digest key so exact dedupe
// survives without retaining the payload.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestRetainedToolStringsAreByteBounded(t *testing.T) {
	var a Analyzer
	for i := 0; i < metrics.RetainedToolNamesMax; i++ {
		name := fmt.Sprintf("tool-%02d-", i) + strings.Repeat("n", 65536)
		a.countResponsesToolCall(fmt.Sprintf("short-call-%d", i), "", name)
	}
	var rec metrics.Record
	a.Fill(&rec)
	if len(rec.ToolNames) != metrics.RetainedToolNamesMax {
		t.Fatalf("retained names = %d, want %d", len(rec.ToolNames), metrics.RetainedToolNamesMax)
	}
	seen := make(map[string]struct{}, len(rec.ToolNames))
	for i, name := range rec.ToolNames {
		if len(name) > 128 {
			t.Fatalf("retained name %d is %d bytes, want at most 128", i, len(name))
		}
		seen[name] = struct{}{}
	}
	if len(seen) != metrics.RetainedToolNamesMax {
		t.Fatalf("retained distinct names = %d, want %d", len(seen), metrics.RetainedToolNamesMax)
	}

	var ids Analyzer
	const count = 1024
	var duplicateID string
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("call-%04d-", i) + strings.Repeat("i", 400)
		if i == 0 {
			duplicateID = id
		}
		if !ids.countResponsesToolCall(id, "", "lookup") {
			t.Fatalf("distinct long id %d was treated as a duplicate", i)
		}
	}
	if ids.countResponsesToolCall(duplicateID, "", "lookup") {
		t.Fatal("duplicate long id was counted twice")
	}
	ids.Fill(&rec)
	if ids.toolCalls != count || rec.ToolCalls != count {
		t.Fatalf("tool calls = analyzer %d record %d, want %d", ids.toolCalls, rec.ToolCalls, count)
	}
	if len(ids.toolCallIDs) != count {
		t.Fatalf("retained id keys = %d, want %d", len(ids.toolCallIDs), count)
	}
	for key := range ids.toolCallIDs {
		if len(key) > 256 {
			t.Fatalf("retained id key is %d bytes, want at most 256", len(key))
		}
	}
}

func TestRetainToolNameDedupesAfterTruncation(t *testing.T) {
	prefix := strings.Repeat("x", 200)
	var a Analyzer
	a.retainToolName(prefix + "first")
	a.retainToolName(prefix + "second")
	if len(a.toolNames) != 1 {
		t.Fatalf("retained names = %v, want one after truncation", a.toolNames)
	}
	if len(a.toolNames[0]) > 128 {
		t.Fatalf("retained name is %d bytes, want at most 128", len(a.toolNames[0]))
	}
}

func TestRetainToolCallIDKeyPreservesShortIDs(t *testing.T) {
	const short = "call_abc123"
	if got := metrics.RetainToolCallIDKey(short); got != short {
		t.Fatalf("short id = %q, want unchanged %q", got, short)
	}
	longA := strings.Repeat("a", 300) + "1"
	longB := strings.Repeat("a", 300) + "2"
	keyA := metrics.RetainToolCallIDKey(longA)
	keyB := metrics.RetainToolCallIDKey(longB)
	if keyA == keyB {
		t.Fatalf("distinct long ids collided at %q", keyA)
	}
	if keyA != metrics.RetainToolCallIDKey(longA) {
		t.Fatal("long-id key is not stable")
	}
	if len(keyA) > 256 || !strings.HasPrefix(keyA, "sha256:") {
		t.Fatalf("long-id key = %q, want a bounded sha256 key", keyA)
	}
}
