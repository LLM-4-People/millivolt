package format

// Round 8 B1 regression: a non-clean non-stream terminal may carry the prefix
// of a tool-call argument value the source never finished. A length-truncated
// Chat call and an incomplete or cancelled Responses call are conforming
// truncations, so the call is dropped with one warning while text and the
// source finish survive. A clean terminal still fails closed on the same bytes.

import (
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
