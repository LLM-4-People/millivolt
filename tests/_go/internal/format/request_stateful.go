package format

// Round 2 TC1-4b regressions: stateful and safety request fields are either
// rejected, carried between Chat and Responses, or reported as dropped.

import (
	"strings"
	"testing"
)

func TestResponsesRejectsContextManagement(t *testing.T) {
	body := `{"model":"m","input":"hi","context_management":[{"type":"compaction","compact_threshold":1000}]}`
	if _, err := DecodeResponsesRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "context_management") {
		t.Fatalf("context_management error = %v", err)
	}
	null := `{"model":"m","input":"hi","context_management":null}`
	if _, err := DecodeResponsesRequest([]byte(null)); err != nil {
		t.Fatalf("null context_management rejected: %v", err)
	}
}
