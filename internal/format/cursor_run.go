package format

// Resumable full-duplex Cursor agent.v1 Run.
//
// A Cursor Run is one turn over a long-lived HTTP/2 stream. When the model
// invokes a client-declared tool, the server sends a blocking
// ExecServerMessage.mcp_args and PARKS - it holds the stream open waiting for
// an ExecClientMessage.mcp_result on the SAME stream. To mimic Cursor's own
// agent loop (and avoid a fresh handshake per tool turn), the proxy keeps that
// stream open across multiple client HTTP requests: a tool result arriving on
// a later request is written back into the parked stream, and the turn
// continues where it left off.
//
// CursorRun owns one such stream: the request-body writer, the response
// reader, a heartbeat, and the background pump that reads server frames and
// answers callbacks. Each client HTTP request drives a "turn" via RunTurn /
// ResumeTurn, which swaps in that request's SSE sink and blocks until the turn
// parks (tool call), finishes, or errors.
//
// Field numbers / encoders live in cursor_bidi.go; this file is the lifecycle.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TurnOutcome tells the proxy how a turn ended.
type TurnOutcome int

const (
	TurnFinished TurnOutcome = iota // normal completion (finish_reason "stop")
	TurnParked                      // a tool call surfaced (finish_reason "tool_calls"); stream parked
	TurnErrored                     // a fatal stream/translation error
)

// TurnResult is the outcome of driving one turn.
type TurnResult struct {
	Outcome   TurnOutcome
	ToolCalls []CursorToolCall // the surfaced calls (when Outcome == TurnParked)
	// Prompt is the conversation's current context occupancy, from the
	// checkpoint update's token_details.used_tokens (0 if no checkpoint
	// arrived - the caller then falls back to the request-payload estimate).
	// It is NEVER a cumulative whole-loop total: the wire carries no
	// prompt/completion summary, only this occupancy figure.
	Prompt int64
	// Output is the per-turn generated token total (the TokenDeltaUpdate sum -
	// the only token counter on the InteractionUpdate channel).
	Output int64
	// Reasoning is always 0: Cursor's wire reports a single combined output
	// counter with no answer/reasoning split.
	Reasoning int64
	// VisibleOutput reports whether the turn emitted any client-visible delta:
	// non-empty content/reasoning text or a tool call. It is the void
	// predicate's only signal - token usage is not a visibility signal.
	VisibleOutput bool
	Err           error // when Outcome == TurnErrored
	// ClientAbort marks a TurnErrored caused by writing the turn's OpenAI SSE
	// to the LOCAL client failing (emitDelta error - the client disconnected
	// mid-stream). That is a cancellation, not an upstream failure, so the
	// proxy records it as client-disconnected instead of stream_read_error.
	ClientAbort bool
}

// CursorToolCall is a client-declared tool the model invoked (parsed from the
// server's blocking ExecServerMessage.mcp_args). CallID is the OpenAI-facing
// tool_call id (echoed back by the client in the role:"tool" message).
type CursorToolCall struct {
	CallID string
	Name   string
	Args   string // JSON

	execID    uint64 // server ExecServerMessage.id, echoed in the result
	execIDStr []byte // server ExecServerMessage.exec_id, echoed if present
}

// runEvent is one thing the pump surfaced (internal).
type runEvent struct {
	kind     int // evDelta / evToolCall / evDone / evErr
	delta    map[string]any
	toolCall *CursorToolCall
	err      error
	size     int64 // retained payload bytes, set once by pushEvent
}

const (
	evDelta = iota
	evToolCall
	evDone
	evErr
)

// CursorRun is one resumable Cursor Run stream (one conversation). The pump
// reads server frames continuously; tool results are written in via Resume.
type CursorRun struct {
	sw      *streamWriter
	in      *connectReader
	blobs   *KVBlobStore
	closeFn func()

	// heartbeat is the ClientHeartbeat cadence (idle parked runs included),
	// supplied by the caller from the live config at run creation.
	heartbeat time.Duration

	mu      sync.Mutex
	events  chan runEvent             // pump -> current turn driver
	closed  bool                      // Close's teardown has run (including pump exit)
	pending map[string]CursorToolCall // tool_call_id -> call awaiting a result
	// pendingBytes is the sum of cursorRunToolCallSize over pending, so the
	// count cap cannot be defeated by a few maximal argument strings. addPending
	// and takePendingLocked are its only mutators, both under r.mu.
	pendingBytes int64

	// queuedBytes is the sum of runEvent.size for events queued or in flight
	// from the pump to the turn driver. pushEvent is the only adder and
	// releaseEvent is the only subtractor. It is meaningful only while the run
	// is open: a fail-closed run may leave queued events undrained, so the
	// residual counter is intentionally irrelevant once Close has run.
	queuedBytes int64
	// closeErr is the first fail-closed guardrail reason, surfaced to the
	// driver instead of the generic unexpected-EOF close. Close does not clear
	// it, so a parked run's next resume also reports the real cause.
	closeErr error

	// retention is the parked-run store's aggregate byte observer. While a run
	// is parked the store installs it through SetRetentionLimit; it is called
	// under mu with the run's pending+queued total after every accounting
	// change, and a non-nil error fails the run closed. Guarded by mu.
	retention func(total int64) error

	// usage accumulators (pump-written). usagePrompt is the conversation's
	// current context occupancy (checkpoint token_details.used_tokens);
	// usageDeltaSum is the per-turn OUTPUT total accumulated from
	// TokenDeltaUpdate increments (the only token counter on the wire).
	usagePrompt   int64
	usageDeltaSum int64
	// A validated TurnEndedUpdate permits the existing marker+EOF completion
	// convention without treating an arbitrary frame-boundary disconnect as
	// success. Keep reading after the marker for late checkpoint usage.
	turnEnded bool

	stopPump chan struct{}

	// idlePauser is the transport body's idle-bound seam, when it exposes one.
	// A parked run by definition makes no read progress until a client tool
	// result arrives, so the run pauses the generic bound while parked and
	// resumes it when the continuation starts. nil when the body has no bound.
	idlePauser interface {
		PauseIdle()
		ResumeIdle()
	}
}

// cursorToolSettleIdleMs is how long to wait for sibling tool calls after the
// FIRST call of a turn before parking. Parallel calls arrive essentially
// together; this bounded window (internal guardrail, not user-tunable) catches
// them without stalling the client on a slow single-call turn.
const cursorToolSettleIdleMs = 800 * time.Millisecond

// runEventChanBuf bounds the pump→turn-driver event buffer: it absorbs bursty
// frame decoding so the pump (which also answers KV/exec callbacks and
// heartbeats) keeps running without stalling on the driver, while bounding how
// much may queue behind a slow one. Internal buffering guardrail, not
// user-tunable.
const runEventChanBuf = 64

// cursorRunEventBytesMax bounds the payload bytes retained by events queued or
// in flight in CursorRun.events. Upstream frames can be connectMaxFrame (32
// MiB) each, so the 64-slot channel alone could retain gigabytes; this budget
// is generous for normal small deltas while failing a hostile/slow run closed
// long before that. Internal safety guardrail, not user-tunable.
const cursorRunEventBytesMax = 64 << 20 // 64 MiB

// cursorRunPendingCap bounds CursorRun.pending. It applies only to new call
// ids; duplicate/stale ids keep their existing replace/skip behavior. Internal
// safety guardrail, not user-tunable.
const cursorRunPendingCap = 4096

// cursorRunPendingBytesMax bounds the aggregate CallID/Name/Args/exec-id bytes
// retained in CursorRun.pending. Normal parallel tool calls carry small JSON
// arguments (typically well under a few hundred KiB), so 64 MiB leaves ample
// headroom while preventing the count cap's worst case (4096 maximal 32 MiB
// frames) from retaining gigabytes. Internal safety guardrail, not
// user-tunable.
const cursorRunPendingBytesMax = 64 << 20 // 64 MiB

// CursorRunRetainedBytesMax returns the per-run cap on pending tool-call plus
// queued event payload bytes. The parked-run store reserves exactly this
// allowance outside its aggregate parked stream budget for the one run being
// actively driven, so the two bounds cannot drift apart.
func CursorRunRetainedBytesMax() int64 { return cursorRunPendingBytesMax + cursorRunEventBytesMax }

// cursorRunEventValueDepthMax bounds the recursive event-size walker. Event
// payloads built by this adapter are shallow; the bound keeps a hostile or
// future nested value from turning accounting into unbounded recursion.
const cursorRunEventValueDepthMax = 16

// cursorRunEventSizeLimit saturates the walker one byte past the admission
// bound: an event that can never fit needs no exact size, and saturation keeps
// measuring a hostile payload's work bounded.
const cursorRunEventSizeLimit = cursorRunEventBytesMax + 1

var (
	// errCursorRunEventBytes is the stable fail-closed reason for exceeding
	// the queued-event byte budget.
	errCursorRunEventBytes = errors.New("cursor: run event queue byte limit exceeded")
	// errCursorRunPendingCap is the stable fail-closed reason for exceeding
	// the pending tool-call count cap.
	errCursorRunPendingCap = errors.New("cursor: pending tool call limit exceeded")
	// errCursorRunPendingBytes is the stable fail-closed reason for exceeding
	// the pending tool-call payload budget.
	errCursorRunPendingBytes = errors.New("cursor: pending tool call byte limit exceeded")
)

// NewCursorRun wraps an established bidi stream. out is the request-body
// writer (kept open), in the response reader, blobs the KV store, closeFn
// tears down the HTTP exchange on Close, and heartbeat is the ClientHeartbeat
// cadence (idle parked runs included) - the caller supplies the configured
// value (config.CursorHeartbeatInterval), never a hardcoded default here.
// Call Start to begin the pump.
func NewCursorRun(out io.Writer, in io.Reader, blobs *KVBlobStore, closeFn func(), heartbeat time.Duration) *CursorRun {
	if blobs == nil {
		blobs = newKVBlobStore()
	}
	run := &CursorRun{
		sw:        &streamWriter{w: out},
		in:        newConnectReader(bufio.NewReader(in)),
		blobs:     blobs,
		closeFn:   closeFn,
		heartbeat: heartbeat,
		events:    make(chan runEvent, runEventChanBuf),
		pending:   map[string]CursorToolCall{},
		stopPump:  make(chan struct{}),
	}
	if pauser, ok := in.(interface {
		PauseIdle()
		ResumeIdle()
	}); ok {
		run.idlePauser = pauser
	}
	return run
}

// Start launches the pump + heartbeat.
func (r *CursorRun) Start() { go r.pump() }

// failClosedLocked records err as the run's terminal reason and tears it down
// through Close, so the pump exits and closes the FIFO event stream after any
// already-queued events. The caller holds r.mu; it is released before Close
// re-acquires it.
func (r *CursorRun) failClosedLocked(err error) {
	if r.closeErr == nil {
		r.closeErr = err
	}
	r.mu.Unlock()
	r.Close()
}

// pushEvent is the single pump→driver queue owner: it measures the event once,
// enforces the queued-byte budget before enqueueing, and accounts the bytes for
// releaseEvent. An over-budget event is never queued; the run fails closed and
// the driver reports errCursorRunEventBytes instead of an unexpected EOF.
func (r *CursorRun) pushEvent(ev runEvent) bool {
	ev.size = cursorRunEventSize(ev)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	if ev.size > cursorRunEventBytesMax-r.queuedBytes {
		r.failClosedLocked(fmt.Errorf("%w (event %d bytes, queued %d, limit %d)",
			errCursorRunEventBytes, ev.size, r.queuedBytes, cursorRunEventBytesMax))
		return false
	}
	if err := r.notifyRetentionLocked(r.pendingBytes + r.queuedBytes + ev.size); err != nil {
		r.failClosedLocked(err)
		return false
	}
	r.queuedBytes += ev.size
	r.mu.Unlock()

	select {
	case r.events <- ev:
		return true
	case <-r.stopPump:
		// The event was reserved but never queued (Close won the select).
		r.mu.Lock()
		r.queuedBytes -= ev.size
		if r.queuedBytes < 0 {
			r.queuedBytes = 0
		}
		_ = r.notifyRetentionLocked(r.pendingBytes + r.queuedBytes)
		r.mu.Unlock()
		return false
	}
}

// releaseEvent subtracts one event's accounted bytes exactly once when the
// driver receives it. Both receive branches in runTurn (the normal select and
// the settle/idle drain) converge here. The clamp is defensive: a direct
// test-injected event has size 0, and queuedBytes must never go negative.
func (r *CursorRun) releaseEvent(ev runEvent) {
	if ev.size == 0 {
		return
	}
	r.mu.Lock()
	r.queuedBytes -= ev.size
	if r.queuedBytes < 0 {
		r.queuedBytes = 0
	}
	_ = r.notifyRetentionLocked(r.pendingBytes + r.queuedBytes)
	r.mu.Unlock()
}

// addPending registers a tool call awaiting a result. Both the count cap and
// the aggregate payload budget apply to new ids; a duplicate replaces exactly
// as before but is re-measured so the budget never retains stale bytes.
// Exceeding either bound fails the whole run closed instead of retaining an
// unbounded map.
func (r *CursorRun) addPending(call CursorToolCall) bool {
	size := cursorRunToolCallSize(call)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	old, exists := r.pending[call.CallID]
	if !exists && len(r.pending) >= cursorRunPendingCap {
		r.failClosedLocked(fmt.Errorf("%w (limit %d)", errCursorRunPendingCap, cursorRunPendingCap))
		return false
	}
	base := r.pendingBytes
	if base < 0 {
		base = 0
	}
	if exists {
		base -= cursorRunToolCallSize(old)
		if base < 0 {
			base = 0
		}
	}
	if size > cursorRunPendingBytesMax-base {
		r.failClosedLocked(fmt.Errorf("%w (call %d bytes, pending %d, limit %d)",
			errCursorRunPendingBytes, size, base, cursorRunPendingBytesMax))
		return false
	}
	if err := r.notifyRetentionLocked(base + size + r.queuedBytes); err != nil {
		r.failClosedLocked(err)
		return false
	}
	r.pending[call.CallID] = call
	r.pendingBytes = base + size
	r.mu.Unlock()
	return true
}

// retainedBytesLocked returns the pending tool-call plus queued-event byte
// total the aggregate parked budget accounts. Callers hold r.mu.
func (r *CursorRun) retainedBytesLocked() int64 {
	total := r.pendingBytes
	if total < 0 {
		total = 0
	}
	total += r.queuedBytes
	if total < 0 {
		total = 0
	}
	return total
}

// notifyRetentionLocked reports a candidate pending+queued total to the
// parked-run store's aggregate observer. It is called before a growth is
// committed, so a denial leaves both per-run accounting and retained payload
// unchanged; shrinks are always accepted. Callers hold r.mu.
func (r *CursorRun) notifyRetentionLocked(total int64) error {
	if r.retention == nil {
		return nil
	}
	return r.retention(total)
}

// SetRetentionLimit installs the parked-run store's aggregate observers: one
// for this run's pending+queued bytes and one for its KV blob store. Both are
// invoked immediately with the current totals, so a run that already exceeds
// the aggregate bound is refused before it is indexed as parked. A later
// rejected growth fails the run closed. The store must call
// ClearRetentionLimits when it releases the run.
func (r *CursorRun) SetRetentionLimit(stream func(total int64) error, blobs func(bytes int64, entries int) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("cursor: run closed before retention limits were installed")
	}
	if r.retention != nil {
		return errors.New("cursor: run retention limits already installed")
	}
	if stream == nil && blobs == nil {
		return errors.New("cursor: retention limits require an observer")
	}
	if stream != nil {
		if err := stream(r.retainedBytesLocked()); err != nil {
			return err
		}
	}
	if blobs != nil {
		if err := r.blobs.SetRetentionLimit(blobs); err != nil {
			if stream != nil {
				_ = stream(0)
			}
			return err
		}
	}
	r.retention = stream
	return nil
}

// ClearRetentionLimits releases both aggregate observers and reports zero to
// each, so the parked-run store's counters drop the run's bytes exactly once.
// It is idempotent.
func (r *CursorRun) ClearRetentionLimits() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retention != nil {
		_ = r.retention(0)
		r.retention = nil
	}
	r.blobs.ClearRetentionLimit()
}

// PendingBytes returns the exact bytes retained by pending tool calls. The
// parked-run store uses it to enforce its aggregate stream budget; the value is
// O(1) and never scans the pending map.
func (r *CursorRun) PendingBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingBytes < 0 {
		return 0
	}
	return r.pendingBytes
}

// QueuedBytes returns the exact payload bytes retained by queued or in-flight
// events. The parked-run store uses it to enforce its aggregate stream budget;
// the value is O(1) and never scans the event channel.
func (r *CursorRun) QueuedBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.queuedBytes < 0 {
		return 0
	}
	return r.queuedBytes
}

// cursorRunToolCallSize is the single retained-size owner for a pending tool
// call. It reuses the event walker's saturating string accounting and counts
// every payload field the pending map retains.
func cursorRunToolCallSize(call CursorToolCall) int64 {
	n := cursorRunAddSize(0, int64(len(call.CallID)))
	n = cursorRunAddSize(n, int64(len(call.Name)))
	n = cursorRunAddSize(n, int64(len(call.Args)))
	n = cursorRunAddSize(n, int64(len(call.execIDStr)))
	return n
}

// takePendingLocked removes one pending call and releases its accounted bytes.
// It is the single pending-deletion owner; callers hold r.mu.
func (r *CursorRun) takePendingLocked(id string) (CursorToolCall, bool) {
	call, ok := r.pending[id]
	if !ok {
		return CursorToolCall{}, false
	}
	delete(r.pending, id)
	r.pendingBytes -= cursorRunToolCallSize(call)
	if r.pendingBytes < 0 {
		r.pendingBytes = 0
	}
	_ = r.notifyRetentionLocked(r.pendingBytes + r.queuedBytes)
	return call, true
}

// cursorRunEventSize estimates the bytes an event retains. It walks the actual
// payload fields instead of serializing a JSON copy: delta values plus the tool
// call's retained fields via cursorRunToolCallSize. Errors count their message,
// which is all a terminal event retains. The walk saturates at
// cursorRunEventSizeLimit.
func cursorRunEventSize(ev runEvent) int64 {
	n := cursorRunValueSize(ev.delta, 0)
	if ev.toolCall != nil {
		n = cursorRunAddSize(n, cursorRunToolCallSize(*ev.toolCall))
	}
	if ev.err != nil {
		n = cursorRunAddSize(n, int64(len(ev.err.Error())))
	}
	return n
}

// cursorRunValueSize is the bounded recursive walker behind cursorRunEventSize.
// Unknown value types count as zero: this adapter only constructs strings,
// scalars, map[string]any, []any and []map[string]any event payloads, and a
// reflection-based fallback would add cost for shapes that cannot occur.
func cursorRunValueSize(v any, depth int) int64 {
	if depth > cursorRunEventValueDepthMax {
		return 0
	}
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return cursorRunAddSize(0, int64(len(x)))
	case []byte:
		return cursorRunAddSize(0, int64(len(x)))
	case bool:
		return 1
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return 8
	case map[string]any:
		var n int64
		for k, vv := range x {
			n = cursorRunAddSize(n, int64(len(k)))
			n = cursorRunAddSize(n, cursorRunValueSize(vv, depth+1))
			if n >= cursorRunEventSizeLimit {
				return cursorRunEventSizeLimit
			}
		}
		return n
	case []any:
		var n int64
		for _, vv := range x {
			n = cursorRunAddSize(n, cursorRunValueSize(vv, depth+1))
			if n >= cursorRunEventSizeLimit {
				return cursorRunEventSizeLimit
			}
		}
		return n
	case []map[string]any:
		var n int64
		for _, vv := range x {
			n = cursorRunAddSize(n, cursorRunValueSize(vv, depth+1))
			if n >= cursorRunEventSizeLimit {
				return cursorRunEventSizeLimit
			}
		}
		return n
	default:
		return 0
	}
}

// cursorRunAddSize adds a nonnegative payload length, saturating at the walker
// limit so hostile sizes cannot overflow the int64 estimate.
func cursorRunAddSize(n, add int64) int64 {
	if add <= 0 {
		return n
	}
	if n >= cursorRunEventSizeLimit-add {
		return cursorRunEventSizeLimit
	}
	return n + add
}

// pendingHas reports whether the call id is still awaiting a result (the pump
// registers every exec before surfacing it; a buffered event whose pending
// entry was already consumed is stale and must be skipped - surfacing it
// again would re-park the run and could Close it mid-continuation).
func (r *CursorRun) pendingHas(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pending[id]
	return ok
}

// RunTurn drives one turn, forwarding SSE deltas to emitDelta (already-framed
// OpenAI chunk objects; the proxy marshals + writes them). It blocks until the
// turn parks (a tool call), finishes, or errors. A fresh run starts at zero;
// ResumeTurn resets usage before sending results that release the next turn.
func (r *CursorRun) RunTurn(ctx context.Context, emitDelta func(delta map[string]any) error) TurnResult {
	return r.activeTurn(ctx, func() TurnResult { return r.runTurn(emitDelta) })
}

// Bind cancellation only while a request drives a turn, including blocked
// result writes. Stop and join before returning ownership to the parked store.
func (r *CursorRun) activeTurn(ctx context.Context, drive func() TurnResult) TurnResult {
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { r.Close(); close(closed) })
	result := drive()
	if !stop() {
		<-closed
		result.Outcome, result.Err, result.ClientAbort = TurnErrored, ctx.Err(), true
	}
	return result
}

func (r *CursorRun) runTurn(emitDelta func(delta map[string]any) error) TurnResult {
	var calls []CursorToolCall
	seen := map[string]bool{}
	visible := false
	var settle *time.Timer
	var idle <-chan time.Time
	expired := false
	defer func() {
		if settle != nil {
			settle.Stop()
		}
	}()
	for {
		var ev runEvent
		var ok bool
		if expired {
			// A slow client write can outlast the settle window while sibling
			// events queue. Drain those before declaring the input idle.
			select {
			case ev, ok = <-r.events:
			default:
				return r.finish(TurnParked, calls, nil, visible)
			}
		} else {
			select {
			case ev, ok = <-r.events:
			case <-idle:
				expired = true
				continue
			}
		}
		if !ok {
			// Pump termination is observed only AFTER every queued event.
			// Ordinary EOF/errors have explicit terminal events; a close with
			// none is interrupted work, never a fabricated empty success. A
			// fail-closed guardrail recorded its descriptive reason first.
			r.mu.Lock()
			closeErr := r.closeErr
			r.mu.Unlock()
			if closeErr != nil {
				return r.finish(TurnErrored, calls, closeErr, visible)
			}
			return r.finish(TurnErrored, calls, io.ErrUnexpectedEOF, visible)
		}
		r.releaseEvent(ev)
		switch ev.kind {
		case evDelta:
			if cursorVisibleDelta(ev.delta) {
				visible = true
			}
			if emitDelta != nil {
				if err := emitDelta(ev.delta); err != nil {
					// Writing to the local client failed (it disconnected
					// mid-stream): a cancellation, not an upstream error.
					return TurnResult{Outcome: TurnErrored, Err: err, VisibleOutput: visible, ClientAbort: true}
				}
			}
		case evToolCall:
			if ev.toolCall == nil || !r.pendingHas(ev.toolCall.CallID) || seen[ev.toolCall.CallID] {
				continue // stale buffered event or duplicate of an answered call
			}
			seen[ev.toolCall.CallID] = true
			calls = append(calls, *ev.toolCall)
			visible = true
			if emitDelta != nil {
				if err := emitDelta(map[string]any{"tool_calls": []map[string]any{{
					"index": len(calls) - 1, "id": ev.toolCall.CallID, "type": "function",
					"function": map[string]any{"name": ev.toolCall.Name, "arguments": ev.toolCall.Args},
				}}}); err != nil {
					return TurnResult{Outcome: TurnErrored, Err: err, VisibleOutput: visible, ClientAbort: true}
				}
			}
			// Don't park on the first call: Cursor may issue MULTIPLE tool
			// calls in one turn (parallel calls). Wait a short window for
			// siblings (or a turn-end) before parking, so every call is
			// surfaced to the OpenAI client. A call missed here is a call
			// the client never sees - the model then loses part of its turn.
			if settle == nil {
				settle = time.NewTimer(cursorToolSettleIdleMs)
				idle = settle.C
			} else {
				settle.Reset(cursorToolSettleIdleMs)
			}
			expired = false
		case evDone:
			return r.finish(TurnFinished, calls, nil, visible)
		case evErr:
			return r.finish(TurnErrored, calls, ev.err, visible)
		}
	}
}

// cursorVisibleDelta reports whether an emitted delta carries client-visible
// output: non-empty answer or reasoning text, or at least one tool call. The
// void predicate uses the turn-wide OR of this at the emit boundary, never a
// token-usage count.
func cursorVisibleDelta(delta map[string]any) bool {
	if s, ok := delta["content"].(string); ok && s != "" {
		return true
	}
	if s, ok := delta["reasoning_content"].(string); ok && s != "" {
		return true
	}
	if calls, ok := delta["tool_calls"].([]map[string]any); ok && len(calls) > 0 {
		return true
	}
	return false
}

// finish assembles the TurnResult with the usage snapshot.
func (r *CursorRun) finish(outcome TurnOutcome, calls []CursorToolCall, err error, visible bool) TurnResult {
	if outcome == TurnParked && r.idlePauser != nil {
		// The pump is (or is about to be) blocked reading a stream that will
		// stay silent until the client returns a tool result. The parked-run
		// store's cursor_park_ttl owns that wait, not the generic idle bound.
		r.idlePauser.PauseIdle()
	}
	prompt, output, reasoning := r.usageSnapshot()
	if errors.Is(err, metrics.ErrMetricRange) {
		prompt, output, reasoning = 0, 0, 0
	}
	return TurnResult{Outcome: outcome, ToolCalls: calls, Prompt: prompt, Output: output, Reasoning: reasoning, VisibleOutput: visible, Err: err}
}

// ToolResult is one client-supplied answer for a parked tool call, keyed by
// tool_call_id in the ResumeTurn map.
type ToolResult struct {
	Text    string
	IsError bool
}

// ResumeTurn writes tool results into the parked stream and drives the
// continuation. results maps tool_call_id -> (result text, isError). It
// returns an error if any id isn't pending (the proxy cold-starts instead).
//
// steerText is a user message the client sent alongside the tool results (the
// user typed mid-turn). When non-empty it is written as a
// conversation_action.user_message_action AFTER the results - Cursor's
// "steer", so the running turn sees the message at its next step instead of
// the proxy silently dropping it. Without this the resume path only forwards
// the tool result and the user's typed text never reaches the model (the
// "message ignored mid-turn" bug).
func (r *CursorRun) ResumeTurn(ctx context.Context, results map[string]ToolResult, steerText string, emitDelta func(delta map[string]any) error,
) TurnResult {
	return r.activeTurn(ctx, func() TurnResult { return r.resumeTurn(results, steerText, emitDelta) })
}

func (r *CursorRun) resumeTurn(results map[string]ToolResult, steerText string, emitDelta func(delta map[string]any) error) TurnResult {
	r.mu.Lock()
	if r.closed {
		closeErr := r.closeErr
		r.mu.Unlock()
		if closeErr != nil {
			return TurnResult{Outcome: TurnErrored, Err: closeErr}
		}
		return TurnResult{Outcome: TurnErrored, Err: fmt.Errorf("cursor: parked stream closed before results arrived")}
	}
	if r.idlePauser != nil {
		// The continuation is expected to make progress again: restore the
		// generic idle bound before any result write can unblock the pump.
		r.idlePauser.ResumeIdle()
	}
	for id := range results {
		if _, ok := r.pending[id]; !ok {
			r.mu.Unlock()
			return TurnResult{Outcome: TurnErrored, Err: fmt.Errorf("cursor: no pending tool call %q", id)}
		}
	}
	// Before any result write can unblock a fast upstream continuation.
	// The fresh-run path needs no reset and must not erase pumped usage.
	r.usagePrompt, r.usageDeltaSum = 0, 0
	r.turnEnded = false
	r.mu.Unlock()

	// Write each result into the stream, releasing the blocked turn(s).
	for id, res := range results {
		r.mu.Lock()
		call, ok := r.takePendingLocked(id)
		r.mu.Unlock()
		if !ok {
			return TurnResult{Outcome: TurnErrored, Err: fmt.Errorf("cursor: tool call %q vanished", id)}
		}
		if err := r.sw.writeFramed(encodeMcpResult(call.execID, call.execIDStr, res.Text, res.IsError)); err != nil {
			return TurnResult{Outcome: TurnErrored, Err: fmt.Errorf("cursor: write mcp_result: %w", err)}
		}
	}
	// Steer the user's mid-turn message into the running turn (after the
	// results, so the model settles the tool calls first, then sees the text).
	if steerText != "" {
		if err := r.sw.writeFramed(encodeSteerUserMessage(steerText)); err != nil {
			return TurnResult{Outcome: TurnErrored, Err: fmt.Errorf("cursor: write steer user message: %w", err)}
		}
	}
	// Drive the continuation (model answers with the tool results).
	return r.runTurn(emitDelta)
}

// BlobBytes returns the exact retained bytes of this run's KV blob store. The
// parked-run store uses it to enforce its aggregate blob budget without
// scanning or copying blob values; NewCursorRun always supplies a store.
func (r *CursorRun) BlobBytes() int64 {
	return r.blobs.RetainedBytes()
}

// BlobEntries returns the number of retained blobs in this run's KV store. The
// parked-run store uses it to enforce its aggregate entry budget without
// scanning the map.
func (r *CursorRun) BlobEntries() int {
	return r.blobs.RetainedEntries()
}

// PendingToolCallIDs returns the set of tool_call_ids awaiting results.
func (r *CursorRun) PendingToolCallIDs() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(r.pending))
	for id := range r.pending {
		out[id] = true
	}
	return out
}

// Closed reports whether the run has been torn down.
func (r *CursorRun) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// usageSnapshot reads the accumulated usage counters. prompt is the current
// context occupancy (0 if no checkpoint arrived - the caller falls back to the
// request-payload estimate); output is the per-turn generated total; reasoning
// is always 0 (Cursor's wire reports a single combined output counter, with no
// answer/reasoning split - see the constant block in cursor.go).
func (r *CursorRun) usageSnapshot() (prompt, output, reasoning int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usagePrompt, r.usageDeltaSum, 0
}

// Close tears down the run: stops the pump and closes the underlying stream.
// Pump exit shares this owner, so EOF also retires the parked heartbeat and
// transport immediately. The closed guard makes teardown exactly once.
func (r *CursorRun) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	close(r.stopPump)
	if r.closeFn != nil {
		r.closeFn()
	}
}

// pump is the background read loop. It reads server frames, answers callbacks
// (KV pulls, request-context handshake), and pushes events onto r.events for
// the current turn driver. It runs until the stream ends or Close. ANY pump
// exit closes the run before closing its FIFO event stream: a parked run whose
// upstream stream died would otherwise stay "resumable" in the proxy's store
// and the next resume writes into the transport-closed pipe ("io: read/write
// on closed pipe"). The store's sweep then removes the dead entry and the
// client cold-starts instead.
func (r *CursorRun) pump() {
	defer close(r.events)
	defer r.Close()

	go func() { // heartbeat keepalive, including while parked
		t := time.NewTicker(r.heartbeat)
		defer t.Stop()
		hb := encodeClientHeartbeat()
		for {
			select {
			case <-r.stopPump:
				return
			case <-t.C:
				_ = r.sw.writeFramed(hb)
			}
		}
	}()

	push := r.pushEvent

	for {
		select {
		case <-r.stopPump:
			return
		default:
		}

		env, err := r.in.next()
		if err == io.EOF {
			r.mu.Lock()
			ended := r.turnEnded
			r.mu.Unlock()
			if ended {
				push(runEvent{kind: evDone})
			} else {
				push(runEvent{kind: evErr, err: io.ErrUnexpectedEOF})
			}
			return
		}
		if err != nil {
			push(runEvent{kind: evErr, err: err})
			return
		}

		if env.endStream {
			push(runEvent{kind: evDone})
			return
		}

		asm, err := parseProtoFields(env.payload)
		if err != nil {
			push(runEvent{kind: evErr, err: fmt.Errorf("cursor: decode AgentServerMessage: %w", err)})
			return
		}

		if kv := firstField(asm, fAgentServerMessageKvServerMessage); kv != nil {
			if err := handleKvServerMessage(r.sw, kv, r.blobs); err != nil {
				push(runEvent{kind: evErr, err: err})
				return
			}
			continue
		}
		if cp := firstField(asm, fAgentServerMessageConversationCheckpointUpdate); cp != nil {
			if err := r.handleCheckpointUpdate(cp); err != nil {
				push(runEvent{kind: evErr, err: fmt.Errorf("cursor: decode ConversationCheckpointUpdate: %w", err)})
				return
			}
			continue
		}
		if ex := firstField(asm, fAgentServerMessageExecServerMessage); ex != nil {
			var call CursorToolCall
			if err := handleExecServerMessage(r.sw, ex, &call); err != nil {
				push(runEvent{kind: evErr, err: err})
				return
			}
			if call.Name != "" {
				tc := CursorToolCall{CallID: call.CallID, Name: call.Name, Args: call.Args, execID: call.execID, execIDStr: call.execIDStr}
				if !r.addPending(tc) {
					return
				}
				if !push(runEvent{kind: evToolCall, toolCall: &tc}) {
					return
				}
			}
			continue
		}

		if iu := firstField(asm, fAgentServerMessageInteractionUpdate); iu != nil {
			if err := r.handleInteractionUpdate(iu, push); err != nil {
				push(runEvent{kind: evErr, err: fmt.Errorf("cursor: decode InteractionUpdate: %w", err)})
				return
			}
			continue
		}
	}
}

// pushInteractionDelta decodes one text-family InteractionUpdate arm (text or
// thinking): when the arm's field is present it decodes the inner text and
// pushes the matching SSE delta key. ok reports whether the arm's field was
// present - a present arm terminates update handling (its decode error, if
// any, is returned), an absent one falls through to the next arm.
func pushInteractionDelta(upd []protoField, deltaField, textField int, key string, push func(runEvent) bool) (bool, error) {
	f := firstField(upd, deltaField)
	if f == nil {
		return false, nil
	}
	td, err := subFields(f)
	if err != nil {
		return true, err
	}
	if t := firstField(td, textField); t != nil && len(t.raw) > 0 {
		push(runEvent{kind: evDelta, delta: map[string]any{key: string(t.raw)}})
	}
	return true, nil
}

// handleInteractionUpdate decodes one InteractionUpdate: accumulates usage and
// pushes text/thinking deltas as events.
func (r *CursorRun) handleInteractionUpdate(iu *protoField, push func(runEvent) bool) error {
	upd, err := subFields(iu)
	if err != nil {
		return err
	}
	if ok, err := pushInteractionDelta(upd, fInteractionUpdateTextDelta, fTextDeltaText, "content", push); ok {
		return err
	}
	if ok, err := pushInteractionDelta(upd, fInteractionUpdateThinkingDelta, fThinkingDeltaText, "reasoning_content", push); ok {
		return err
	}
	if f := firstField(upd, fInteractionUpdateTokenDelta); f != nil {
		td, err := subFields(f)
		if err != nil {
			return err
		}
		if t := firstField(td, fTokenDeltaTokens); t != nil {
			count, err := protoTokenCount(t.n)
			if err != nil {
				return err
			}
			r.mu.Lock()
			sum, err := metrics.SumCounts(r.usageDeltaSum, count)
			if err == nil {
				r.usageDeltaSum = sum
			}
			r.mu.Unlock()
			if err != nil {
				return err
			}
		}
		return nil
	}
	if f := firstField(upd, fInteractionUpdateTurnEnded); f != nil {
		if _, err := subFields(f); err != nil {
			return err
		}
		r.mu.Lock()
		r.turnEnded = true
		r.mu.Unlock()
	}
	return nil
}

// handleCheckpointUpdate decodes a ConversationStateStructure checkpoint and
// records the conversation's CURRENT context-window occupancy (token_details
// .used_tokens) as the authoritative per-turn input. This is the only accurate
// prompt-size signal on the wire: the InteractionUpdate channel carries no
// prompt/completion split, so input would otherwise have to be estimated.
func (r *CursorRun) handleCheckpointUpdate(cp *protoField) error {
	cs, err := subFields(cp)
	if err != nil {
		return err
	}
	td := firstField(cs, fConversationStateTokenDetails)
	if td == nil {
		return nil
	}
	details, err := subFields(td)
	if err != nil {
		return err
	}
	if u := firstField(details, fTokenDetailsUsedTokens); u != nil && u.n > 0 {
		count, err := protoTokenCount(u.n)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.usagePrompt = count
		r.mu.Unlock()
	}
	return nil
}

// Wire varints are uint64, but recorded token counts are nonnegative int64.
// Validate before narrowing at the shared proto count boundary.
func protoTokenCount(n uint64) (int64, error) {
	if n > math.MaxInt64 {
		return 0, metrics.ErrMetricRange
	}
	return int64(n), nil
}
