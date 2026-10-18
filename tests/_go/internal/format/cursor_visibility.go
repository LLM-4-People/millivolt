package format

// The void predicate consumes TurnResult.VisibleOutput, which must be set at
// the actual emit boundary - not inferred from token usage later.

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestCursorRunVisibleOutputFlag(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(t *testing.T, r *CursorRun) bool
		want    bool
	}{
		{
			name: "empty content is not visible",
			prepare: func(t *testing.T, r *CursorRun) bool {
				return r.pushEvent(runEvent{kind: evDelta, delta: map[string]any{"content": ""}})
			},
		},
		{
			name: "content is visible",
			prepare: func(t *testing.T, r *CursorRun) bool {
				return r.pushEvent(runEvent{kind: evDelta, delta: map[string]any{"content": "answer"}})
			},
			want: true,
		},
		{
			name: "reasoning is visible",
			prepare: func(t *testing.T, r *CursorRun) bool {
				return r.pushEvent(runEvent{kind: evDelta, delta: map[string]any{"reasoning_content": "thinking"}})
			},
			want: true,
		},
		{
			name: "tool call is visible",
			prepare: func(t *testing.T, r *CursorRun) bool {
				call := CursorToolCall{CallID: "call-1", Name: "tool", Args: "{}"}
				if !r.addPending(call) {
					return false
				}
				return r.pushEvent(runEvent{kind: evToolCall, toolCall: &call})
			},
			want: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
			t.Cleanup(r.Close)
			r.events = make(chan runEvent, 8)
			if !tt.prepare(t, r) {
				t.Fatal("fixture event was rejected")
			}
			if !r.pushEvent(runEvent{kind: evDone}) {
				t.Fatal("terminal event was rejected")
			}
			res := r.RunTurn(t.Context(), func(map[string]any) error { return nil })
			if res.Outcome != TurnFinished {
				t.Fatalf("outcome = %v, want TurnFinished", res.Outcome)
			}
			if res.VisibleOutput != tt.want {
				t.Fatalf("VisibleOutput = %v, want %v", res.VisibleOutput, tt.want)
			}
		})
	}
}
