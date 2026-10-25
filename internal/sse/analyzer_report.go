package sse

import (
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TailTerminates reports whether one unterminated trailing line (the bytes
// after the last newline, without their newline) carries a completion terminal
// marker for the chatlike wires: a Chat [DONE], a non-null Chat finish_reason,
// or a Responses terminal event. It reads the same frame grammar as Feed and
// never mutates analyzer state; the relay uses it to decide whether a
// truncation-class tail is dropped or fed. Standalone in-band error terminals
// and foreign-wire terminals such as Anthropic message_stop are deliberately
// excluded: an unterminated in-band error line is dropped rather than
// delivered, so it is not a delivery terminal. A fed tail is delivered only
// when it supplies a complete answer or tool call; an empty or reasoning-only
// completion resolves through the degenerate boundary, which withholds or
// replaces the terminal bytes.
func (a *Analyzer) TailTerminates(line []byte) bool {
	payload, ok := DataPayload(line)
	if !ok {
		return false
	}
	if isDonePayload(payload) {
		return a.DoneTerminates()
	}
	var keys frameKeys
	keys.scan(payload)
	if jsonValueString(keys.finishReason) != "" {
		return true
	}
	return responsesTerminalEvent(keys.responseEventType(payload))
}

// OutcomeCode classifies the finished stream via the canonical degenerate
// rules (metrics.ClassifyOutcome - the same predicate every relay path uses).
// Answer presence and reasoning presence are separate inputs: reasoning is
// auxiliary output, so a reasoning-only stop is its own degenerate class
// (metrics.CodeReasoningOnly), never a healthy completion and never the
// nothing-at-all void. Answer-shaped output includes content (chat
// delta.content, legacy choices[].text) and the visible non-content forms the
// rescue gates already protect - a non-empty refusal or deprecated
// function_call declaration; empty values of either are not answer output.
// "" = healthy; otherwise the error code to surface (or, on the passthrough
// path, the code whose in-band error chunk replaces the held terminal event).
func (a *Analyzer) OutcomeCode() string {
	if a.anthropicWire {
		if !a.terminatorSeen {
			return metrics.CodeTruncated
		}
		return ""
	}
	hadAnswer := !a.firstAnswerAt.IsZero() || a.answerishSeen
	hadReasoning := !a.firstReasoningAt.IsZero() || a.terminalReasoningSeen
	return metrics.ClassifyOutcome(a.finishReason, a.terminatorSeen,
		hadAnswer, hadReasoning, a.responsesOutputSeen, a.toolCalls, a.chatlike)
}

// ResponsesToolCallItem reports whether one Responses output item is a model
// tool invocation. These items are tool activity even when they carry no
// function-call arguments, and they must never be classified as an empty
// completion. Provider-executed output items (web search, file search, image
// generation, code interpreter) are output presence instead. The streaming
// analyzer and the non-stream Responses decoder share this one vocabulary.
func ResponsesToolCallItem(typ string) bool {
	switch typ {
	case "function_call", "custom_tool_call", "local_shell_call", "mcp_call",
		"shell_call", "apply_patch_call", "computer_call", "tool_search_call":
		return true
	}
	return false
}

// RetryableTruncation reports whether the finished stream is a truncation
// (metrics.CodeTruncated) the proxy may transparently re-send: a chatlike
// stream that hit clean EOF without any terminal marker while the client had
// received NO content-bearing chunk (no content, reasoning, or tool call)
// and no answer-shaped bytes (a refusal or deprecated function_call delta is
// visible output even though it is not metrics content) and no provider
// in-band error. The trailing event is flushed first so a final multi-line
// error frame that never saw its blank-line boundary is still caught.
// Anything the client already saw - generation content or the provider's own
// error - makes a re-send duplicate visible bytes and stays client-side.
// Client-disconnect gating is the caller's (it owns the record). When this
// reports true the terminal region was never opened (holding requires a seen
// terminator), so no withheld bytes are pending.
func (a *Analyzer) RetryableTruncation() bool {
	// answerishSeen (a refusal or deprecated function_call delta) is
	// answer-shaped output the client saw: no rescue may append behind it,
	// even though it is not metrics content. Responses feeds are excluded once
	// any event was seen: their per-response sequence numbers are documented
	// ordering state, and a re-send would restart them mid-stream (the same
	// wire-safety gate RetryableReasoningTruncation applies).
	if !a.chatlike || a.terminatorSeen || a.answerishSeen || a.responsesStream {
		return false
	}
	a.flushEvent(time.Now())
	return a.firstTokenAt.IsZero() && a.errType == ""
}

// RetryableReasoningTruncation reports whether the finished stream is a
// mid-thinking death the proxy may transparently re-send even though the
// client already saw reasoning bytes: a chat-completions-shaped stream that
// hit clean EOF or a read error without any terminal marker, carrying
// reasoning but NO answer-shaped output (no content delta, no tool call, no
// refusal or deprecated function_call) and no provider in-band error. The
// completion contract is answer content and tool calls; reasoning is
// auxiliary display text, so the fresh attempt's frames append cleanly to the
// committed SSE connection (chat chunks have no ordering fields and the
// official SDKs accumulate delta frames independently). Responses-API feeds
// are excluded: their per-response sequence numbers are documented ordering
// state, and a re-send would restart them mid-stream. finish_reason length
// never reaches here (it is a terminator, not a truncation). Client-disconnect
// gating is the caller's (it owns the record).
func (a *Analyzer) RetryableReasoningTruncation() bool {
	if !a.chatlike || a.terminatorSeen || a.responsesStream ||
		a.answerishSeen || a.toolCalls > 0 {
		return false
	}
	// Flush the trailing event BEFORE the in-band-error gate: a multi-line
	// provider error that never saw its blank-line boundary is discovered
	// only by the flush, and the provider's own failure statement must
	// disqualify the rescue (mirrors RetryableTruncation's ordering).
	a.flushEvent(time.Now())
	return !a.firstReasoningAt.IsZero() && a.firstAnswerAt.IsZero() && a.errType == ""
}

// RetryableReasoningStop reports whether a classified reasoning-only clean
// stop (the caller has already seen metrics.CodeReasoningOnly) may be
// transparently re-sent while its terminal region is still withheld. This is
// the stop twin of RetryableReasoningTruncation's wire-safety gates: a
// refusal or deprecated function_call delta is answer-shaped output the
// client saw, and a Responses-API feed carries per-response sequence numbers
// a re-send would restart mid-stream - neither may ever be appended behind.
// The still-withheld check (the relay's hold state) is the caller's.
func (a *Analyzer) RetryableReasoningStop() bool {
	return a.chatlike && a.terminatorSeen && a.errType == "" && !a.responsesStream &&
		!a.answerishSeen && a.toolCalls == 0
}

// RetryableUpstreamError reports whether the provider's in-band error was just
// discovered in a wire state where the relay may still transparently re-send
// the SAME request: the error registered on a data line that was the first
// data line of its SSE event (the single-line frame shape every
// OpenAI-compatible provider uses - the boundary-flush discovery of a
// multi-line error never qualifies, its earlier lines may already be
// relayed), the stream's terminal region never opened, and no answer-shaped
// output, content delta or tool call was relayed. A Responses-API feed is
// excluded once any of its events was seen: per-response sequence numbers
// are documented ordering state, and a re-send would restart them
// mid-stream (an error as the feed's first-ever frame is still rescuable -
// nothing was relayed). With no reasoning on the wire either, nothing
// distinguishes the delivered prefix for any remaining SSE protocol - the
// fresh attempt opens a clean new event stream, so no chat-shape gate
// applies (an error-only stream is exactly the transient-failure shape: the
// provider failing at first content). With reasoning frames already relayed
// the fresh attempt appends behind them, which is only clean behind
// chat-shaped deltas, so that variant keeps RetryableReasoningTruncation's
// shape gates and reports reasoning=true (the thinking budget owns the
// rescue, like a reasoning truncation). The report is one-shot: the first
// call consumes the mark, so a declined rescue (no budget, non-retryable
// class, client gone) can never re-fire on a later line. Error-class
// policy, budget and client-state gating stay with the caller, exactly like
// RetryableTruncation's.
func (a *Analyzer) RetryableUpstreamError() (typ, code, msg string, reasoning bool) {
	defer func() { a.errRescuableFrame = false }()
	if !a.errRescuableFrame || a.terminatorSeen || a.responsesStream ||
		a.answerishSeen || a.toolCalls > 0 || !a.firstAnswerAt.IsZero() {
		return "", "", "", false
	}
	if !a.firstReasoningAt.IsZero() {
		if !a.chatlike {
			return "", "", "", false
		}
		return a.errType, a.errCode, a.errMsg, true
	}
	return a.errType, a.errCode, a.errMsg, false
}
