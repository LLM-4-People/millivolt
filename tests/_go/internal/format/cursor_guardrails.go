package format

// Bounded internal guardrails for the resumable Cursor Run queue.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func queuedEventBytes(r *CursorRun) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queuedBytes
}

func pendingCallBytes(r *CursorRun) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingBytes
}

func cursorRunCloseErr(r *CursorRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

// cursorGuardrailExecFrame builds an AgentServerMessage carrying a blocking
// ExecServerMessage.mcp_args call, the same path the pump registers in pending.
func cursorGuardrailExecFrame(t *testing.T, id uint32, callID string) []byte {
	t.Helper()
	mcp := appendString(nil, fMcpArgsName, "tool")
	mcp = appendString(mcp, fMcpArgsToolCallID, callID)
	ex := appendUint32(nil, fExecMessageID, id)
	ex = appendMessage(ex, fExecServerMcpArgs, mcp)
	return appendMessage(nil, fAgentServerMessageExecServerMessage, ex)
}

// TestCursorRunEventSizeEstimator pins the payload accounting: a large tool
// argument must never be undercounted, and a pathological nested value must
// terminate at the walker depth bound instead of recursing without limit.
func TestCursorRunEventSizeEstimator(t *testing.T) {
	arg := strings.Repeat("a", 3<<20)
	callSize := cursorRunEventSize(runEvent{
		kind:     evToolCall,
		toolCall: &CursorToolCall{CallID: "call", Name: "tool", Args: arg},
	})
	if callSize < int64(len(arg)) {
		t.Fatalf("tool argument size = %d, want at least its %d bytes", callSize, len(arg))
	}

	text := strings.Repeat("b", 1<<20)
	reasoning := strings.Repeat("c", 1<<19)
	deltaSize := cursorRunEventSize(runEvent{kind: evDelta, delta: map[string]any{
		"content":           text,
		"reasoning_content": reasoning,
	}})
	if want := int64(len(text) + len(reasoning)); deltaSize < want {
		t.Fatalf("delta size = %d, want at least %d text bytes", deltaSize, want)
	}

	var nested any = "leaf"
	for i := 0; i < 10000; i++ {
		nested = map[string]any{"n": []any{nested}}
	}
	bounded := cursorRunEventSize(runEvent{kind: evDelta, delta: map[string]any{"content": nested}})
	if bounded < 0 || bounded > cursorRunEventValueDepthMax*2 {
		t.Fatalf("nested value size = %d, want a small depth-bounded estimate (max depth %d)", bounded, cursorRunEventValueDepthMax)
	}
}

// TestCursorRunEventQueueByteLimitFailsClosed fills the queue to the exact byte
// budget, then proves the next event is never enqueued, the run closes with the
// descriptive guardrail error, and every received event releases its bytes.
func TestCursorRunEventQueueByteLimitFailsClosed(t *testing.T) {
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)
	r.events = make(chan runEvent, 8) // controlled sink; no pump owns the channel here

	small := runEvent{kind: evDelta, delta: map[string]any{"content": "hello"}}
	if !r.pushEvent(small) {
		t.Fatal("normal small event was rejected")
	}
	smallSize := cursorRunEventSize(small)

	// One event that fills the remaining byte budget exactly. The pieces share
	// one chunk so the test allocates a chunk, not the full budget.
	const chunk = 1 << 20
	if smallSize >= int64(chunk-len("content")) {
		t.Fatalf("fixture small event unexpectedly large: %d bytes", smallSize)
	}
	filler := chunk - len("content") - int(smallSize)
	shared := strings.Repeat("x", chunk)
	parts := make([]any, 0, cursorRunEventBytesMax/chunk)
	for len(parts) < cursorRunEventBytesMax/chunk-1 {
		parts = append(parts, shared)
	}
	parts = append(parts, shared[:filler])
	atLimit := runEvent{kind: evDelta, delta: map[string]any{"content": parts}}
	if got, want := cursorRunEventSize(atLimit), cursorRunEventBytesMax-smallSize; got != want {
		t.Fatalf("fixture event size = %d, want exactly %d", got, want)
	}
	if !r.pushEvent(atLimit) {
		t.Fatal("event filling the remaining byte budget was rejected")
	}
	if got, want := queuedEventBytes(r), int64(cursorRunEventBytesMax); got != want {
		t.Fatalf("queued bytes = %d, want %d", got, want)
	}

	over := runEvent{kind: evDelta, delta: map[string]any{"content": "x"}}
	if r.pushEvent(over) {
		t.Fatal("event beyond the byte budget was queued")
	}
	if !r.Closed() {
		t.Fatal("over-budget push did not tear the run down")
	}
	r.mu.Lock()
	closeErr := r.closeErr
	r.mu.Unlock()
	if !errors.Is(closeErr, errCursorRunEventBytes) {
		t.Fatalf("close reason = %v, want %v", closeErr, errCursorRunEventBytes)
	}
	if !strings.Contains(closeErr.Error(), fmt.Sprint(cursorRunEventBytesMax)) {
		t.Fatalf("close reason = %q, want the configured limit", closeErr)
	}
	if got, want := queuedEventBytes(r), int64(cursorRunEventBytesMax); got != want {
		t.Fatalf("rejected event changed queued bytes: got %d, want %d", got, want)
	}

	// Simulate the pump's deferred event-channel close after the fail-closed
	// return, then drive the queued events. The driver must report the
	// guardrail reason, not a generic unexpected EOF, and end at zero bytes.
	close(r.events)
	res := r.RunTurn(t.Context(), nil)
	if res.Outcome != TurnErrored || !errors.Is(res.Err, errCursorRunEventBytes) {
		t.Fatalf("driver result = %+v, want TurnErrored with %v", res, errCursorRunEventBytes)
	}
	if got := queuedEventBytes(r); got != 0 {
		t.Fatalf("queued bytes after draining = %d, want 0", got)
	}
}

// TestCursorRunPendingByteBudgetFailsClosed fills pending with large argument
// strings until the aggregate payload budget, not the count cap, rejects the
// next new call. Accounting must match the exact sizes admitted.
func TestCursorRunPendingByteBudgetFailsClosed(t *testing.T) {
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)

	arg := strings.Repeat("a", 4<<20)
	var want int64
	var rejected CursorToolCall
	for i := 0; ; i++ {
		call := CursorToolCall{CallID: fmt.Sprintf("call-%d", i), Name: "bulk", Args: arg}
		size := cursorRunToolCallSize(call)
		if want+size > cursorRunPendingBytesMax {
			rejected = call
			break
		}
		if !r.addPending(call) {
			t.Fatalf("insertion %d rejected before the byte budget: %v", i, cursorRunCloseErr(r))
		}
		want += size
	}
	r.mu.Lock()
	accepted := len(r.pending)
	r.mu.Unlock()
	if accepted >= cursorRunPendingCap {
		t.Fatalf("byte fill hit the count cap first: %d pending", accepted)
	}
	if got := pendingCallBytes(r); got != want {
		t.Fatalf("pending bytes = %d, want exactly %d", got, want)
	}

	if r.addPending(rejected) {
		t.Fatal("new call beyond the pending byte budget was accepted")
	}
	if !r.Closed() {
		t.Fatal("pending byte overrun did not tear the run down")
	}
	closeErr := cursorRunCloseErr(r)
	if !errors.Is(closeErr, errCursorRunPendingBytes) {
		t.Fatalf("close reason = %v, want %v", closeErr, errCursorRunPendingBytes)
	}
	if !strings.Contains(closeErr.Error(), fmt.Sprint(cursorRunPendingBytesMax)) {
		t.Fatalf("close reason = %q, want the configured byte limit", closeErr)
	}
	if got := pendingCallBytes(r); got != want {
		t.Fatalf("rejected call changed pending bytes: got %d, want %d", got, want)
	}
	r.mu.Lock()
	after := len(r.pending)
	r.mu.Unlock()
	if after != accepted {
		t.Fatalf("rejected call changed the pending map: got %d entries, want %d", after, accepted)
	}
}

// TestCursorRunPendingDuplicateByteAccounting proves duplicate replacement is
// charged by delta, not by adding the full new size, and that a replacement
// which would overrun the byte budget fails closed without corrupting the
// existing entry or its accounting.
func TestCursorRunPendingDuplicateByteAccounting(t *testing.T) {
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)

	small := strings.Repeat("s", 1<<10)
	large := strings.Repeat("l", 8<<20)
	smaller := strings.Repeat("m", 1<<9)
	call := CursorToolCall{CallID: "dup", Name: "tool", Args: small}
	if !r.addPending(call) {
		t.Fatal("initial duplicate candidate was rejected")
	}
	if got, want := pendingCallBytes(r), cursorRunToolCallSize(call); got != want {
		t.Fatalf("initial pending bytes = %d, want %d", got, want)
	}

	call.Args = large
	if !r.addPending(call) {
		t.Fatalf("larger duplicate replacement was rejected: %v", cursorRunCloseErr(r))
	}
	want := cursorRunToolCallSize(call)
	if got := pendingCallBytes(r); got != want {
		t.Fatalf("larger replacement bytes = %d, want exactly %d", got, want)
	}

	call.Args = smaller
	if !r.addPending(call) {
		t.Fatalf("smaller duplicate replacement was rejected: %v", cursorRunCloseErr(r))
	}
	want = cursorRunToolCallSize(call)
	if got := pendingCallBytes(r); got != want {
		t.Fatalf("smaller replacement bytes = %d, want exactly %d", got, want)
	}

	// Fill the remaining budget with other calls so replacing dup with the
	// large arguments can only fit if the old entry's bytes were not released.
	fillerArg := strings.Repeat("f", 4<<20)
	for i := 0; ; i++ {
		filler := CursorToolCall{CallID: fmt.Sprintf("filler-%d", i), Name: "tool", Args: fillerArg}
		size := cursorRunToolCallSize(filler)
		if want+size > cursorRunPendingBytesMax {
			break
		}
		if !r.addPending(filler) {
			t.Fatalf("filler insertion %d was rejected: %v", i, cursorRunCloseErr(r))
		}
		want += size
	}
	if got := pendingCallBytes(r); got != want {
		t.Fatalf("filled pending bytes = %d, want exactly %d", got, want)
	}
	r.mu.Lock()
	beforeCount := len(r.pending)
	r.mu.Unlock()

	call.Args = large
	if r.addPending(call) {
		t.Fatal("duplicate replacement beyond the pending byte budget was accepted")
	}
	closeErr := cursorRunCloseErr(r)
	if !errors.Is(closeErr, errCursorRunPendingBytes) {
		t.Fatalf("close reason = %v, want %v", closeErr, errCursorRunPendingBytes)
	}
	if got := pendingCallBytes(r); got != want {
		t.Fatalf("failed replacement changed pending bytes: got %d, want %d", got, want)
	}
	r.mu.Lock()
	kept := r.pending["dup"].Args
	afterCount := len(r.pending)
	r.mu.Unlock()
	if kept != smaller {
		t.Fatalf("failed replacement lost the existing entry: args length %d, want %d", len(kept), len(smaller))
	}
	if afterCount != beforeCount {
		t.Fatalf("failed replacement changed the pending map: got %d entries, want %d", afterCount, beforeCount)
	}
}

// TestCursorRunResumeReleasesPendingBytes consumes a pending call through the
// real resume path and asserts its payload bytes are released with the entry.
func TestCursorRunResumeReleasesPendingBytes(t *testing.T) {
	var r *CursorRun
	out := runResultWriter(func(p []byte) (int, error) {
		r.events <- runEvent{kind: evDone}
		return len(p), nil
	})
	r = NewCursorRun(out, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)

	call := CursorToolCall{CallID: "call", Name: "tool", Args: strings.Repeat("a", 2<<20)}
	if !r.addPending(call) {
		t.Fatal("pending call was rejected")
	}
	if got, want := pendingCallBytes(r), cursorRunToolCallSize(call); got != want {
		t.Fatalf("pending bytes = %d, want %d", got, want)
	}

	res := r.ResumeTurn(t.Context(), map[string]ToolResult{"call": {Text: "ok"}}, "", nil)
	if res.Outcome != TurnFinished || res.Err != nil {
		t.Fatalf("resume result = %+v, want TurnFinished", res)
	}
	if got := pendingCallBytes(r); got != 0 {
		t.Fatalf("pending bytes after consume = %d, want 0", got)
	}
	if ids := r.PendingToolCallIDs(); len(ids) != 0 {
		t.Fatalf("pending ids after consume = %v, want none", ids)
	}
}

// TestCursorRunPendingCapFailsClosed exercises the same insertion owner the
// pump uses: duplicates replace normally at any size, while one new id past the
// cap fails the run closed with its descriptive error.
func TestCursorRunPendingCapFailsClosed(t *testing.T) {
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)

	if !r.addPending(CursorToolCall{CallID: "dup", Name: "first"}) {
		t.Fatal("first duplicate insertion was rejected")
	}
	if !r.addPending(CursorToolCall{CallID: "dup", Name: "second"}) {
		t.Fatal("duplicate below the cap was rejected")
	}
	for i := 1; i < cursorRunPendingCap; i++ {
		id := fmt.Sprintf("call-%d", i)
		if !r.addPending(CursorToolCall{CallID: id, Name: "tool"}) {
			t.Fatalf("pending insertion %d of %d was rejected", i+1, cursorRunPendingCap)
		}
	}
	if !r.addPending(CursorToolCall{CallID: "dup", Name: "third"}) {
		t.Fatal("duplicate id at the cap was rejected; the cap must apply only to new ids")
	}
	r.mu.Lock()
	dupName, pendingLen := r.pending["dup"].Name, len(r.pending)
	r.mu.Unlock()
	if dupName != "third" || pendingLen != cursorRunPendingCap {
		t.Fatalf("duplicate replacement = %q with %d pending, want third/%d", dupName, pendingLen, cursorRunPendingCap)
	}
	beforeBytes := pendingCallBytes(r)

	if r.addPending(CursorToolCall{CallID: "overflow", Name: "tool"}) {
		t.Fatal("new pending call beyond the cap was accepted")
	}
	if !r.Closed() {
		t.Fatal("pending over-cap insertion did not tear the run down")
	}
	r.mu.Lock()
	closeErr := r.closeErr
	afterLen := len(r.pending)
	r.mu.Unlock()
	if !errors.Is(closeErr, errCursorRunPendingCap) {
		t.Fatalf("close reason = %v, want %v", closeErr, errCursorRunPendingCap)
	}
	if got := pendingCallBytes(r); got != beforeBytes {
		t.Fatalf("rejected call changed pending bytes: got %d, want %d", got, beforeBytes)
	}
	if afterLen != cursorRunPendingCap {
		t.Fatalf("rejected call changed the pending map: got %d entries, want %d", afterLen, cursorRunPendingCap)
	}

	close(r.events)
	res := r.RunTurn(t.Context(), nil)
	if res.Outcome != TurnErrored || !errors.Is(res.Err, errCursorRunPendingCap) {
		t.Fatalf("driver result = %+v, want TurnErrored with %v", res, errCursorRunPendingCap)
	}
}

// TestCursorRunPendingCapStopsPumpBeforeNextFrame proves the pump fails at the
// insertion site and never reads the frame queued behind the over-cap call.
func TestCursorRunPendingCapStopsPumpBeforeNextFrame(t *testing.T) {
	frames := append(env(t, cursorGuardrailExecFrame(t, 1, "overflow")), env(t, textDeltaMsg(t, "late"))...)
	r := NewCursorRun(io.Discard, bytes.NewReader(frames), nil, nil, time.Hour)
	t.Cleanup(r.Close)
	for i := 0; i < cursorRunPendingCap; i++ {
		if !r.addPending(CursorToolCall{CallID: fmt.Sprintf("call-%d", i), Name: "tool"}) {
			t.Fatalf("prefill insertion %d was rejected", i+1)
		}
	}

	r.pump()

	var emitted []string
	res := r.RunTurn(t.Context(), func(delta map[string]any) error {
		if text, ok := delta["content"].(string); ok {
			emitted = append(emitted, text)
		}
		return nil
	})
	if res.Outcome != TurnErrored || !errors.Is(res.Err, errCursorRunPendingCap) {
		t.Fatalf("driver result = %+v, want TurnErrored with %v", res, errCursorRunPendingCap)
	}
	if len(emitted) != 0 {
		t.Fatalf("pump read past the over-cap call: emitted %v", emitted)
	}
}

// TestCursorRunGuardrailsPreserveNormalParkAndResume is the normal-path
// regression: a few small deltas and parallel tool calls still settle, park,
// resume, and finish with zero queued bytes.
func TestCursorRunGuardrailsPreserveNormalParkAndResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var r *CursorRun
		out := runResultWriter(func(p []byte) (int, error) {
			r.events <- runEvent{kind: evDone}
			return len(p), nil
		})
		r = NewCursorRun(out, bytes.NewReader(nil), nil, nil, time.Hour)
		t.Cleanup(r.Close)
		r.events = make(chan runEvent, 16)

		calls := []CursorToolCall{
			{CallID: "call-a", Name: "one", Args: `{"x":1}`},
			{CallID: "call-b", Name: "two", Args: `{}`},
		}
		for i := range calls {
			call := calls[i]
			if !r.addPending(call) {
				t.Fatalf("pending call %q was rejected", call.CallID)
			}
			if !r.pushEvent(runEvent{kind: evToolCall, toolCall: &call}) {
				t.Fatalf("tool-call event %q was rejected", call.CallID)
			}
			if !r.pushEvent(runEvent{kind: evDelta, delta: map[string]any{"content": "small chunk"}}) {
				t.Fatal("small delta was rejected")
			}
		}

		done := make(chan TurnResult, 1)
		go func() { done <- r.RunTurn(t.Context(), func(map[string]any) error { return nil }) }()
		synctest.Wait()
		time.Sleep(cursorToolSettleIdleMs + time.Millisecond)
		synctest.Wait()

		parked := <-done
		if parked.Outcome != TurnParked || len(parked.ToolCalls) != 2 {
			t.Fatalf("parked result = %+v, want two surfaced calls", parked)
		}
		resumed := r.ResumeTurn(t.Context(), map[string]ToolResult{
			"call-a": {Text: "ra"},
			"call-b": {Text: "rb"},
		}, "", nil)
		if resumed.Outcome != TurnFinished || resumed.Err != nil {
			t.Fatalf("resumed result = %+v, want TurnFinished", resumed)
		}
		if got := queuedEventBytes(r); got != 0 {
			t.Fatalf("queued bytes after park/resume = %d, want 0", got)
		}
		if got := pendingCallBytes(r); got != 0 {
			t.Fatalf("pending bytes after park/resume = %d, want 0", got)
		}
	})
}
