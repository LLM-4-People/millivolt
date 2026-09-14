package sse

import (
	"encoding/json"
	"testing"
)

// The envelope constructors are the single owner of the OpenAI wire shapes
// both format bridges render. These pins hold the exact marshaled bytes so a
// change to the chunk envelope, the completion envelope, the shared choice
// or message assembly, or the error object can never drift silently: the
// values below are the bytes clients see today (encoding/json sorts map
// keys, so the field order is deterministic).

func TestChunkEnvelopeBytes(t *testing.T) {
	// A delta chunk: one DeltaChoice, no extra root fields.
	b, err := json.Marshal(Chunk("req-1", 1700000000, "claude-sonnet-4-5",
		[]map[string]any{DeltaChoice(map[string]any{"content": "hi"}, nil)}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"created":1700000000,"id":"req-1","model":"claude-sonnet-4-5","object":"chat.completion.chunk"}`
	if string(b) != want {
		t.Fatalf("delta chunk = %s, want %s", b, want)
	}

	// A usage-only chunk: empty choices, usage at the root.
	b, err = json.Marshal(Chunk("req-1", 1700000000, "m", []map[string]any{},
		map[string]any{"usage": map[string]any{"prompt_tokens": 10}}))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"choices":[],"created":1700000000,"id":"req-1","model":"m","object":"chat.completion.chunk","usage":{"prompt_tokens":10}}`
	if string(b) != want {
		t.Fatalf("usage chunk = %s, want %s", b, want)
	}

	// An error chunk: the shared four-field error object at the root.
	b, err = json.Marshal(Chunk("req-1", 1700000000, "m", []map[string]any{},
		map[string]any{"error": ErrorObject(TypeUpstreamError, "truncated", "stream ended")}))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"choices":[],"created":1700000000,"error":{"code":"truncated","message":"stream ended","param":null,"type":"upstream_error"},"id":"req-1","model":"m","object":"chat.completion.chunk"}`
	if string(b) != want {
		t.Fatalf("error chunk = %s, want %s", b, want)
	}

	// A finish chunk: empty delta, string finish_reason.
	b, err = json.Marshal(Chunk("req-1", 1700000000, "m",
		[]map[string]any{DeltaChoice(map[string]any{}, "stop")}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1700000000,"id":"req-1","model":"m","object":"chat.completion.chunk"}`
	if string(b) != want {
		t.Fatalf("finish chunk = %s, want %s", b, want)
	}
}

func TestCompletionEnvelopeBytes(t *testing.T) {
	b, err := json.Marshal(Completion("req-1", 1700000000, "claude-sonnet-4-5",
		AssistantMessage("hello", nil), "stop",
		map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"hello","role":"assistant"}}],"created":1700000000,"id":"req-1","model":"claude-sonnet-4-5","object":"chat.completion","usage":{"completion_tokens":2,"prompt_tokens":3,"total_tokens":5}}`
	if string(b) != want {
		t.Fatalf("completion = %s, want %s", b, want)
	}

	// Tool calls join the message only when the turn produced any.
	b, err = json.Marshal(Completion("req-1", 1700000000, "m",
		AssistantMessage("", []map[string]any{{"id": "call-1"}}), "tool_calls", nil))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"choices":[{"finish_reason":"tool_calls","index":0,"message":{"content":"","role":"assistant","tool_calls":[{"id":"call-1"}]}}],"created":1700000000,"id":"req-1","model":"m","object":"chat.completion","usage":null}`
	if string(b) != want {
		t.Fatalf("tool-call completion = %s, want %s", b, want)
	}
}

func TestErrorObjectBytes(t *testing.T) {
	b, err := json.Marshal(ErrorObject("upstream_error", nil, "gone"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"code":null,"message":"gone","param":null,"type":"upstream_error"}`; string(b) != want {
		t.Fatalf("error object = %s, want %s", b, want)
	}
	// The owned type constant is the wire spelling every emitter renders.
	if TypeUpstreamError != "upstream_error" {
		t.Fatalf("TypeUpstreamError = %q, want the OpenAI wire spelling", TypeUpstreamError)
	}
}
