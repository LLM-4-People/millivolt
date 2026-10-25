package format

// Round 13 C1 regression: a reasoning item's summary and content channels are
// independent even when their part indexes collide. A streamed reasoning_text
// content part at index 0 must not suppress a terminal summary at summary
// index 0.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func translateSummaryStream(t *testing.T, target ir.Protocol, source string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := TranslateStreamBetween(target, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("%s translation: %v", target, err)
	}
	return buf.String()
}

func TestResponsesSummaryChannelCoverage(t *testing.T) {
	const addedReasoning = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

`
	const bothTerminal = `event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"world"}],"content":[{"type":"reasoning_text","text":"hello world"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"world"}],"content":[{"type":"reasoning_text","text":"hello world"}]}]}}

`
	contentThenSummary := addedReasoning + `event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"hello world"}

` + bothTerminal

	streamedSummaryTail := addedReasoning + `event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"alpha"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha beta"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha beta"}]}]}}

`
	summaryOnly := addedReasoning + `event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"only summary"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"only summary"}]}]}}

`

	for _, target := range []ir.Protocol{ir.Responses, ir.Chat, ir.Anthropic} {
		t.Run(string(target), func(t *testing.T) {
			out := translateSummaryStream(t, target, contentThenSummary)
			if got := reasoningDeltaCount(t, target, out, "hello world"); got != 1 {
				t.Fatalf("content channel count = %d, want 1: %s", got, out)
			}
			if got := reasoningDeltaCount(t, target, out, "world"); got != 1 {
				t.Fatalf("summary channel count = %d, want 1: %s", got, out)
			}
			order := reasoningEmissionOrder(t, target, out)
			if strings.Join(order, "") != "hello worldworld" {
				t.Fatalf("reasoning emission order = %q, want content then summary", order)
			}

			tail := translateSummaryStream(t, target, streamedSummaryTail)
			if got := strings.Join(reasoningEmissionOrder(t, target, tail), ""); got != "alpha beta" {
				t.Fatalf("streamed summary tail = %q, want alpha beta: %s", got, tail)
			}

			only := translateSummaryStream(t, target, summaryOnly)
			if got := strings.Join(reasoningEmissionOrder(t, target, only), ""); got != "only summary" {
				t.Fatalf("summary-only emissions = %q, want exactly one copy: %s", got, only)
			}
		})
	}
}
