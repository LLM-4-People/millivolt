package sse

// F2 regression: the first nested Anthropic error owns the record. A later
// nested error must not overwrite its type, code or message.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicNestedErrorFirstWins(t *testing.T) {
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	var a Analyzer
	a.SetWire(ir.Anthropic)
	a.Feed([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"bad","message":"first"}}`), now)
	a.Feed([]byte(`data: {"type":"error","error":{"type":"api_error","code":"second_code","message":"second"}}`), now)

	if a.errType != "invalid_request_error" || a.errCode != "bad" || a.errMsg != "first" {
		t.Fatalf("analyzer error = %q/%q/%q, want the first error", a.errType, a.errCode, a.errMsg)
	}
	if !a.terminatorSeen {
		t.Fatal("a later error must still leave the stream terminal")
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ErrorType != "invalid_request_error" || rec.ErrorCode != "bad" || rec.ErrorMsg != "first" {
		t.Fatalf("record error = %q/%q/%q, want the first error", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
}

func TestAnthropicStandaloneErrorStillOwnsRecord(t *testing.T) {
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	var a Analyzer
	a.SetWire(ir.Anthropic)
	a.Feed([]byte(`data: {"type":"error","code":"standalone_code","message":"standalone"}`), now)
	if a.errType == "" || a.errMsg != "standalone" {
		t.Fatalf("standalone error = %q/%q, want a recorded failure", a.errType, a.errMsg)
	}
	if !a.terminatorSeen {
		t.Fatal("standalone error must leave the stream terminal")
	}
}
