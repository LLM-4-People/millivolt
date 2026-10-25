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

// TestNonStreamProviderErrorFieldsBounded is the B2 regression: the non-stream
// Chat, Responses and error-body decoders copy provider type/code into the IR
// and every target encoder echoes them. A 200 in-band envelope may be as large
// as the translation cap, so all three fields must carry the shared bound.
func TestNonStreamProviderErrorFieldsBounded(t *testing.T) {
	long := strings.Repeat("T", 200_000)
	check := func(t *testing.T, e *ir.Error, target ir.Protocol) {
		t.Helper()
		if e == nil {
			t.Fatal("no decoded error")
		}
		for name, value := range map[string]string{"type": e.Type, "code": e.Code, "message": e.Message} {
			if len(value) > metrics.PreviewMaxBytes+3 {
				t.Fatalf("%s length = %d, want the preview bound", name, len(value))
			}
		}
		out, err := EncodeError(target, e)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) >= 1024 {
			t.Fatalf("encoded %s error is %d bytes, want the bounded envelope", target, len(out))
		}
		if strings.Contains(string(out), long[:1024]) {
			t.Fatalf("encoded %s error carries the unbounded provider value", target)
		}
	}

	envelope := `"type":"` + long + `","code":"` + long + `","message":"m"`

	t.Run("chat 200 envelope", func(t *testing.T) {
		body := `{"id":"c","object":"chat.completion","created":1,"model":"m","error":{` + envelope + `},"choices":[]}`
		resp, err := DecodeChatResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		check(t, resp.Error, ir.Chat)
	})

	t.Run("responses 200 envelope", func(t *testing.T) {
		body := `{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[],"error":{` + envelope + `}}`
		resp, err := DecodeResponsesResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		check(t, resp.Error, ir.Responses)
	})

	t.Run("responses failed status", func(t *testing.T) {
		body := `{"id":"r","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{` + envelope + `}}`
		resp, err := DecodeResponsesResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		check(t, resp.Error, ir.Responses)
	})

	t.Run("responses error body", func(t *testing.T) {
		e, err := DecodeResponsesError([]byte(`{"error":{` + envelope + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		check(t, e, ir.Responses)
	})
}

// TestFailedTerminalErrorFieldsBounded is the C1 regression: the
// response.failed terminal branch copied error.type and code without the
// shared bound, and the Chat encoder echoes both verbatim.
func TestFailedTerminalErrorFieldsBounded(t *testing.T) {
	long := strings.Repeat("T", 200_000)
	source := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\"," +
		"\"error\":{\"type\":\"" + long + "\",\"code\":\"" + long + "\",\"message\":\"m\"},\"output\":[]}}\n\n"
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
		t.Run(string(target), func(t *testing.T) {
			out, err := translateStreamSource(t, ir.Responses, target, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) >= 1024 {
				t.Fatalf("failed-terminal %s output is %d bytes, want the bounded error", target, len(out))
			}
			if strings.Contains(out, long[:1024]) {
				t.Fatalf("failed-terminal %s output carries the unbounded provider value", target)
			}
			if !strings.Contains(out, "upstream response failed") && !strings.Contains(out, `"message":"m"`) {
				t.Fatalf("failed-terminal %s output lost the error message: %s", target, out)
			}
		})
	}
}
