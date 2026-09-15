package metrics

import (
	"bytes"
	"encoding/json"
)

// Degenerate-outcome detection: a "successful" upstream response that carried
// nothing usable. These are the canonical error codes surfaced in-band on
// streams and as HTTP 502s on non-streaming bodies. The strings deliberately
// avoid the verbs that downgrade retryability in common clients (Vercel AI
// SDK's getStatusCode maps codes containing "invalid"/"bad_request"/
// "context_length"/"authentication"/"permission"/"not_found"/"overload"/
// "timeout" to non-retryable 4xx classes); an unrecognized code maps to a
// retryable 500 instead - which is exactly how a degenerate completion must
// behave (the client retries it, the user retries it, nothing is masked).
const (
	// CodeEmptyCompletion: the stream terminated cleanly (terminator seen),
	// but produced no answer content, no reasoning, and no tool calls, with a
	// stop (or absent) finish_reason.
	CodeEmptyCompletion = "empty_completion"
	// CodeEmptyToolCall: finish_reason tool_calls but zero tool-call deltas
	// and no answer content - the model "declared" a tool and never called it.
	CodeEmptyToolCall = "empty_tool_call"
	// CodeTruncated: the stream ended (clean read EOF) without its documented
	// terminator - no [DONE], no finish_reason chunk, no terminal Responses
	// API event.
	CodeTruncated = "truncated"
	// CodeReasoningOnly: the stream terminated (clean stop or an
	// interruption finish) after reasoning-only output - the model thought,
	// the client saw reasoning deltas, but no answer content, no tool call
	// and no refusal ever arrived. The proxy may re-send the request on its
	// thinking budget (the already-relayed reasoning is auxiliary display
	// text, never completion contract); past the budget the class surfaces
	// in-band as an upstream_error so the client can retry.
	CodeReasoningOnly = "reasoning_only"
)

// DegenerateMessage is the single canonical user-facing message per code.
func DegenerateMessage(code string) string {
	switch code {
	case CodeEmptyCompletion:
		return "completion was empty: no content, reasoning, or tool calls were generated"
	case CodeEmptyToolCall:
		return "completion finished with tool_calls but no tool call was generated"
	case CodeTruncated:
		return "stream ended without its completion marker - response is truncated"
	case CodeReasoningOnly:
		return "completion ended after reasoning without an answer"
	}
	return code
}

// ClassifyOutcome evaluates a finished upstream stream against the degenerate
// classes. finish is the last non-empty finish_reason seen; terminatorSeen
// says a documented end-of-stream marker was observed ([DONE], a non-null
// finish_reason chunk, or a Responses-API terminal event); hadAnswer says any
// answer content was streamed; hadReasoning says reasoning ("thinking")
// deltas were streamed (auxiliary output, never the completion contract);
// toolCalls counts distinct tool-call deltas; chatlike gates the whole rule on
// "this really was a chat-completions/Responses stream" so a foreign SSE
// protocol proxied through never gets a synthetic error injected. Returns ""
// for healthy streams, else the error code to surface. The single choke point
// every relay path (passthrough, translated, cursor-upstream) routes through.
func ClassifyOutcome(finish string, terminatorSeen bool, hadAnswer, hadReasoning bool, toolCalls int, chatlike bool) string {
	if !chatlike {
		return ""
	}
	// A clean EOF without any terminator is a truncation: the client saw a
	// stream that simply stopped. (A non-EOF read error is the transport's
	// own failure and is classified by the relay, not here.)
	if !terminatorSeen {
		return CodeTruncated
	}
	// Answer content or a real tool call is contract output: whatever the
	// finish reason says, the completion delivered something usable.
	if hadAnswer || toolCalls > 0 {
		return ""
	}
	switch finish {
	case "tool_calls":
		// The model declared a tool and never called it. Reasoning already
		// relayed does not rescue the lie: the tool-call defect owns the
		// classification.
		return CodeEmptyToolCall
	case "stop", "":
		// A clean stop (or a provider that omits finish_reason entirely,
		// grok-style) after reasoning-only output: the model thought,
		// never answered, and declared the stream done.
		if hadReasoning {
			return CodeReasoningOnly
		}
		return CodeEmptyCompletion
	case "insufficient_system_resource", "aborted":
		// DeepSeek's documented mid-generation interruption finishes: the
		// request was cut short by the inference system, not decided by
		// the model. With reasoning relayed and nothing usable produced
		// they are the same rescuable void as a stop; with no reasoning at
		// all the provider's own signed finish stays healthy (never masked).
		if hadReasoning {
			return CodeReasoningOnly
		}
	}
	// length / content_filter / sensitive / network_error / error / unknown
	// values are legitimate or provider-signaled states - never a void.
	// length in particular is the client's own token cap: retrying cannot
	// clear it and it must never be rescued.
	return ""
}

// ClassifyNonStreamBody evaluates a fully-buffered non-streaming OpenAI-shaped
// JSON body (chat completions only) with the same degenerate rules as
// ClassifyOutcome: answer presence comes from choices[0].message.content,
// reasoning presence from the message's chat-completions reasoning field
// family (reasoning_content, reasoning, reasoning_details, reasoning_text -
// the spellings that appear on a chat message; the streaming analyzer's
// additional Anthropic/Gemini shapes never occur in a buffered chat-completions
// body), finish from choices[0].finish_reason, tools from its tool_calls
// array. This is the non-streaming twin of the streaming semantics: a
// reasoning-only body classifies reasoning_only, never empty_completion, so
// the two surfaces can never disagree about the same outcome. A non-streaming
// body is never "truncated" (it is complete by transport definition) - only
// the void, tool-mismatch and reasoning-only classes apply.
func ClassifyNonStreamBody(body []byte) string {
	var raw struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content       json.RawMessage `json:"content"`
				Reasoning     json.RawMessage `json:"reasoning_content"`
				ReasoningBare json.RawMessage `json:"reasoning"`
				ReasoningDet  json.RawMessage `json:"reasoning_details"`
				ReasoningText json.RawMessage `json:"reasoning_text"`
				ToolCalls     []struct{}      `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	if len(raw.Choices) == 0 {
		// Responses-API / usage-only / unknown shapes: not chat-completions -
		// nothing to classify.
		return ""
	}
	ch := raw.Choices[0]
	hadAnswer := contentPresent(ch.Message.Content)
	hadReasoning := contentPresent(ch.Message.Reasoning) ||
		contentPresent(ch.Message.ReasoningBare) ||
		contentPresent(ch.Message.ReasoningDet) ||
		contentPresent(ch.Message.ReasoningText)
	return ClassifyOutcome(ch.FinishReason, true, hadAnswer, hadReasoning, len(ch.Message.ToolCalls), true)
}

// contentPresent reports whether the raw value is a present, non-empty
// payload: a non-empty string, a non-empty array of parts, or an object part
// (null and empty values are decoys, never presence). The single presence
// predicate for the buffered body's answer content and reasoning fields.
func contentPresent(content json.RawMessage) bool {
	t := bytes.TrimLeft(content, " \t\r\n")
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '"':
		var s string
		if json.Unmarshal(t, &s) == nil {
			return s != ""
		}
	case '[':
		var parts []json.RawMessage
		if json.Unmarshal(t, &parts) == nil {
			return len(parts) > 0
		}
	case 'n':
		return false // "null"
	default:
		return true // an object part is content
	}
	return false
}
