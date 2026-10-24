package format

// Round 5 C3 regression: a Chat stream error frame must carry the schema-required
// param and code members. Every other in-band path renders through
// sse.ErrorObject; the hand-built encoder object omitted both when the IR error
// had no code.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatStreamErrorObject(t *testing.T, out string) map[string]any {
	t.Helper()
	for _, frame := range strings.Split(out, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatal(err)
			}
			if failure, ok := payload["error"].(map[string]any); ok {
				return failure
			}
		}
	}
	t.Fatalf("no Chat error frame in: %s", out)
	return nil
}

func TestChatStreamErrorCarriesParamAndCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *ir.Error
		code any
	}{
		{name: "uncoded", err: &ir.Error{Type: "overloaded_error", Message: "busy"}, code: nil},
		{name: "coded", err: &ir.Error{Type: "rate_limit_error", Code: "rate", Message: "slow"}, code: "rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewChatStreamEncoder(&buf, nil, false)
			if err := enc.Write(&ir.Event{Type: ir.EventError, Error: tc.err}); err != nil {
				t.Fatal(err)
			}
			failure := chatStreamErrorObject(t, buf.String())
			param, hasParam := failure["param"]
			code, hasCode := failure["code"]
			if !hasParam || param != nil {
				t.Fatalf("param = %v (present %v), want explicit null: %v", param, hasParam, failure)
			}
			if !hasCode || code != tc.code {
				t.Fatalf("code = %v (present %v), want %v: %v", code, hasCode, tc.code, failure)
			}
			if failure["type"] != tc.err.Type || failure["message"] != tc.err.Message {
				t.Fatalf("error = %v, want type/message preserved", failure)
			}
		})
	}
}
