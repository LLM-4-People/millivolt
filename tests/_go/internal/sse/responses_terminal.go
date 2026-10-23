package sse

// Batch 4 regression: a Responses terminal event maps its own status and
// incomplete reason onto the record's finish vocabulary, so a short
// max_output_tokens answer is not reclassified as an empty completion and a
// completed tool call keeps a reason.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesTerminalMapsFinishReason(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	cases := []struct {
		name   string
		lines  []string
		finish string
		tools  int
	}{
		{
			name: "completed text",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.output_text.delta","delta":"hi","output_index":0,"content_index":0}`,
				`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
			},
			finish: "stop",
		},
		{
			name: "completed tool call",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"lookup"}}`,
				`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
			},
			finish: "tool_calls",
			tools:  1,
		},
		{
			name: "incomplete max output",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
			},
			finish: "length",
		},
		{
			name: "incomplete max output with terminal tool",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"lookup","arguments":"{}"}]}}`,
			},
			finish: "length",
			tools:  1,
		},
		{
			name: "incomplete max messages",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_messages"}}}`,
			},
			finish: "other",
		},
		{
			name: "cancelled",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.cancelled","response":{"id":"r1","status":"cancelled"}}`,
			},
			finish: "cancelled",
		},
		{
			name: "failed",
			lines: []string{
				`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`,
				`data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
			},
			finish: "error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			for _, line := range tc.lines {
				a.Feed([]byte(line), now)
			}
			var rec metrics.Record
			a.Fill(&rec)
			if a.finishReason != tc.finish {
				t.Fatalf("analyzer finish = %q, want %q", a.finishReason, tc.finish)
			}
			if rec.FinishReason != tc.finish {
				t.Fatalf("record finish = %q, want %q", rec.FinishReason, tc.finish)
			}
			if a.toolCalls != tc.tools {
				t.Fatalf("tool calls = %d, want %d", a.toolCalls, tc.tools)
			}
			if tc.name == "incomplete max output" && a.OutcomeCode() == metrics.CodeEmptyCompletion {
				t.Fatal("a legitimate incomplete max_output_tokens answer was classified as an empty completion")
			}
		})
	}
}

// TestResponsesTerminalOnlyFunctionCall pins N5: a function_call that appears
// only in response.completed.output is a real tool turn, counted once per
// call_id with its name retained, not an empty completion.
func TestResponsesTerminalOnlyFunctionCall(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	lines := []string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`,
		``,
	}
	var a Analyzer
	for _, line := range lines {
		a.Feed([]byte(line), now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	if a.finishReason != "tool_calls" || rec.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q/%q, want tool_calls", a.finishReason, rec.FinishReason)
	}
	if a.toolCalls != 1 || rec.ToolCalls != 1 {
		t.Fatalf("tool calls = %d/%d, want 1", a.toolCalls, rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
		t.Fatalf("tool names = %v, want [lookup]", rec.ToolNames)
	}
	if code := a.OutcomeCode(); code != "" {
		t.Fatalf("outcome = %q, want no degenerate class", code)
	}
	if got := a.ResponseID(); got != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", got)
	}
}

// TestResponsesTerminalOnlyOutputPresence pins N9: answer, refusal, reasoning
// and tool output that appears only in the terminal document is real output.
// It must feed finish classification, the degenerate outcome rules, the
// retained tool names and the preview, exactly like streamed deltas.
func TestResponsesTerminalOnlyOutputPresence(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	cases := []struct {
		name          string
		output        string
		wantFinish    string
		wantOutcome   string
		wantPreview   string
		wantTools     int
		wantToolNames []string
		wantHadAnswer bool
		wantTTFT      bool
		wantAnswerTok int64
		wantGenTokens int64
	}{
		{
			name:          "text",
			output:        `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]`,
			wantFinish:    "stop",
			wantOutcome:   "",
			wantPreview:   "hello",
			wantHadAnswer: true,
			wantTTFT:      true,
			wantAnswerTok: 2,
			wantGenTokens: 2,
		},
		{
			name:        "refusal",
			output:      `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`,
			wantFinish:  "stop",
			wantOutcome: "",
			wantPreview: "no",
		},
		{
			name:        "reasoning summary",
			output:      `[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}]`,
			wantFinish:  "stop",
			wantOutcome: metrics.CodeReasoningOnly,
		},
		{
			name:        "reasoning content",
			output:      `[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"deep"}]}]`,
			wantFinish:  "stop",
			wantOutcome: metrics.CodeReasoningOnly,
		},
		{
			name: "mixed text and tool",
			output: `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
				`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]`,
			wantFinish:    "tool_calls",
			wantOutcome:   "",
			wantPreview:   "hello",
			wantTools:     1,
			wantToolNames: []string{"lookup"},
			wantHadAnswer: true,
			wantTTFT:      true,
			wantAnswerTok: 2,
			wantGenTokens: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			a.CapturePreview = true
			a.Feed([]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`), now)
			a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"output":`+tc.output+`}}`), now)
			a.Feed([]byte(""), now)
			var rec metrics.Record
			rec.FinalAttemptAt = now.Add(-time.Millisecond)
			a.Fill(&rec)
			metrics.FinalizeRecord(&rec)
			if a.finishReason != tc.wantFinish || rec.FinishReason != tc.wantFinish {
				t.Fatalf("finish = %q/%q, want %q", a.finishReason, rec.FinishReason, tc.wantFinish)
			}
			if got := a.OutcomeCode(); got != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got, tc.wantOutcome)
			}
			if rec.ResponsePreview != tc.wantPreview {
				t.Fatalf("preview = %q, want %q", rec.ResponsePreview, tc.wantPreview)
			}
			if rec.ToolCalls != tc.wantTools || a.toolCalls != tc.wantTools {
				t.Fatalf("tool calls = %d/%d, want %d", a.toolCalls, rec.ToolCalls, tc.wantTools)
			}
			if len(rec.ToolNames) != len(tc.wantToolNames) {
				t.Fatalf("tool names = %v, want %v", rec.ToolNames, tc.wantToolNames)
			}
			for i, name := range tc.wantToolNames {
				if rec.ToolNames[i] != name {
					t.Fatalf("tool names = %v, want %v", rec.ToolNames, tc.wantToolNames)
				}
			}
			if rec.HadAnswerContent != tc.wantHadAnswer {
				t.Fatalf("had answer content = %v, want %v", rec.HadAnswerContent, tc.wantHadAnswer)
			}
			if tc.wantTTFT && rec.TTFTMs != 1 {
				t.Fatalf("TTFT = %dms, want 1ms", rec.TTFTMs)
			}
			if tc.wantTTFT && (rec.AnswerTokens != tc.wantAnswerTok || rec.GenTokens != tc.wantGenTokens) {
				t.Fatalf("answer/gen tokens = %d/%d, want %d/%d", rec.AnswerTokens, rec.GenTokens, tc.wantAnswerTok, tc.wantGenTokens)
			}
		})
	}
}

// TestResponsesTerminalTextMatchesStreamed pins the N17 invariant: a terminal
// document that carries text (alone or beside a function call) records the
// same answer presence, token counts and TTFT as the streamed equivalent.
func TestResponsesTerminalTextMatchesStreamed(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	run := func(lines []string) metrics.Record {
		var a Analyzer
		a.CapturePreview = true
		for _, line := range lines {
			a.Feed([]byte(line), now)
		}
		var rec metrics.Record
		rec.FinalAttemptAt = now.Add(-time.Millisecond)
		a.Fill(&rec)
		metrics.FinalizeRecord(&rec)
		return rec
	}
	terminal := run([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`,
	})
	streamed := run([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	})
	if terminal.HadAnswerContent != streamed.HadAnswerContent ||
		terminal.AnswerTokens != streamed.AnswerTokens ||
		terminal.GenTokens != streamed.GenTokens ||
		terminal.TTFTMs != streamed.TTFTMs {
		t.Fatalf("terminal record %+v does not match streamed %+v", terminal, streamed)
	}
	if !terminal.HadAnswerContent || terminal.TTFTMs != 1 {
		t.Fatalf("terminal text did not record answer content and TTFT: %+v", terminal)
	}
}
