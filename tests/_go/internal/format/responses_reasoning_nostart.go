package format

// Round 4 batch 8 regression (C3) plus round 7 NEW-1: a reasoning_text delta is
// incremental and reaches every target verbatim, with or without the optional
// content_part.added announcement and without a done payload masking a loss.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// responsesReasoningTextStream builds a Responses reasoning stream. announced
// adds the optional content_part.added with startText; done, when non-empty,
// adds the authoritative done payload.
func responsesReasoningTextStream(announced bool, startText string, deltas []string, done string) string {
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

`)
	if announced {
		fmt.Fprintf(&b, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"reasoning_text\",\"text\":%q}}\n\n", startText)
	}
	for _, delta := range deltas {
		fmt.Fprintf(&b, "event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", delta)
	}
	if done != "" {
		fmt.Fprintf(&b, "event: response.reasoning_text.done\ndata: {\"type\":\"response.reasoning_text.done\",\"output_index\":0,\"content_index\":0,\"text\":%q}\n\n", done)
	}
	b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[]}}\n\n")
	return b.String()
}

func TestResponsesReasoningTextIncrementalVariants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		announced bool
		startText string
		deltas    []string
		want      string
	}{
		{
			name:   "unannounced two deltas",
			deltas: []string{"step ", "one"},
			want:   "step one",
		},
		{
			name:      "announced empty start two deltas",
			announced: true,
			deltas:    []string{"step ", "one"},
			want:      "step one",
		},
		{
			name:      "announced non-empty start plus delta",
			announced: true,
			startText: "step",
			deltas:    []string{" one"},
			want:      "step one",
		},
		{
			name:   "repeated incremental delta is new text",
			deltas: []string{"x", "x"},
			want:   "xx",
		},
	} {
		source := responsesReasoningTextStream(tc.announced, tc.startText, tc.deltas, "")
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
					t.Fatal(err)
				}
				out := buf.String()
				if got := strings.Join(reasoningEmissionOrder(t, target.inbound, out), ""); got != tc.want {
					t.Fatalf("reasoning = %q, want %q: %s", got, tc.want, out)
				}
				if got := textDeltaCount(t, target.inbound, out, "step "); got != 0 {
					t.Fatalf("reasoning leaked into visible text: %s", out)
				}
			})
		}
	}
}

func TestResponsesReasoningPartAnnouncementStillReconciles(t *testing.T) {
	source := responsesReasoningTextStream(true, "", []string{"step ", "one"}, "step one")
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
