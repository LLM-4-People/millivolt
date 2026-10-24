package format

// Round 4 batch 8 regression (C3): a reasoning_text delta without the optional
// content_part.added announcement still reaches every target exactly once.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesReasoningNoPartStream(deltas []string, done string, terminalOutput string) string {
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

`)
	for _, delta := range deltas {
		fmt.Fprintf(&b, "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", delta)
	}
	if done != "" {
		fmt.Fprintf(&b, "event: response.reasoning_text.done\ndata: {\"type\":\"response.reasoning_text.done\",\"output_index\":0,\"content_index\":0,\"text\":%q}\n\n", done)
	}
	fmt.Fprintf(&b, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":%s}}\n\n", terminalOutput)
	return b.String()
}

func TestResponsesReasoningTextDeltaWithoutPartAnnouncement(t *testing.T) {
	source := responsesReasoningNoPartStream([]string{"step ", "one"}, "step one", `[]`)
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			if got := strings.Join(reasoningEmissionOrder(t, target.inbound, out), ""); got != "step one" {
				t.Fatalf("reasoning = %q, want step one exactly once: %s", got, out)
			}
			if got := textDeltaCount(t, target.inbound, out, "step "); got != 0 {
				t.Fatalf("reasoning leaked into visible text: %s", out)
			}
		})
	}
}

func TestResponsesReasoningPartAnnouncementStillReconciles(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":""}}

event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"step "}

event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"one"}

event: response.reasoning_text.done
data: {"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"step one"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reasoningEmissionOrder(t, ir.Anthropic, buf.String()), ""); got != "step one" {
		t.Fatalf("announced reasoning = %q, want step one exactly once: %s", got, buf.String())
	}
}

func TestResponsesReasoningPartKeyBoundStillApplies(t *testing.T) {
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

`)
	for i := 0; i <= 10000; i++ {
		fmt.Fprintf(&b, "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":%d,\"delta\":\"x\"}\n\n", i)
	}
	b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[]}}\n\n")

	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, strings.NewReader(b.String()), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "announced content parts") {
		t.Fatalf("part-key bound error = %v, want the 10000-part rejection", err)
	}
}
