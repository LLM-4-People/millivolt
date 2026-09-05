package format

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func completedRun(t *testing.T, frames []byte) *CursorRun {
	t.Helper()
	r := NewCursorRun(io.Discard, bytes.NewReader(frames), nil, nil, time.Hour)
	t.Cleanup(r.Close)
	// Decode to completion before starting the driver. The small fixture fits
	// the existing event buffer; this forces the queued-data/reader-exit race.
	r.pump()
	return r
}

func TestTurnPublisherClosesAfterQueuedEvents(t *testing.T) {
	frames := append(env(t, textDeltaMsg(t, "answer")), endStreamEnv(t, `{}`)...)
	r := completedRun(t, frames)
	if first, last := <-r.events, <-r.events; first.kind != evDelta || last.kind != evDone {
		t.Fatalf("publisher reordered data and terminal: %v, %v", first.kind, last.kind)
	}
	select {
	case _, ok := <-r.events:
		if ok {
			t.Fatal("unexpected extra event")
		}
	default:
		t.Fatal("publisher exited without closing its FIFO event stream")
	}
}

func TestTurnDrainsBufferedDeltasBeforeTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  []byte
		err  bool
	}{
		{"unmarked frame-boundary EOF", nil, true},
		{"turn-ended frame-boundary EOF", env(t, turnEndedField14(t)), false},
		{"success trailer", endStreamEnv(t, `{}`), false},
		{"error trailer", endStreamEnv(t, `{"error":{"code":"internal","message":"fixture failure"}}`), true},
		{"truncated frame", []byte{0, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := env(t, textDeltaMsg(t, "first"))
			frames = append(frames, env(t, thinkingDeltaMsg(t, "thinking"))...)
			frames = append(frames, env(t, textDeltaMsg(t, "last"))...)
			frames = append(frames, env(t, tokenDeltaMsg(t, 17))...)
			frames = append(frames, tc.end...)
			r := completedRun(t, frames)
			var got []string
			result := r.RunTurn(t.Context(), func(delta map[string]any) error {
				for _, k := range []string{"content", "reasoning_content"} {
					if v, ok := delta[k].(string); ok {
						got = append(got, v)
					}
				}
				return nil
			})
			if !slices.Equal(got, []string{"first", "thinking", "last"}) || result.Output != 17 || (result.Err != nil) != tc.err {
				t.Fatalf("buffered events lost: deltas=%v result=%+v", got, result)
			}
			if (result.Outcome == TurnErrored) != tc.err {
				t.Fatalf("terminal outcome = %+v", result)
			}
		})
	}
}

func TestTurnSettlingRetainsDeltasAndErrors(t *testing.T) {
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)
	first := CursorToolCall{CallID: "first", Name: "tool", Args: `{}`}
	second := CursorToolCall{CallID: "second", Name: "tool", Args: `{}`}
	r.pending[first.CallID], r.pending[second.CallID] = first, second
	wantErr := errors.New("terminal failure")
	for _, ev := range []runEvent{
		{kind: evToolCall, toolCall: &first},
		{kind: evDelta, delta: map[string]any{"content": "after first call"}},
		{kind: evToolCall, toolCall: &second},
		{kind: evDelta, delta: map[string]any{"reasoning_content": "after second call"}},
		{kind: evErr, err: wantErr},
	} {
		r.events <- ev
	}
	var deltas int
	result := r.RunTurn(t.Context(), func(delta map[string]any) error {
		if delta["content"] != nil || delta["reasoning_content"] != nil {
			deltas++
		}
		return nil
	})
	if deltas != 2 || len(result.ToolCalls) != 2 || !errors.Is(result.Err, wantErr) {
		t.Fatalf("settling discarded queued events: deltas=%d result=%+v", deltas, result)
	}
}

func TestTurnSettleWindowRearmsForSibling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
		t.Cleanup(r.Close)
		first := CursorToolCall{CallID: "first", Name: "tool"}
		second := CursorToolCall{CallID: "second", Name: "tool"}
		r.pending[first.CallID], r.pending[second.CallID] = first, second
		r.events <- runEvent{kind: evToolCall, toolCall: &first}
		done := make(chan TurnResult, 1)
		go func() { done <- r.RunTurn(t.Context(), nil) }()
		synctest.Wait()
		time.Sleep(cursorToolSettleIdleMs / 2)
		r.events <- runEvent{kind: evToolCall, toolCall: &second}
		synctest.Wait()
		time.Sleep(cursorToolSettleIdleMs / 2)
		select {
		case <-done:
			t.Fatal("sibling did not rearm the existing settle window")
		default:
		}
		time.Sleep(cursorToolSettleIdleMs / 2)
		synctest.Wait()
		if result := <-done; result.Outcome != TurnParked || len(result.ToolCalls) != 2 {
			t.Fatalf("settled result = %+v", result)
		}
	})
}

func TestReaderExitRetiresHeartbeatAndTransport(t *testing.T) {
	var closes atomic.Int32
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, func() { closes.Add(1) }, time.Hour)
	t.Cleanup(r.Close)
	r.pump()
	select {
	case <-r.stopPump:
	default:
		t.Fatal("finished reader left heartbeat running")
	}
	r.Close()
	if !r.Closed() || closes.Load() != 1 {
		t.Fatalf("transport teardown not exactly once: closed=%v calls=%d", r.Closed(), closes.Load())
	}
}

func TestMalformedTerminalCannotBecomeSuccess(t *testing.T) {
	for _, raw := range []string{"", " \n ", `{`, `null`, `[]`, `true`, `{} {}`, `{"error":42}`, `{"error":{"code":1}}`, `{"error":null}`, `{"error":{}}`, `{"error":{"code":null}}`} {
		t.Run(raw, func(t *testing.T) {
			r := completedRun(t, endStreamEnv(t, raw))
			if result := r.RunTurn(t.Context(), nil); result.Outcome != TurnErrored || result.Err == nil {
				t.Fatalf("malformed terminal was accepted: %+v", result)
			}
		})
	}
}

func TestTurnInterruptedWithoutTerminalIsError(t *testing.T) {
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	r.Close()
	r.pump() // interrupted before any terminal event can be published
	if result := r.RunTurn(t.Context(), nil); result.Outcome != TurnErrored || !errors.Is(result.Err, io.ErrUnexpectedEOF) {
		t.Fatalf("interrupted reader fabricated success: %+v", result)
	}
}

type runResultWriter func([]byte) (int, error)

func (fn runResultWriter) Write(p []byte) (int, error) { return fn(p) }

func TestResumeOwnsUsageResetBeforeWritingResults(t *testing.T) {
	var r *CursorRun
	out := runResultWriter(func(p []byte) (int, error) {
		// A fast upstream continuation can publish usage immediately after the
		// tool result write, before ResumeTurn reaches its event loop.
		r.mu.Lock()
		r.usagePrompt, r.usageDeltaSum = 222, r.usageDeltaSum+7
		r.mu.Unlock()
		r.events <- runEvent{kind: evDone}
		return len(p), nil
	})
	r = NewCursorRun(out, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)
	r.usagePrompt, r.usageDeltaSum = 111, 99
	r.pending["call"] = CursorToolCall{CallID: "call", Name: "tool"}
	result := r.ResumeTurn(t.Context(), map[string]struct {
		Text    string
		IsError bool
	}{"call": {Text: "result"}}, "", nil)
	if result.Outcome != TurnFinished || result.Output != 7 || result.Prompt != 222 {
		t.Fatalf("continuation retained past-turn usage or erased new usage: %+v", result)
	}
}

func TestKnownUpdateDecodeErrorsStopPump(t *testing.T) {
	interaction := func(field int, payload []byte) []byte {
		return appendMessage(nil, fAgentServerMessageInteractionUpdate, appendMessage(nil, field, payload))
	}
	for _, tc := range []struct {
		name string
		msg  []byte
	}{
		{"interaction", appendMessage(nil, fAgentServerMessageInteractionUpdate, []byte{0x0a, 0x80})},
		{"text", interaction(fInteractionUpdateTextDelta, []byte{0x0a, 0x80})},
		{"thinking", interaction(fInteractionUpdateThinkingDelta, []byte{0x0a, 0x80})},
		{"tokens", interaction(fInteractionUpdateTokenDelta, []byte{0x08, 0x80})},
		{"turn ended", interaction(fInteractionUpdateTurnEnded, []byte{0x08, 0x80})},
		{"text wrong message wire", appendMessage(nil, fAgentServerMessageInteractionUpdate, appendInt32(nil, fInteractionUpdateTextDelta, 1))},
		{"checkpoint", appendMessage(nil, fAgentServerMessageConversationCheckpointUpdate, []byte{0x2a, 0x80})},
		{"checkpoint token details", appendMessage(nil, fAgentServerMessageConversationCheckpointUpdate, appendMessage(nil, fConversationStateTokenDetails, []byte{0x08, 0x80}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := env(t, textDeltaMsg(t, "before"))
			frames = append(frames, env(t, tokenDeltaMsg(t, 17))...)
			frames = append(frames, env(t, checkpointMsg(t, 23))...)
			frames = append(frames, env(t, tc.msg)...)
			frames = append(frames, env(t, textDeltaMsg(t, "after"))...)
			frames = append(frames, env(t, tokenDeltaMsg(t, 999))...)
			frames = append(frames, env(t, checkpointMsg(t, 99))...)
			frames = append(frames, endStreamEnv(t, `{}`)...)
			r := completedRun(t, frames)
			var got []string
			result := r.RunTurn(t.Context(), func(delta map[string]any) error {
				if text, ok := delta["content"].(string); ok {
					got = append(got, text)
				}
				return nil
			})
			if result.Outcome != TurnErrored || result.Err == nil || !slices.Equal(got, []string{"before"}) || result.Output != 17 || result.Prompt != 23 {
				t.Fatalf("malformed payload was ignored or pump continued beyond it: deltas=%v result=%+v", got, result)
			}
		})
	}
}

func TestKnownUpdateEmptyAndUnknownFieldsRemainValid(t *testing.T) {
	// Empty embedded messages are valid defaults; an unknown length-delimited
	// extension is opaque, even when its bytes are not themselves protobuf.
	unknown := appendMessage(nil, 63, []byte{0x80})
	var frames []byte
	for _, msg := range [][]byte{
		nil,
		unknown,
		appendMessage(nil, fAgentServerMessageInteractionUpdate, nil),
		appendMessage(nil, fAgentServerMessageInteractionUpdate, unknown),
		appendMessage(nil, fAgentServerMessageConversationCheckpointUpdate, nil),
		appendMessage(nil, fAgentServerMessageConversationCheckpointUpdate, unknown),
		appendMessage(nil, fAgentServerMessageConversationCheckpointUpdate, appendMessage(nil, fConversationStateTokenDetails, nil)),
	} {
		frames = append(frames, env(t, msg)...)
	}
	for _, field := range []int{fInteractionUpdateTextDelta, fInteractionUpdateThinkingDelta, fInteractionUpdateTokenDelta, fInteractionUpdateTurnEnded} {
		for _, payload := range [][]byte{nil, unknown} {
			msg := appendMessage(nil, fAgentServerMessageInteractionUpdate, appendMessage(nil, field, payload))
			frames = append(frames, env(t, msg)...)
		}
	}
	frames = append(frames, env(t, textDeltaMsg(t, "answer"))...)
	frames = append(frames, env(t, tokenDeltaMsg(t, 17))...)
	frames = append(frames, env(t, checkpointMsg(t, 23))...)
	frames = append(frames, endStreamEnv(t, `{}`)...)
	r := completedRun(t, frames)
	var got []string
	result := r.RunTurn(t.Context(), func(delta map[string]any) error {
		if text, ok := delta["content"].(string); ok {
			got = append(got, text)
		}
		return nil
	})
	if result.Outcome != TurnFinished || result.Err != nil || !slices.Equal(got, []string{"answer"}) || result.Output != 17 || result.Prompt != 23 {
		t.Fatalf("valid defaults/extensions changed decoding: deltas=%v result=%+v", got, result)
	}
}

func TestTurnEOFRequiresCurrentCompletionMarker(t *testing.T) {
	lateUsage := env(t, tokenDeltaMsg(t, 17))
	lateUsage = append(lateUsage, env(t, turnEndedField14(t))...)
	lateUsage = append(lateUsage, env(t, checkpointMsg(t, 23))...)
	for _, tc := range []struct {
		name   string
		frames []byte
		err    bool
		prompt int64
		output int64
	}{
		{name: "empty EOF", err: true},
		{name: "unmarked EOF", frames: env(t, textDeltaMsg(t, "partial")), err: true},
		{name: "turn marker EOF", frames: env(t, turnEndedField14(t))},
		{name: "late checkpoint after turn marker", frames: lateUsage, prompt: 23, output: 17},
		{name: "explicit terminal without turn marker", frames: endStreamEnv(t, `{}`)},
		{name: "partial header after marker", frames: append(env(t, turnEndedField14(t)), 0, 0), err: true},
		{name: "partial body after marker", frames: append(env(t, turnEndedField14(t)), 0, 0, 0, 0, 1), err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := completedRun(t, tc.frames)
			result := r.RunTurn(t.Context(), nil)
			if (result.Outcome == TurnErrored) != tc.err || (result.Err != nil) != tc.err || result.Prompt != tc.prompt || result.Output != tc.output {
				t.Fatalf("completion evidence was misclassified: %+v", result)
			}
			if tc.err && (tc.name == "empty EOF" || tc.name == "unmarked EOF") && !errors.Is(result.Err, io.ErrUnexpectedEOF) {
				t.Fatalf("unmarked EOF error = %v, want unexpected EOF", result.Err)
			}
		})
	}
}

func TestResumeClearsPreviousCompletionMarker(t *testing.T) {
	for _, currentMarker := range []bool{false, true} {
		t.Run(fmt.Sprint(currentMarker), func(t *testing.T) {
			var frames []byte
			if currentMarker {
				frames = env(t, turnEndedField14(t))
			}
			var r *CursorRun
			out := runResultWriter(func(p []byte) (int, error) {
				// The current turn can reach EOF as soon as its tool result is
				// written. Prior-turn state must already be retired here.
				r.pump()
				return len(p), nil
			})
			r = NewCursorRun(out, bytes.NewReader(frames), nil, nil, time.Hour)
			t.Cleanup(r.Close)
			r.handleInteractionUpdate(&protoField{wire: wireBytes, raw: appendMessage(nil, fInteractionUpdateTurnEnded, nil)}, func(runEvent) bool { return true })
			r.pending["call"] = CursorToolCall{CallID: "call", Name: "tool"}
			result := r.ResumeTurn(t.Context(), map[string]struct {
				Text    string
				IsError bool
			}{"call": {Text: "result"}}, "", nil)
			if currentMarker {
				if result.Outcome != TurnFinished || result.Err != nil {
					t.Fatalf("current-turn completion marker lost: %+v", result)
				}
			} else if result.Outcome != TurnErrored || !errors.Is(result.Err, io.ErrUnexpectedEOF) {
				t.Fatalf("past-turn completion marker leaked into resumed turn: %+v", result)
			}
		})
	}
}
