package format

// Round 13 B1/H1 regressions: every error consumer decodes its members
// independently. A wrong-typed member must not abort a stream or erase a valid
// sibling, while a structurally empty error value stays a decoy.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicErrorTolerantMembers(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTyp string
		wantCde string
		wantMsg string
		decoy   bool
	}{
		{
			name:    "numeric type keeps string message",
			body:    `{"type":"error","error":{"type":529,"message":"boom"}}`,
			wantMsg: "boom",
		},
		{
			name:    "object message keeps valid type",
			body:    `{"type":"error","error":{"type":"overloaded_error","message":{"detail":"x"}}}`,
			wantTyp: "overloaded_error",
		},
		{
			name:    "numeric code keeps string message",
			body:    `{"type":"error","error":{"code":529,"message":"boom"}}`,
			wantCde: "529",
			wantMsg: "boom",
		},
		{
			name:    "valid envelope unchanged",
			body:    `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
			wantTyp: "overloaded_error",
			wantMsg: "busy",
		},
		{
			name:    "wrong-typed only yields a generic error",
			body:    `{"type":"error","error":{"type":529,"message":{"detail":"x"}}}`,
			wantTyp: "api_error",
			wantMsg: genericProviderErrorMessage,
		},
		{
			name:  "empty object is a decoy",
			body:  `{"type":"error","error":{}}`,
			decoy: true,
		},
		{
			name:  "empty string is a decoy",
			body:  `{"type":"error","error":""}`,
			decoy: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeAnthropicError([]byte(tc.body))
			if tc.decoy {
				if err == nil {
					t.Fatalf("decoy decoded as a provider error: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeAnthropicError = %v", err)
			}
			if got.Type != tc.wantTyp || got.Code != tc.wantCde || got.Message != tc.wantMsg {
				t.Fatalf("error = type %q code %q message %q, want %q/%q/%q",
					got.Type, got.Code, got.Message, tc.wantTyp, tc.wantCde, tc.wantMsg)
			}
		})
	}
}

func chatErrorStream(value string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"error":` + value + "}\n\ndata: [DONE]\n\n"
}

func anthropicErrorStream(value string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error","error":` + value + "}\n\n"
}

func responsesErrorStream(fields string) string {
	return "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		"event: error\n" +
		`data: {"type":"error"` + fields + "}\n\n"
}

// chatStreamPayloads decodes the data frames of a Chat-family stream. The
// [DONE] sentinel is not a document and is skipped.
func chatStreamPayloads(t *testing.T, out string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, frame := range strings.Split(out, "\n\n") {
		data := ""
		for _, line := range strings.Split(frame, "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "data: "); ok {
				data = value
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		payloads = append(payloads, obj)
	}
	return payloads
}

// wireErrorCount counts error statements a client of target can observe.
func wireErrorCount(t *testing.T, target ir.Protocol, out string) int {
	t.Helper()
	count := 0
	switch target {
	case ir.Chat:
		for _, payload := range chatStreamPayloads(t, out) {
			if payload["error"] != nil {
				count++
			}
		}
	case ir.Anthropic:
		_, payloads := anthropicEventData(t, out)
		for _, payload := range payloads {
			if payload["type"] == "error" {
				count++
			}
		}
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] == "error" || payload["type"] == "response.failed" {
				count++
			}
		}
	}
	return count
}

func translateErrorStream(t *testing.T, source, target ir.Protocol, stream string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := TranslateStreamBetween(target, source, &buf, strings.NewReader(stream), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("%s to %s aborted: %v", source, target, err)
	}
	return buf.String()
}

func TestTranslatorErrorMemberTolerance(t *testing.T) {
	targets := []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses}
	cases := []struct {
		name     string
		value    string
		wantText string
	}{
		{"numeric type keeps its message", `{"type":529,"message":"boom"}`, "boom"},
		{"object message keeps its type", `{"type":"overloaded_error","message":{"detail":"x"}}`, "overloaded_error"},
		{"numeric code keeps its message", `{"code":529,"message":"boom"}`, "boom"},
		{"wrong-typed only is generic", `{"type":529,"message":{"detail":"x"}}`, genericProviderErrorMessage},
	}
	for _, tc := range cases {
		t.Run("anthropic/"+tc.name, func(t *testing.T) {
			for _, target := range targets {
				out := translateErrorStream(t, ir.Anthropic, target, anthropicErrorStream(tc.value))
				if !strings.Contains(out, tc.wantText) {
					t.Fatalf("%s lost %q: %s", target, tc.wantText, out)
				}
				if wireErrorCount(t, target, out) == 0 {
					t.Fatalf("%s emitted no error statement: %s", target, out)
				}
			}
		})
		t.Run("chat/"+tc.name, func(t *testing.T) {
			for _, target := range targets {
				out := translateErrorStream(t, ir.Chat, target, chatErrorStream(tc.value))
				if !strings.Contains(out, tc.wantText) {
					t.Fatalf("%s lost %q: %s", target, tc.wantText, out)
				}
				if wireErrorCount(t, target, out) == 0 {
					t.Fatalf("%s emitted no error statement: %s", target, out)
				}
			}
		})
	}

	t.Run("responses", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			fields   string
			wantText string
		}{
			{"numeric code keeps its message", `,"code":529,"message":"boom"`, "boom"},
			{"object message is generic", `,"message":{"detail":"x"}`, genericProviderErrorMessage},
			{"wrong-typed only is generic", `,"code":{"x":1},"message":{"y":2}`, genericProviderErrorMessage},
			{"object param keeps valid siblings", `,"param":{"x":1},"code":"server_error","message":"boom"`, "boom"},
			{"array param keeps valid siblings", `,"param":[1],"code":"server_error","message":"boom"`, "boom"},
			{"empty event is generic", ``, genericProviderErrorMessage},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, target := range targets {
					out := translateErrorStream(t, ir.Responses, target, responsesErrorStream(tc.fields))
					if !strings.Contains(out, tc.wantText) {
						t.Fatalf("%s lost %q: %s", target, tc.wantText, out)
					}
					if wireErrorCount(t, target, out) == 0 {
						t.Fatalf("%s emitted no error statement: %s", target, out)
					}
				}
			})
		}
	})
}

func TestTranslatorEmptyErrorValueIsDecoy(t *testing.T) {
	// An Anthropic error event with an empty object is not a provider
	// statement: it keeps its existing missing-terminal classification, never
	// the generic provider text.
	for _, value := range []string{`{}`, `""`, `null`} {
		out := translateErrorStream(t, ir.Anthropic, ir.Chat, anthropicErrorStream(value))
		if !strings.Contains(out, metrics.CodeTruncated) {
			t.Fatalf("empty error value %s was not treated as a missing terminal: %s", value, out)
		}
		if strings.Contains(out, genericProviderErrorMessage) {
			t.Fatalf("empty error value %s produced the generic provider error: %s", value, out)
		}
	}

	// A Chat chunk that carries a decoy error object alongside valid content
	// must not mask that content or emit an error.
	for _, value := range []string{`{}`, `""`, `null`} {
		stream := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}],"error":` + value + "}\n\n" +
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
		out := translateErrorStream(t, ir.Chat, ir.Chat, stream)
		if !strings.Contains(out, `"content":"ok"`) {
			t.Fatalf("decoy error value %s masked the chunk content: %s", value, out)
		}
		if wireErrorCount(t, ir.Chat, out) != 0 {
			t.Fatalf("decoy error value %s produced an error frame: %s", value, out)
		}
	}
}
