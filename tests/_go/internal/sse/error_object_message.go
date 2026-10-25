package sse

// Round 12 F-U regression: a provider error object whose message member is an
// object still classifies as a failure. The frame reached the client, so an
// empty record would report a healthy stream.

import "testing"

func TestObjectErrorMessageStillClassifiesStream(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"error":{"type":"server_error","message":{"detail":"x"}}}`,
	})
	if rec.ErrorType != "server_error" {
		t.Fatalf("error type = %q, want server_error (record %+v)", rec.ErrorType, rec)
	}
	if !rec.IsError() {
		t.Fatalf("object-message error classified as healthy: record %+v", rec)
	}
	if a.OutcomeCode() != "" {
		t.Fatalf("outcome = %q, want no degenerate substitution for an explicit error", a.OutcomeCode())
	}

	// A numeric type with a string message keeps the message and falls back to
	// the canonical provider classification.
	_, rec = analyzeLines([]string{
		`data: {"error":{"type":42,"message":"boom"}}`,
	})
	if rec.ErrorType != "provider_error" || rec.ErrorMsg != "boom" {
		t.Fatalf("numeric type record = %q/%q, want provider_error/boom", rec.ErrorType, rec.ErrorMsg)
	}
}
