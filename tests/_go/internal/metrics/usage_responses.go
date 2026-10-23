package metrics

// Batch 13 regression: the shared usage parser owns the Responses detail
// paths, not only the Chat spellings.

import (
	"encoding/json"
	"testing"
)

func TestResponsesUsageDetailsParsed(t *testing.T) {
	var root any
	const body = `{"input_tokens":100,"output_tokens":50,"total_tokens":150,` +
		`"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30},` +
		`"output_tokens_details":{"reasoning_tokens":10}}`
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		t.Fatal(err)
	}
	got := ParseUsage(root, nil)
	want := Usage{
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
		CacheReadTokens: 20, CacheWrite: 30, ReasoningTokens: 10,
	}
	if got != want {
		t.Fatalf("ParseUsage = %+v, want %+v", got, want)
	}
}
