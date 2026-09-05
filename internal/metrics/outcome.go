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
	}
	return code
}

// ClassifyOutcome evaluates a finished upstream stream against the degenerate
// classes. finish is the last non-empty finish_reason seen; terminatorSeen
// says a documented end-of-stream marker was observed ([DONE], a non-null
// finish_reason chunk, or a Responses-API terminal event); hadContent says any
// content-bearing chunk (answer, reasoning, or tool-call delta) was streamed;
// toolCalls counts distinct tool-call deltas; chatlike gates the whole rule on
// "this really was a chat-completions/Responses stream" so a foreign SSE
// protocol proxied through never gets a synthetic error injected. Returns ""
// for healthy streams, else the error code to surface. The single choke point
// every relay path (passthrough, translated, cursor-upstream) routes through.
func ClassifyOutcome(finish string, terminatorSeen bool, hadContent bool, toolCalls int, chatlike bool) string {
	if !chatlike {
		return ""
	}
	// A clean EOF without any terminator is a truncation: the client saw a
	// stream that simply stopped. (A non-EOF read error is the transport's
	// own failure and is classified by the relay, not here.)
	if !terminatorSeen {
		return CodeTruncated
	}
	if hadContent || toolCalls > 0 {
		return ""
	}
	switch finish {
	case "tool_calls":
		return CodeEmptyToolCall
	case "stop", "":
		return CodeEmptyCompletion
	}
	// length / content_filter / sensitive / network_error / error / unknown
	// values are legitimate or provider-signaled states - never a void.
	return ""
}

// ClassifyNonStreamBody evaluates a fully-buffered non-streaming OpenAI-shaped
// JSON body (chat completions only) with the same degenerate rules as
// ClassifyOutcome: content presence comes from choices[0].message.content,
// finish from choices[0].finish_reason, tools from its tool_calls array. A
// non-streaming body is never "truncated" (it is complete by transport
// definition) - only the void / tool-mismatch classes apply.
func ClassifyNonStreamBody(body []byte) string {
	var raw struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct{}      `json:"tool_calls"`
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
	hadContent := contentPresent(ch.Message.Content)
	return ClassifyOutcome(ch.FinishReason, true, hadContent, len(ch.Message.ToolCalls), true)
}

// contentPresent reports whether the raw content value is a non-empty string
// or a non-empty array of parts (either shape counts as answer presence).
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
