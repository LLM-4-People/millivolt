package format

// Round 5 C2 regression: the Anthropic stream error event maps the shared error
// vocabulary through anthropicErrorType, exactly like the non-stream owner, so a
// truncated internal class or a foreign server_error becomes api_error while
// Anthropic's own classes pass through.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicStreamErrorType(t *testing.T, out string) string {
	t.Helper()
	for _, frame := range strings.Split(out, "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if name != "error" || data == "" {
			continue
		}
		var payload struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Error.Type
	}
	return ""
}

func TestAnthropicStreamErrorUsesAnthropicVocabulary(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want string
	}{
		{"upstream_error", "api_error"},
		{"server_error", "api_error"},
		{"overloaded_error", "overloaded_error"},
		{"rate_limit_error", "rate_limit_error"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewAnthropicStreamEncoder(&buf, nil)
			if err := enc.Write(&ir.Event{Type: ir.EventError, Error: &ir.Error{Type: tc.typ, Message: "boom"}}); err != nil {
				t.Fatal(err)
			}
			if got := anthropicStreamErrorType(t, buf.String()); got != tc.want {
				t.Fatalf("error type = %q, want %q: %s", got, tc.want, buf.String())
			}
		})
	}
}

func TestTruncatedChatStreamErrorMapsToAPIError(t *testing.T) {
	const source = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := anthropicStreamErrorType(t, buf.String()); got != "api_error" {
		t.Fatalf("truncated error type = %q, want api_error: %s", got, buf.String())
	}
	if strings.Contains(buf.String(), `"type":"upstream_error"`) {
		t.Fatalf("internal class leaked to the Anthropic client: %s", buf.String())
	}
}
