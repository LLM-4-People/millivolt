package format

// Round 3 recheck NEW-3 regression: aggregate tool-argument coverage is a
// bounded prefix, not the result. When parallel legitimate calls exceed it,
// the truncated prefix must not be validated as if it were the final value.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func appendResponsesEvent(t *testing.T, b *strings.Builder, name string, payload map[string]any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
}

func largeToolArguments(marker string, size int) string {
	head := `{"` + marker + `":"`
	tail := `"}`
	if size <= len(head)+len(tail) {
		panic("largeToolArguments size is too small")
	}
	return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
}

// TestResponsesParallelLargeToolArgumentsPartialCoverage reproduces the
// verifier's rv5e fixture: call A streams 3.6 MB and call B streams 700 KB
// plus a valid authoritative done value. Aggregate coverage crosses the 4 MiB
// bound, so B's retained prefix is truncated; translation must keep the
// streamed bytes instead of failing the valid done value.
func TestResponsesParallelLargeToolArgumentsPartialCoverage(t *testing.T) {
	const aSize = 3_600_000
	const bSize = 700_000
	aFull := largeToolArguments("a", aSize)
	bFull := largeToolArguments("b", bSize)

	var source strings.Builder
	source.WriteString(terminalStreamCreated)
	appendResponsesEvent(t, &source, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "write_a", "arguments": ""},
	})
	appendResponsesEvent(t, &source, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 1,
		"item": map[string]any{"type": "function_call", "id": "fc_b", "call_id": "call_b", "name": "write_b", "arguments": ""},
	})
	const chunk = 900_000
	for offset := 0; offset < len(aFull); offset += chunk {
		end := offset + chunk
		if end > len(aFull) {
			end = len(aFull)
		}
		appendResponsesEvent(t, &source, "response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "output_index": 0, "delta": aFull[offset:end],
		})
	}
	appendResponsesEvent(t, &source, "response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "output_index": 1, "delta": bFull,
	})
	appendResponsesEvent(t, &source, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "output_index": 1, "arguments": bFull,
	})
	appendResponsesEvent(t, &source, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 1,
		"item": map[string]any{"type": "function_call", "id": "fc_b", "call_id": "call_b", "name": "write_b", "arguments": ""},
	})
	appendResponsesEvent(t, &source, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "write_a", "arguments": ""},
	})
	appendResponsesEvent(t, &source, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_1", "model": "m", "status": "completed", "output": []any{},
		},
	})

	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source.String()), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("parallel large tool calls failed closed: %v", err)
			}
			if buf.Len() < aSize+bSize {
				t.Fatalf("translated output = %d bytes, want the streamed argument bytes", buf.Len())
			}
		})
	}
}

// TestResponsesToolArgumentsCompleteCoverageStillValidates keeps the other
// side of the gate: a complete retained prefix is still the result, so an
// invalid authoritative value must fail closed.
func TestResponsesToolArgumentsCompleteCoverageStillValidates(t *testing.T) {
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(
		responsesFunctionCallDoneStream("", `{\"q\":`, `{\"q\":1`)), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "tool arguments are not valid JSON") {
		t.Fatalf("invalid complete arguments error = %v", err)
	}
}
