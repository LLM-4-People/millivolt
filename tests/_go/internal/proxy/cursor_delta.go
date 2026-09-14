package proxy

import (
	"io"
	"testing"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// A Cursor tool-call turn with streamed text before the call is a MIXED
// response - FinalizeRecord must keep its answer tokens. Regression for the
// quality-gate finding: cursorTrackDelta is the only path feeding cursor
// records, and it never set HadAnswerContent, so a text+tool_call cursor turn
// was misclassified tool-only (answer/gen tokens zeroed).
func TestCursorTrackDeltaSetsHadAnswerContent(t *testing.T) {
	rec := &metrics.Record{}

	// A text delta arrives before the tool call parks.
	cursorTrackDelta(rec, map[string]any{"content": "Let me check that. "})
	if !rec.HadAnswerContent {
		t.Fatal("content delta must set HadAnswerContent")
	}
	// The tool-call delta itself does not carry content.
	cursorTrackDelta(rec, map[string]any{"tool_calls": []any{map[string]any{"id": "c1"}}})

	rec.FinishReason = "tool_calls"
	rec.FirstTokenAt = rec.LastTokenAt.Add(-100 * 1e6) // 100ms window
	rec.End = rec.LastTokenAt
	rec.Usage.OutputTokens = 40
	rec.Usage.ReasoningTokens = 5
	metrics.FinalizeRecord(rec)

	// Mixed turn: answer tokens kept (40 output - 5 reasoning = 35), not zeroed.
	if rec.AnswerTokens != 35 {
		t.Errorf("AnswerTokens = %d, want 35 (mixed content+tool keeps answer tokens)", rec.AnswerTokens)
	}
	if rec.GenTokens != 40 {
		t.Errorf("GenTokens = %d, want 40 (reasoning 5 + answer 35)", rec.GenTokens)
	}

	// Contrast: a tool-call-ONLY turn (no content deltas) stays zero-answer.
	rec2 := &metrics.Record{}
	cursorTrackDelta(rec2, map[string]any{"tool_calls": []any{map[string]any{"id": "c1"}}})
	rec2.FinishReason = "tool_calls"
	rec2.FirstTokenAt = rec2.LastTokenAt.Add(-50 * 1e6)
	rec2.End = rec2.LastTokenAt
	rec2.Usage.OutputTokens = 17 // all tool-arg tokens
	metrics.FinalizeRecord(rec2)
	if rec2.HadAnswerContent {
		t.Error("tool-call delta must not set HadAnswerContent")
	}
	if rec2.AnswerTokens != 0 || rec2.GenTokens != 0 {
		t.Errorf("tool-only turn: AnswerTokens=%d GenTokens=%d, want 0/0", rec2.AnswerTokens, rec2.GenTokens)
	}
}

// TestOpenCursorStreamOpeningDeltaDisconnect pins the disconnect accounting
// for a client that died before the turn's first content frame: the opening
// role delta writes to the dead socket and openCursorStream must mark the
// disconnect immediately, because a turn that then ends with zero delta
// events would never reach an emit-failure path and would otherwise record a
// clean outcome. The flag must also SURVIVE finishRunTurn's zero-delta finish
// (markGone is monotone, it never clears the flag) with cursor's documented
// exception: the committed upstream 200 stays the record's status.
func TestOpenCursorStreamOpeningDeltaDisconnect(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}

	emit := openCursorStream(failingSSEWriter{}, rec, "cursor-1", "neutral-model")
	if !rec.ClientDisconnected {
		t.Fatal("the opening role delta hit a dead socket: ClientDisconnected must be set immediately")
	}

	// A turn that finishes with zero delta events renders only the terminal
	// frames on the same dead socket.
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	s.finishRunTurn(failingSSEWriter{}, run,
		providerformat.TurnResult{Outcome: providerformat.TurnFinished},
		emit, rec, "cursor-1", cursorTurnRender{est: 10}, false)

	if !rec.ClientDisconnected {
		t.Error("the zero-delta turn finish must keep the disconnect flag set at stream open")
	}
	if rec.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (cursor's documented exception: never markClientGone's 499)", rec.StatusCode)
	}
	if rec.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop (the turn itself finished normally)", rec.FinishReason)
	}
}
