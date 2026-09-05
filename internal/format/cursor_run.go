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
	Err       error // when Outcome == TurnErrored
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
	return &CursorRun{
		sw:        &streamWriter{w: out},
		in:        newConnectReader(bufio.NewReader(in)),
		blobs:     blobs,
		closeFn:   closeFn,
		heartbeat: heartbeat,
		events:    make(chan runEvent, runEventChanBuf),
		pending:   map[string]CursorToolCall{},
		stopPump:  make(chan struct{}),
	}
}

// Start launches the pump + heartbeat.
func (r *CursorRun) Start() { go r.pump() }

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
				return r.finish(TurnParked, calls, nil)
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
			// none is interrupted work, never a fabricated empty success.
			return r.finish(TurnErrored, calls, io.ErrUnexpectedEOF)
		}
		switch ev.kind {
		case evDelta:
			if emitDelta != nil {
				if err := emitDelta(ev.delta); err != nil {
					// Writing to the local client failed (it disconnected
					// mid-stream): a cancellation, not an upstream error.
					return TurnResult{Outcome: TurnErrored, Err: err, ClientAbort: true}
				}
			}
		case evToolCall:
			if ev.toolCall == nil || !r.pendingHas(ev.toolCall.CallID) || seen[ev.toolCall.CallID] {
				continue // stale buffered event or duplicate of an answered call
			}
			seen[ev.toolCall.CallID] = true
			calls = append(calls, *ev.toolCall)
			if emitDelta != nil {
				if err := emitDelta(map[string]any{"tool_calls": []map[string]any{{
					"index": len(calls) - 1, "id": ev.toolCall.CallID, "type": "function",
					"function": map[string]any{"name": ev.toolCall.Name, "arguments": ev.toolCall.Args},
				}}}); err != nil {
					return TurnResult{Outcome: TurnErrored, Err: err, ClientAbort: true}
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
			return r.finish(TurnFinished, calls, nil)
		case evErr:
			return r.finish(TurnErrored, calls, ev.err)
		}
	}
}

// finish assembles the TurnResult with the usage snapshot.
func (r *CursorRun) finish(outcome TurnOutcome, calls []CursorToolCall, err error) TurnResult {
	prompt, output, reasoning := r.usageSnapshot()
	if errors.Is(err, metrics.ErrMetricRange) {
		prompt, output, reasoning = 0, 0, 0
	}
	return TurnResult{Outcome: outcome, ToolCalls: calls, Prompt: prompt, Output: output, Reasoning: reasoning, Err: err}
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
func (r *CursorRun) ResumeTurn(ctx context.Context, results map[string]struct {
	Text    string
	IsError bool
}, steerText string, emitDelta func(delta map[string]any) error,
) TurnResult {
	return r.activeTurn(ctx, func() TurnResult { return r.resumeTurn(results, steerText, emitDelta) })
}

func (r *CursorRun) resumeTurn(results map[string]struct {
	Text    string
	IsError bool
}, steerText string, emitDelta func(delta map[string]any) error) TurnResult {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return TurnResult{Outcome: TurnErrored, Err: fmt.Errorf("cursor: parked stream closed before results arrived")}
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
		call, ok := r.pending[id]
		if ok {
			delete(r.pending, id)
		}
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

// Parked reports whether the run is waiting on tool results.
func (r *CursorRun) Parked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) > 0
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

	push := func(ev runEvent) bool {
		select {
		case r.events <- ev:
			return true
		case <-r.stopPump:
			return false
		}
	}

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
				r.mu.Lock()
				r.pending[call.CallID] = tc
				r.mu.Unlock()
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

// handleInteractionUpdate decodes one InteractionUpdate: accumulates usage and
// pushes text/thinking deltas as events.
func (r *CursorRun) handleInteractionUpdate(iu *protoField, push func(runEvent) bool) error {
	upd, err := subFields(iu)
	if err != nil {
		return err
	}
	if f := firstField(upd, fInteractionUpdateTextDelta); f != nil {
		td, err := subFields(f)
		if err != nil {
			return err
		}
		if t := firstField(td, fTextDeltaText); t != nil && len(t.raw) > 0 {
			push(runEvent{kind: evDelta, delta: map[string]any{"content": string(t.raw)}})
		}
		return nil
	}
	if f := firstField(upd, fInteractionUpdateThinkingDelta); f != nil {
		td, err := subFields(f)
		if err != nil {
			return err
		}
		if t := firstField(td, fThinkingDeltaText); t != nil && len(t.raw) > 0 {
			push(runEvent{kind: evDelta, delta: map[string]any{"reasoning_content": string(t.raw)}})
		}
		return nil
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
