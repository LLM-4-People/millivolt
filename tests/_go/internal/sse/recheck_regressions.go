package sse

import (
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestResponsesFailedCapturesNestedProviderError pins PRO-2's analyzer half:
// response.failed carries the provider error under response.error, and the
// first real error becomes the record's error instead of an empty completion.
func TestResponsesFailedCapturesNestedProviderError(t *testing.T) {
	var a Analyzer
	a.SetWire(ir.Chat)
	for _, line := range []string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"},"sequence_number":0}`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"upstream exploded"}},"sequence_number":1}`,
	} {
		a.Feed([]byte(line), time.Now())
	}
	if !a.HasInBandError() {
		t.Fatal("nested response.error was not recognized as an in-band provider error")
	}
	var rec metrics.Record
	a.Fill(&rec)
	if rec.ErrorType != "provider_error" || rec.ErrorCode != "server_error" || rec.ErrorMsg != "upstream exploded" {
		t.Fatalf("record error = type=%q code=%q msg=%q", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
}

// TestResponsesCompletedStaysHealthy is the PRO-2 control.
func TestResponsesCompletedStaysHealthy(t *testing.T) {
	var a Analyzer
	a.SetWire(ir.Chat)
	for _, line := range []string{
		`data: {"type":"response.created","response":{"id":"resp_2"},"sequence_number":0}`,
		`data: {"type":"response.output_text.delta","delta":"hello","sequence_number":1}`,
		`data: {"type":"response.completed","response":{"id":"resp_2","status":"completed"},"sequence_number":2}`,
	} {
		a.Feed([]byte(line), time.Now())
	}
	if a.HasInBandError() || a.OutcomeCode() != "" {
		t.Fatalf("healthy Responses control: error=%v outcome=%q", a.HasInBandError(), a.OutcomeCode())
	}
}

// TestResponsesTruncationIsNeverRescued pins PRO-3: once a Responses event was
// relayed, its sequence numbering is committed wire state, so clean EOF is not
// a retryable truncation. A chat stream keeps the truncation rescue.
func TestResponsesTruncationIsNeverRescued(t *testing.T) {
	var responses Analyzer
	responses.SetWire(ir.Responses)
	responses.Feed([]byte(`data: {"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`), time.Now())
	if !responses.responsesStream {
		t.Fatal("response.created did not mark a Responses feed")
	}
	if responses.RetryableTruncation() {
		t.Fatal("a committed Responses feed was classified retryable")
	}
	if responses.RetryableReasoningTruncation() {
		t.Fatal("a committed Responses feed was classified reasoning-retryable")
	}
	if !strings.Contains(responses.OutcomeCode(), "truncat") && responses.OutcomeCode() != metrics.CodeTruncated {
		t.Fatalf("Responses truncation outcome = %q", responses.OutcomeCode())
	}

	var chat Analyzer
	chat.SetWire(ir.Chat)
	chat.Feed([]byte(`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`), time.Now())
	if !chat.RetryableTruncation() {
		t.Fatal("chat truncation stopped being retryable")
	}
}
