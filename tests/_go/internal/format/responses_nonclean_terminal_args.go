package format

// Round 7 verifier NEW-1 and round 8 H1 regression: the Responses terminal
// class decides how an incomplete function-call value is treated. Failed,
// errored and truncated terminals drop the unparseable call so the provider
// error or truncation survives. Incomplete and cancelled are normal turn
// boundaries, so a partial terminal or buffered value fails closed instead of
// closing the tool block with an executable empty object. A completed terminal
// fails closed on the same bytes, and a complete value on an incomplete
// terminal is still emitted.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesFunctionCallItem(arguments string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_1", "call_id": "call_1",
		"name": "lookup", "arguments": arguments,
	}
}

func appendResponsesTerminalPayload(t *testing.T, b *strings.Builder, eventType, status string, output []any, extra map[string]any) {
	t.Helper()
	response := map[string]any{
		"id": "resp_1", "model": "m", "status": status, "output": output,
	}
	for key, value := range extra {
		response[key] = value
	}
	appendResponsesEvent(t, b, eventType, map[string]any{"type": eventType, "response": response})
}

func responsesTerminalArgsStream(t *testing.T, eventType, status string, output []any, extra map[string]any) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	appendResponsesTerminalPayload(t, &b, eventType, status, output, extra)
	return b.String()
}

func responsesBufferedTerminalArgsStream(t *testing.T, arguments, eventType, status string, extra map[string]any) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	appendResponsesEvent(t, &b, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": responsesFunctionCallItem(arguments),
	})
	appendResponsesTerminalPayload(t, &b, eventType, status, nil, extra)
	return b.String()
}

func chatToolArgumentValues(t *testing.T, out string) string {
	t.Helper()
	var args strings.Builder
	for _, payload := range chatPayloads(t, out) {
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			function, _ := raw.(map[string]any)["function"].(map[string]any)
			value, _ := function["arguments"].(string)
			args.WriteString(value)
		}
	}
	return args.String()
}

func TestResponsesNonCleanTerminalPartialArguments(t *testing.T) {
	const partial = `{"q":`
	const complete = `{"q":1}`
	failedExtra := map[string]any{"error": map[string]any{
		"type": "server_error", "code": "overloaded", "message": "boom",
	}}
	incompleteExtra := map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}}

	for _, tc := range []struct {
		name     string
		source   string
		want     string
		wantArgs string
		wantErr  bool
	}{
		{
			name:    "failed terminal partial",
			source:  responsesTerminalArgsStream(t, "response.failed", "failed", []any{responsesFunctionCallItem(partial)}, failedExtra),
			want:    `"code":"overloaded"`,
			wantErr: false,
		},
		{
			name:     "failed buffered added partial keeps the accepted empty block",
			source:   responsesBufferedTerminalArgsStream(t, partial, "response.failed", "failed", failedExtra),
			want:     `"code":"overloaded"`,
			wantArgs: "",
		},
		{
			name:    "incomplete terminal partial fails closed",
			source:  responsesTerminalArgsStream(t, "response.incomplete", "incomplete", []any{responsesFunctionCallItem(partial)}, incompleteExtra),
			wantErr: true,
		},
		{
			name:    "incomplete buffered added partial fails closed",
			source:  responsesBufferedTerminalArgsStream(t, partial, "response.incomplete", "incomplete", incompleteExtra),
			wantErr: true,
		},
		{
			name:    "cancelled terminal partial fails closed",
			source:  responsesTerminalArgsStream(t, "response.cancelled", "cancelled", []any{responsesFunctionCallItem(partial)}, nil),
			wantErr: true,
		},
		{
			name:    "cancelled buffered added partial fails closed",
			source:  responsesBufferedTerminalArgsStream(t, partial, "response.cancelled", "cancelled", nil),
			wantErr: true,
		},
		{
			name:     "incomplete terminal complete value",
			source:   responsesTerminalArgsStream(t, "response.incomplete", "incomplete", []any{responsesFunctionCallItem(complete)}, incompleteExtra),
			want:     `"finish_reason":"length"`,
			wantArgs: complete,
		},
		{
			name:    "completed terminal partial still fails closed",
			source:  responsesTerminalArgsStream(t, "response.completed", "completed", []any{responsesFunctionCallItem(partial)}, nil),
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(tc.source), nil, nil, StreamOptions{})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "arguments") {
					t.Fatalf("error = %v, want the incomplete-argument rejection", err)
				}
				if bytes.Contains(buf.Bytes(), []byte(`"arguments":"{}"`)) {
					t.Fatalf("a partial call closed with an executable empty object: %s", buf.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("non-clean terminal became a translator failure: %v", err)
			}
			got := buf.String()
			if strings.Contains(got, "translation_error") {
				t.Fatalf("client received a translation_error frame: %s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("client missing %s: %s", tc.want, got)
			}
			args := chatToolArgumentValues(t, got)
			if args != tc.wantArgs {
				t.Fatalf("tool arguments = %q, want %q: %s", args, tc.wantArgs, got)
			}
			if !strings.Contains(got, "data: [DONE]") {
				t.Fatalf("client stream has no terminal sentinel: %s", got)
			}
		})
	}
}
