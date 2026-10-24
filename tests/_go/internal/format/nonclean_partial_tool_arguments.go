package format

// Round 8 B1 regression: a non-clean non-stream terminal may carry the prefix
// of a tool-call argument value the source never finished. A length-truncated
// Chat call and an incomplete or cancelled Responses call are conforming
// truncations, so the call is dropped with one warning while text and the
// source finish survive. A clean terminal still fails closed on the same bytes.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const (
	partialChatLength = `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial text","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":"length"}]}`
	partialChatStop   = `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial text","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":"stop"}]}`
)

func responsesPartialCallResponse(status, reason string) string {
	incomplete := ""
	if reason != "" {
		incomplete = `"incomplete_details":{"reason":"` + reason + `"},`
	}
	return `{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"` + status + `",` + incomplete + `"output":[` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"partial text"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":"}]}`
}

func TestNonCleanPartialToolArgumentsAreDropped(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     ir.Protocol
		target     ir.Protocol
		body       string
		want       string
		wantWarn   string
		wantFinish string
	}{
		{
			name:       "chat length drops the partial call",
			source:     ir.Chat,
			target:     ir.Anthropic,
			body:       partialChatLength,
			want:       "partial text",
			wantWarn:   "tool_calls.arguments",
			wantFinish: `"stop_reason":"max_tokens"`,
		},
		{
			name:       "chat content filter drops the partial call",
			source:     ir.Chat,
			target:     ir.Anthropic,
			body:       strings.Replace(partialChatLength, `"finish_reason":"length"`, `"finish_reason":"content_filter"`, 1),
			want:       "partial text",
			wantWarn:   "tool_calls.arguments",
			wantFinish: `"stop_reason":"refusal"`,
		},
		{
			name:       "responses incomplete drops the partial call",
			source:     ir.Responses,
			target:     ir.Chat,
			body:       responsesPartialCallResponse("incomplete", "max_output_tokens"),
			want:       "partial text",
			wantWarn:   "function_call.arguments",
			wantFinish: `"finish_reason":"length"`,
		},
		{
			name:       "responses cancelled drops the partial call",
			source:     ir.Responses,
			target:     ir.Chat,
			body:       responsesPartialCallResponse("cancelled", ""),
			want:       "partial text",
			wantWarn:   "function_call.arguments",
			wantFinish: `"finish_reason":"stop"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, warnings, err := TranslateResponseBetween(tc.target, tc.source, []byte(tc.body))
			if err != nil {
				t.Fatalf("non-clean terminal became a translator failure: %v", err)
			}
			wire := string(out)
			if !strings.Contains(wire, tc.want) {
				t.Fatalf("client lost the answer text %q: %s", tc.want, wire)
			}
			if strings.Contains(wire, "tool_use") || strings.Contains(wire, "tool_calls") {
				t.Fatalf("dropped call still reached the client: %s", wire)
			}
			if !strings.Contains(wire, tc.wantFinish) {
				t.Fatalf("client lost the source finish %s: %s", tc.wantFinish, wire)
			}
			if got := warningCount(warnings, ir.WarningDropped, tc.wantWarn); got != 1 {
				t.Fatalf("%s warnings = %d, want 1 (%+v)", tc.wantWarn, got, warnings)
			}
		})
	}

	t.Run("chat clean stop still fails closed", func(t *testing.T) {
		if _, _, err := TranslateResponseBetween(ir.Anthropic, ir.Chat, []byte(partialChatStop)); err == nil ||
			!strings.Contains(err.Error(), "tool call arguments must be") {
			t.Fatalf("error = %v, want the clean-terminal rejection", err)
		}
	})

	t.Run("responses completed still fails closed", func(t *testing.T) {
		body := responsesPartialCallResponse("completed", "")
		if _, _, err := TranslateResponseBetween(ir.Chat, ir.Responses, []byte(body)); err == nil ||
			!strings.Contains(err.Error(), "tool call arguments must be") {
			t.Fatalf("error = %v, want the clean-terminal rejection", err)
		}
	})
}

func responsesCallResponse(status, reason, arguments string) string {
	incomplete := ""
	if reason != "" {
		incomplete = `"incomplete_details":{"reason":"` + reason + `"},`
	}
	return `{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"` + status + `",` + incomplete + `"output":[` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"partial text"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":` + arguments + `}]}`
}

// translatedToolCall reports the translated tool-call count and accumulated
// arguments. A call that carries no argument bytes reports {} because each
// target encodes an empty object as either an empty input or an empty value.
func translatedToolCall(t *testing.T, target ir.Protocol, wire string) (int, string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(wire), &doc); err != nil {
		t.Fatalf("translated body is not JSON: %v\n%s", err, wire)
	}
	switch target {
	case ir.Chat:
		choices, _ := doc["choices"].([]any)
		if len(choices) == 0 {
			return 0, ""
		}
		message, _ := choices[0].(map[string]any)["message"].(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		var args strings.Builder
		for _, raw := range calls {
			function, _ := raw.(map[string]any)["function"].(map[string]any)
			if value, _ := function["arguments"].(string); value != "" {
				args.WriteString(value)
			}
		}
		if len(calls) > 0 && args.Len() == 0 {
			args.WriteString("{}")
		}
		return len(calls), args.String()
	case ir.Anthropic:
		content, _ := doc["content"].([]any)
		count := 0
		var args strings.Builder
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if block["type"] != "tool_use" {
				continue
			}
			count++
			if input, ok := block["input"].(map[string]any); ok && len(input) > 0 {
				encoded, _ := json.Marshal(input)
				args.Write(encoded)
			}
		}
		if count > 0 && args.Len() == 0 {
			args.WriteString("{}")
		}
		return count, args.String()
	case ir.Responses:
		output, _ := doc["output"].([]any)
		count := 0
		var args strings.Builder
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] != "function_call" {
				continue
			}
			count++
			if value, _ := item["arguments"].(string); value != "" {
				args.WriteString(value)
			}
		}
		if count > 0 && args.Len() == 0 {
			args.WriteString("{}")
		}
		return count, args.String()
	}
	return 0, ""
}

func TestResponsesNonCleanTerminalMissingArguments(t *testing.T) {
	const empty = `""`
	const null = `null`
	const whitespace = `"   "`
	const explicitEmpty = `"{}"`
	const complete = `"{\"q\":1}"`
	for _, tc := range []struct {
		name      string
		status    string
		reason    string
		arguments string
		wantCall  bool
		wantArgs  string
	}{
		{"incomplete empty", "incomplete", "max_output_tokens", empty, false, ""},
		{"incomplete null", "incomplete", "max_output_tokens", null, false, ""},
		{"incomplete whitespace", "incomplete", "max_output_tokens", whitespace, false, ""},
		{"cancelled empty", "cancelled", "", empty, false, ""},
		{"cancelled null", "cancelled", "", null, false, ""},
		{"cancelled whitespace", "cancelled", "", whitespace, false, ""},
		{"completed with incomplete reason empty", "completed", "max_output_tokens", empty, false, ""},
		{"incomplete explicit empty", "incomplete", "max_output_tokens", explicitEmpty, true, "{}"},
		{"cancelled explicit empty", "cancelled", "", explicitEmpty, true, "{}"},
		{"incomplete complete", "incomplete", "max_output_tokens", complete, true, `{"q":1}`},
		{"completed empty", "completed", "", empty, true, "{}"},
		{"completed complete", "completed", "", complete, true, `{"q":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(responsesCallResponse(tc.status, tc.reason, tc.arguments))
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					out, warnings, err := TranslateResponseBetween(target, ir.Responses, body)
					if err != nil {
						t.Fatalf("translation failed: %v", err)
					}
					wire := string(out)
					if !strings.Contains(wire, "partial text") {
						t.Fatalf("client lost the answer text: %s", wire)
					}
					count, args := translatedToolCall(t, target, wire)
					if got := warningCount(warnings, ir.WarningDropped, "function_call.arguments"); got != 0 && tc.wantCall {
						t.Fatalf("kept call carried %d argument warnings: %+v", got, warnings)
					}
					if !tc.wantCall {
						if count != 0 {
							t.Fatalf("unusable call reached the client: count=%d args=%q\n%s", count, args, wire)
						}
						if got := warningCount(warnings, ir.WarningDropped, "function_call.arguments"); got != 1 {
							t.Fatalf("argument warnings = %d, want 1: %+v", got, warnings)
						}
						return
					}
					if count != 1 || args != tc.wantArgs {
						t.Fatalf("tool calls = %d args = %q, want one %q\n%s", count, args, tc.wantArgs, wire)
					}
				})
			}
		})
	}
}

func chatCallResponse(finish, arguments string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial text","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":` + arguments + `}}]},"finish_reason":"` + finish + `"}]}`
}

func TestChatNonCleanTerminalMissingArguments(t *testing.T) {
	const empty = `""`
	const null = `null`
	const whitespace = `"   "`
	const explicitEmpty = `"{}"`
	const complete = `"{\"q\":1}"`
	for _, tc := range []struct {
		name      string
		finish    string
		arguments string
		wantCall  bool
		wantArgs  string
	}{
		{"length empty", "length", empty, false, ""},
		{"length null", "length", null, false, ""},
		{"length whitespace", "length", whitespace, false, ""},
		{"content filter whitespace", "content_filter", whitespace, false, ""},
		{"length explicit empty", "length", explicitEmpty, true, "{}"},
		{"length complete", "length", complete, true, `{"q":1}`},
		{"stop empty", "stop", empty, true, "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(chatCallResponse(tc.finish, tc.arguments))
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					out, warnings, err := TranslateResponseBetween(target, ir.Chat, body)
					if err != nil {
						t.Fatalf("translation failed: %v", err)
					}
					wire := string(out)
					if !strings.Contains(wire, "partial text") {
						t.Fatalf("client lost the answer text: %s", wire)
					}
					count, args := translatedToolCall(t, target, wire)
					if !tc.wantCall {
						if count != 0 {
							t.Fatalf("unusable call reached the client: count=%d args=%q\n%s", count, args, wire)
						}
						if got := warningCount(warnings, ir.WarningDropped, "tool_calls.arguments"); got != 1 {
							t.Fatalf("argument warnings = %d, want 1: %+v", got, warnings)
						}
						return
					}
					if count != 1 || args != tc.wantArgs {
						t.Fatalf("tool calls = %d args = %q, want one %q\n%s", count, args, tc.wantArgs, wire)
					}
				})
			}
		})
	}
}

func TestChatNonCleanLegacyFunctionMissingArgumentsDropped(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial text","function_call":{"name":"lookup","arguments":""}},"finish_reason":"length"}]}`)
	out, warnings, err := TranslateResponseBetween(ir.Chat, ir.Chat, body)
	if err != nil {
		t.Fatalf("translation failed: %v", err)
	}
	wire := string(out)
	if !strings.Contains(wire, "partial text") {
		t.Fatalf("client lost the answer text: %s", wire)
	}
	if count, args := translatedToolCall(t, ir.Chat, wire); count != 0 {
		t.Fatalf("unusable legacy call reached the client: count=%d args=%q\n%s", count, args, wire)
	}
	if got := warningCount(warnings, ir.WarningDropped, "function_call.arguments"); got != 1 {
		t.Fatalf("argument warnings = %d, want 1: %+v", got, warnings)
	}
}
