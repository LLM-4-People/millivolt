package sse

// OpenAI-compatible wire envelopes shared by every path that synthesizes a
// chat completion - streamed chunk or final document - for a client: the
// Anthropic bridge (internal/format) and the Cursor bridge
// (internal/proxy). The constructors own the id/object/created/model/
// choices skeleton so the two bridges cannot drift in envelope shape.
// created is a PARAMETER: the Anthropic bridge pins one timestamp per
// stream, the Cursor bridge stamps per chunk - both policies survive.

// TypeUpstreamError is the OpenAI error type for an upstream failure,
// carried in-band on a stream or recorded on an attempt. One owner: every
// emitter renders through this constant.
const TypeUpstreamError = "upstream_error"

// ErrorObject returns the client-facing OpenAI error object embedded in
// envelopes: {"message":...,"type":...,"param":null,"code":...}. code is
// nil or a provider error code string. ErrorEnvelope and the in-chunk error
// renderers share this one shape owner.
func ErrorObject(typ string, code any, msg string) map[string]any {
	return map[string]any{"message": msg, "type": typ, "param": nil, "code": code}
}

// DeltaChoice returns the single streaming choice element: index 0, one
// delta, and the finish_reason (nil until the terminating chunk).
func DeltaChoice(delta map[string]any, finish any) map[string]any {
	return map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
}

// Chunk returns one chat.completion.chunk envelope: id, created, model and
// the choices list, plus the extra root fields a chunk carries beside the
// skeleton (usage, error) when non-nil. choices holds the DeltaChoice
// elements, or an empty list for a usage/error-only chunk.
func Chunk(id string, created int64, model string, choices []map[string]any, extra map[string]any) map[string]any {
	obj := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"model": model, "choices": choices,
	}
	for k, v := range extra {
		obj[k] = v
	}
	return obj
}

// Completion returns one chat.completion envelope with a single choice: the
// assistant message, its finish_reason and the usage object. The
// non-streaming renderers of both bridges share it.
func Completion(id string, created int64, model string, message map[string]any, finish string, usage map[string]any) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   usage,
	}
}

// AssistantMessage assembles the OpenAI assistant message shared by the
// non-streaming completion renderers: role, the joined text content, and
// the turn's tool calls when it produced any.
func AssistantMessage(content string, toolCalls []map[string]any) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return message
}
