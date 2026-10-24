package format

// Round 8 verifier NEW-1 regression: strict incomplete/cancelled validation
// must not leave an executable empty tool call. A partial or absent argument
// value fails the translation, and the target abort must not close an open tool
// block as a completed empty call. An explicit empty object and a clean
// completed turn without arguments remain valid.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesWhitespaceDeltaStream(t *testing.T, eventType, status string, extra map[string]any, deltas ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	appendResponsesEvent(t, &b, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": responsesFunctionCallItem(""),
	})
	for _, delta := range deltas {
		appendResponsesEvent(t, &b, "response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "output_index": 0, "delta": delta,
		})
	}
	appendResponsesTerminalPayload(t, &b, eventType, status, nil, extra)
	return b.String()
}

func TestResponsesStrictToolArgumentsAbortMatrix(t *testing.T) {
	incompleteExtra := map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}}
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name:   "announced partial",
			source: responsesBufferedTerminalArgsStream(t, `{"q":`, "response.incomplete", "incomplete", incompleteExtra),
		},
		{
			name:   "announced no arguments",
			source: responsesBufferedTerminalArgsStream(t, "", "response.incomplete", "incomplete", incompleteExtra),
		},
		{
			name:   "terminal partial",
			source: responsesTerminalArgsStream(t, "response.incomplete", "incomplete", []any{responsesFunctionCallItem(`{"q":`)}, incompleteExtra),
		},
		{
			name:   "terminal no arguments",
			source: responsesTerminalArgsStream(t, "response.incomplete", "incomplete", []any{responsesFunctionCallItem("")}, incompleteExtra),
		},
		{
			name:   "cancelled no arguments",
			source: responsesTerminalArgsStream(t, "response.cancelled", "cancelled", []any{responsesFunctionCallItem("")}, nil),
		},
		{
			name:   "announced whitespace delta on incomplete",
			source: responsesWhitespaceDeltaStream(t, "response.incomplete", "incomplete", incompleteExtra, "   "),
		},
		{
			name:   "announced whitespace delta on cancelled",
			source: responsesWhitespaceDeltaStream(t, "response.cancelled", "cancelled", nil, "   "),
		},
		{
			name:   "terminal whitespace on incomplete",
			source: responsesTerminalArgsStream(t, "response.incomplete", "incomplete", []any{responsesFunctionCallItem("   ")}, incompleteExtra),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					var buf bytes.Buffer
					err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(tc.source), nil, nil, StreamOptions{})
					if err == nil || !strings.Contains(err.Error(), "arguments") {
						t.Fatalf("error = %v, want the missing-arguments rejection (wire %s)", err, buf.String())
					}
					out := buf.String()
					if strings.Contains(out, `"arguments":"{}"`) {
						t.Fatalf("executable empty arguments reached the client: %s", out)
					}
					if target == ir.Anthropic && strings.Contains(out, `"type":"content_block_stop"`) {
						t.Fatalf("unvalidated Anthropic tool block was closed: %s", out)
					}
					if target == ir.Chat && (strings.Contains(out, `"finish_reason":"stop"`) ||
						strings.Contains(out, `"finish_reason":"length"`) ||
						strings.Contains(out, `"finish_reason":"tool_calls"`)) {
						t.Fatalf("failed Chat translation carried a successful finish: %s", out)
					}
					if target == ir.Responses && strings.Contains(out, `"status":"completed"`) {
						t.Fatalf("failed Responses translation completed an item: %s", out)
					}
				})
			}
		})
	}
}

func TestResponsesStrictToolArgumentsExplicitEmptyAllowed(t *testing.T) {
	incompleteExtra := map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}}
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name:   "explicit empty object on incomplete",
			source: responsesTerminalArgsStream(t, "response.incomplete", "incomplete", []any{responsesFunctionCallItem(`{}`)}, incompleteExtra),
		},
		{
			name:   "completed call without arguments",
			source: responsesTerminalArgsStream(t, "response.completed", "completed", []any{responsesFunctionCallItem("")}, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(string(target), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(tc.source), nil, nil, StreamOptions{}); err != nil {
						t.Fatalf("valid empty call failed: %v", err)
					}
					out := buf.String()
					switch target {
					case ir.Chat:
						if got := chatToolArgumentValues(t, out); got != "{}" {
							t.Fatalf("Chat arguments = %q, want {}", got)
						}
					case ir.Anthropic:
						if inputs := anthropicToolInputs(t, out); len(inputs) != 1 || len(inputs[0]) != 0 {
							t.Fatalf("Anthropic inputs = %v, want one empty object", inputs)
						}
					case ir.Responses:
						if !strings.Contains(out, `"arguments":"{}"`) {
							t.Fatalf("Responses empty call missing: %s", out)
						}
					}
				})
			}
		})
	}
}

// A whitespace-only delta is not argument evidence, but whitespace followed by
// a real object accumulates to valid JSON and must still complete the call.
func TestResponsesWhitespaceThenObjectDeltaCompletes(t *testing.T) {
	incompleteExtra := map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}}
	source := responsesWhitespaceDeltaStream(t, "response.incomplete", "incomplete", incompleteExtra, "   ", `{"q":1}`)
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("whitespace-prefixed object failed: %v", err)
	}
	got := chatToolArgumentValues(t, buf.String())
	if strings.TrimSpace(got) != `{"q":1}` || !json.Valid([]byte(got)) {
		t.Fatalf("Chat arguments = %q, want a JSON object with the leading whitespace", got)
	}
}

func TestValidateStreamedToolArgumentsWhitespace(t *testing.T) {
	if err := validateStreamedToolArguments(""); err != nil {
		t.Fatalf("no emitted bytes = %v, want nil", err)
	}
	for _, raw := range []string{" ", "\n", " \t "} {
		if err := validateStreamedToolArguments(raw); !errors.Is(err, errToolArgumentsJSON) {
			t.Fatalf("validateStreamedToolArguments(%q) = %v, want errToolArgumentsJSON", raw, err)
		}
	}
	if err := validateStreamedToolArguments("{}"); err != nil {
		t.Fatalf("explicit empty object = %v, want nil", err)
	}
}
