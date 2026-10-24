package sse

// Round 11 NEW-6 regression: the terminal classifier for an unterminated final
// line must use the analyzer's own frame grammar and must never commit state.
// The relay calls it before deciding whether a truncation-class tail may be
// dropped.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnalyzerTailTerminates(t *testing.T) {
	var chat Analyzer
	for _, line := range []string{
		`data: [DONE]`,
		`data:[DONE]`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
	} {
		if !chat.TailTerminates([]byte(line)) {
			t.Fatalf("chat terminal tail not recognized: %q", line)
		}
	}
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":"TAIL"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{}}]}`,
		``,
		`event: message_stop`,
		// A standalone in-band error terminal and a foreign-wire terminal are
		// terminal for Feed but deliberately outside the delivery classifier:
		// an unterminated in-band error line is dropped rather than delivered.
		`data: {"type":"error","code":"x","message":"boom"}`,
		`data: {"type":"message_stop"}`,
	} {
		if chat.TailTerminates([]byte(line)) {
			t.Fatalf("non-delivery tail classified terminal: %q", line)
		}
	}
	// The classifier must not commit: no terminator, no token time.
	var rec metrics.Record
	chat.Fill(&rec)
	if rec.FinishReason != "" || !rec.FirstTokenAt.IsZero() || !rec.FirstAnswerAt.IsZero() {
		t.Fatalf("classifier committed state: %+v", rec)
	}
	if code := chat.OutcomeCode(); code != "" {
		t.Fatalf("classifier changed the outcome: %q", code)
	}

	var responses Analyzer
	responses.SetWire(ir.Responses)
	if !responses.TailTerminates([]byte(`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`)) {
		t.Fatal("Responses terminal tail not recognized")
	}
	if responses.TailTerminates([]byte(`data: [DONE]`)) {
		t.Fatal("Responses feed treated [DONE] as terminal")
	}
	if responses.TailTerminates([]byte(`data: {"type":"response.output_text.delta","delta":"x"}`)) {
		t.Fatal("Responses content tail classified terminal")
	}
}

// TestAnalyzerTailTerminatesRecognizesCompletionTerminals pins the narrowed
// contract: the classifier owns the completion terminals that decide whether a
// truncation-class tail is delivered, not every terminal Feed can record.
// Standalone error terminals and foreign-wire terminals are excluded and
// asserted false in TestAnalyzerTailTerminates.
func TestAnalyzerTailTerminatesRecognizesCompletionTerminals(t *testing.T) {
	now := time.Unix(100, 0)
	for _, line := range []string{
		`data: [DONE]`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[{"delta":{"content":"TAIL"}}]}`,
	} {
		var probe Analyzer
		var fed Analyzer
		terminates := probe.TailTerminates([]byte(line))
		fed.Feed([]byte(line), now)
		if fed.Terminated() != terminates {
			t.Fatalf("TailTerminates(%q) = %v, Feed terminated = %v", line, terminates, fed.Terminated())
		}
	}
}
