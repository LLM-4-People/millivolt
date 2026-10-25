package sse

// Round 3 batch 2 regressions (F1): a completed Responses terminal carrying a
// recognized tool call or any other official output item is never replaced by
// the empty-completion failure.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesCustomToolCallCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_1","name":"custom"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if rec.ToolCalls != 1 {
		t.Fatalf("custom tool calls = %d, want 1", rec.ToolCalls)
	}
	if rec.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
	}
	if a.OutcomeCode() != "" {
		t.Fatalf("custom tool call outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesLocalShellCallCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"local_shell_call","call_id":"call_2","name":"shell"}]}}`,
	})
	if rec.ToolCalls != 1 {
		t.Fatalf("local shell calls = %d, want 1", rec.ToolCalls)
	}
	if a.OutcomeCode() != "" {
		t.Fatalf("local shell outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesEncryptedReasoningOnlyCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]}]}}`,
	})
	if a.OutcomeCode() == metrics.CodeEmptyCompletion {
		t.Fatalf("encrypted reasoning was classified as an empty completion (record %+v)", rec)
	}
}

func TestResponsesWebSearchCallCompletes(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"web_search_call","id":"ws_1","status":"completed"}]}}`,
	})
	if a.OutcomeCode() != "" {
		t.Fatalf("web search outcome = %q, want healthy", a.OutcomeCode())
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("web search finish = %q, want stop", rec.FinishReason)
	}
}

func TestResponsesUnknownOutputItemCompletes(t *testing.T) {
	a, _ := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"provider_specific_item","id":"x_1"}]}}`,
	})
	if a.OutcomeCode() != "" {
		t.Fatalf("unknown output item outcome = %q, want healthy", a.OutcomeCode())
	}
}

func TestResponsesEmptyMessageStaysEmptyCompletion(t *testing.T) {
	a, _ := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress","reasoning":{"effort":null,"summary":null}}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","reasoning":{"effort":null,"summary":null},"output":[]}}`,
	})
	if a.OutcomeCode() != metrics.CodeEmptyCompletion {
		t.Fatalf("empty message outcome = %q, want empty_completion", a.OutcomeCode())
	}
}

// TestResponsesReasoningEchoIsNotPresence is the round 12 F1 regression: the
// official response.created and response.completed envelopes carry
// "reasoning":{"effort":null,"summary":null}. That object is a request echo,
// not output, so it must not set TTFT, the reasoning timestamp, chunks or the
// no-usage token fallback, and an empty turn stays empty_completion.
func TestResponsesReasoningEchoIsNotPresence(t *testing.T) {
	a, rec := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress","reasoning":{"effort":null,"summary":null}}}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","reasoning":{"effort":null,"summary":null},"output":[]}}`,
	})
	if !rec.FirstTokenAt.IsZero() {
		t.Fatalf("first token at %v, want zero for an empty turn", rec.FirstTokenAt)
	}
	if rec.FirstReasoningAt != 0 {
		t.Fatalf("first reasoning at %d, want zero", rec.FirstReasoningAt)
	}
	if rec.Chunks != 0 || rec.GenTokens != 0 {
		t.Fatalf("chunks/gen = %d/%d, want 0/0", rec.Chunks, rec.GenTokens)
	}
	if got := a.OutcomeCode(); got != metrics.CodeEmptyCompletion {
		t.Fatalf("outcome = %q, want empty_completion", got)
	}

	// A typed reasoning summary delta is real output presence.
	_, typed := analyzeLines([]string{
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress","reasoning":{"effort":null,"summary":null}}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"think"}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if typed.FirstReasoningAt == 0 {
		t.Fatal("typed reasoning summary delta recorded no reasoning timestamp")
	}

	// The Chat reasoning-content control still runs through the generic scan.
	_, chat := analyzeLines([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	})
	if chat.FirstReasoningAt == 0 {
		t.Fatal("Chat reasoning_content recorded no reasoning timestamp")
	}
}

// prettyDataLines formats one pretty-printed JSON document as SSE data lines
// followed by the event boundary.
func prettyDataLines(t *testing.T, compact string) []string {
	t.Helper()
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(compact), "", "  "); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(indented.String(), "\n")
	for i := range lines {
		lines[i] = "data: " + lines[i]
	}
	return append(lines, "")
}

// TestResponsesReasoningEchoPrettyPrintedSeeded pins F1 on the production
// shape: the relay seeds both analyzers with SetWire before any frame arrives,
// and a pretty-printed official echo must still record no output presence. If
// the wire seed is removed, the multi-line document is not recognized and the
// echo is counted as TTFT, reasoning and a chunk.
func TestResponsesReasoningEchoPrettyPrintedSeeded(t *testing.T) {
	var a Analyzer
	a.SetWire(ir.Responses)
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, payload := range []string{
		`{"type":"response.created","response":{"id":"r1","status":"in_progress","reasoning":{"effort":null,"summary":null}}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed","reasoning":{"effort":null,"summary":null},"output":[]}}`,
	} {
		for _, line := range prettyDataLines(t, payload) {
			a.Feed([]byte(line), now)
		}
	}
	var rec metrics.Record
	a.Fill(&rec)
	if !rec.FirstTokenAt.IsZero() {
		t.Fatalf("first token at %v, want zero for an empty turn", rec.FirstTokenAt)
	}
	if rec.FirstReasoningAt != 0 {
		t.Fatalf("first reasoning at %d, want zero", rec.FirstReasoningAt)
	}
	if rec.Chunks != 0 || rec.GenTokens != 0 {
		t.Fatalf("chunks/gen = %d/%d, want 0/0", rec.Chunks, rec.GenTokens)
	}
	if got := a.OutcomeCode(); got != metrics.CodeEmptyCompletion {
		t.Fatalf("outcome = %q, want empty_completion", got)
	}
}
