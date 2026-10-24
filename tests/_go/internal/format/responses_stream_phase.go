package format

// Round 8 H3 regression: the Responses output phase is a wire hint with no
// cross-protocol equivalent. A streamed phase on an announced or terminal item
// was dropped silently; the decoder now records one diagnostic warning without
// adding anything to the client wire.

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesPhaseStream(addedPhase, terminalPhase string) string {
	added := `"type":"message","id":"msg_1","role":"assistant"`
	if addedPhase != "" {
		added += `,"phase":"` + addedPhase + `"`
	}
	added += `,"content":[]`
	terminal := `"type":"message","id":"msg_1","role":"assistant"`
	if terminalPhase != "" {
		terminal += `,"phase":"` + terminalPhase + `"`
	}
	terminal += `,"content":[{"type":"output_text","text":"hi"}]`
	return terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{` + added + `}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{` + terminal + `}]}}

`
}

func responsesPhaseWarningCount(t *testing.T, source string) int {
	t.Helper()
	dec := NewResponsesStreamDecoder(strings.NewReader(source))
	for {
		if _, err := dec.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	count := 0
	for _, warning := range dec.Warnings() {
		if warning.Field == "output.phase" {
			count++
		}
	}
	return count
}

func TestResponsesStreamOutputPhaseWarns(t *testing.T) {
	for _, tc := range []struct {
		name          string
		addedPhase    string
		terminalPhase string
		want          int
	}{
		{name: "announced phase", addedPhase: "commentary", want: 1},
		{name: "terminal phase", terminalPhase: "final_answer", want: 1},
		{name: "both phases", addedPhase: "commentary", terminalPhase: "final_answer", want: 1},
		{name: "no phase", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := responsesPhaseStream(tc.addedPhase, tc.terminalPhase)
			if got := responsesPhaseWarningCount(t, source); got != tc.want {
				t.Fatalf("output.phase warnings = %d, want %d", got, tc.want)
			}

			var out bytes.Buffer
			if err := TranslateStreamBetween(ir.Chat, ir.Responses, &out, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			wire := out.String()
			if strings.Contains(wire, "phase") || strings.Contains(wire, "commentary") ||
				strings.Contains(wire, "final_answer") {
				t.Fatalf("phase reached the client wire: %s", wire)
			}
			if !strings.Contains(wire, `"content":"hi"`) || !strings.Contains(wire, `"finish_reason":"stop"`) {
				t.Fatalf("client lost the answer or terminal: %s", wire)
			}
		})
	}
}
