package format

// Round 9 B3 regression: Anthropic error type and message were the only error
// text surfaces that reached the client unbounded. Both the non-streaming
// envelope and the SSE error event now use the shared preview truncation.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicErrorTextBounded(t *testing.T) {
	long := strings.Repeat("x", 4096)
	limit := metrics.PreviewMaxBytes + len("…")
	e, err := DecodeAnthropicError([]byte(`{"type":"error","error":{"type":"` + long + `","message":"` + long + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Type) > limit || len(e.Message) > limit {
		t.Fatalf("decoded error sizes = type %d message %d, want at most %d", len(e.Type), len(e.Message), limit)
	}
	if !strings.HasSuffix(e.Message, "…") {
		t.Fatalf("decoded message = %q, want the truncation marker", e.Message)
	}

	short, err := DecodeAnthropicError([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if short.Type != "overloaded_error" || short.Message != "busy" {
		t.Fatalf("short error = %+v, want it unchanged", short)
	}

	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"` + long + `","message":"` + long + `"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"
	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), long) {
		t.Fatalf("stream error kept the 4 KiB text")
	}
	if got := strings.Count(out.String(), "…"); got < 2 {
		t.Fatalf("stream error markers = %d, want both type and message truncated: %s", got, out.String())
	}
}
