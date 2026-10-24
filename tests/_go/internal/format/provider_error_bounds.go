package format

// C4 regression: provider-controlled error type/code/message is bounded at
// every stream decoder and the Anthropic non-stream error decoder.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestProviderStreamErrorTextBounded(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	check := func(t *testing.T, e *ir.Error, wantCode bool) {
		t.Helper()
		if e == nil {
			t.Fatal("no error event")
		}
		for name, value := range map[string]string{"type": e.Type, "message": e.Message} {
			if value == "" {
				t.Fatalf("%s field is empty", name)
			}
			if len(value) > metrics.PreviewMaxBytes+3 {
				t.Fatalf("%s length = %d, want the preview bound", name, len(value))
			}
		}
		if wantCode {
			if e.Code == "" {
				t.Fatal("code field is empty")
			}
			if len(e.Code) > metrics.PreviewMaxBytes+3 {
				t.Fatalf("code length = %d, want the preview bound", len(e.Code))
			}
		}
	}

	t.Run("chat", func(t *testing.T) {
		source := `data: {"error":{"type":"` + long + `","code":"` + long + `","message":"` + long + `"}}` + "\n\n"
		ev, err := NewChatStreamDecoder(strings.NewReader(source)).Next()
		if err != nil {
			t.Fatal(err)
		}
		check(t, ev.Error, true)
	})

	t.Run("responses", func(t *testing.T) {
		source := `event: error` + "\n" +
			`data: {"type":"error","code":"` + long + `","message":"` + long + `"}` + "\n\n"
		ev, err := NewResponsesStreamDecoder(strings.NewReader(source)).Next()
		if err != nil {
			t.Fatal(err)
		}
		check(t, ev.Error, true)
	})

	t.Run("anthropic stream", func(t *testing.T) {
		source := `event: error` + "\n" +
			`data: {"type":"error","error":{"type":"` + long + `","message":"` + long + `"}}` + "\n\n"
		ev, err := NewAnthropicStreamDecoder(strings.NewReader(source), nil).Next()
		if err != nil {
			t.Fatal(err)
		}
		check(t, ev.Error, false)
	})

	t.Run("anthropic envelope", func(t *testing.T) {
		body := `{"type":"error","error":{"type":"` + long + `","message":"` + long + `"}}`
		e, err := DecodeAnthropicError([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		check(t, e, false)
	})
}
