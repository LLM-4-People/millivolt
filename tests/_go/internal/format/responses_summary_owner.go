package format

// Round 2 F6-1 regression: summary coverage is owned by the exact reasoning
// item id, not by a string prefix. An item id containing a colon must not
// absorb a longer sibling's summary bytes.

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalSummaryColonIDsExactOwner(t *testing.T) {
	const twoItems = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs:1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"alpha"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs:1","summary":[{"type":"summary_text","text":"alpha"}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs:1:extra","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":1,"summary_index":0,"part":{"type":"summary_text","text":"beta"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs:1:extra","summary":[{"type":"summary_text","text":"beta"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs:1","summary":[{"type":"summary_text","text":"alpha"}]},{"type":"reasoning","id":"rs:1:extra","summary":[{"type":"summary_text","text":"beta"}]}]}}

`
	t.Run("unchanged terminal records no partition warning", func(t *testing.T) {
		dec := NewResponsesStreamDecoder(strings.NewReader(terminalStreamCreated + twoItems))
		for {
			if _, err := dec.Next(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("Next: %v", err)
			}
		}
		for _, warning := range dec.Warnings() {
			if warning.Field == "summary_partition" {
				t.Fatalf("spurious summary warning: %+v", warning)
			}
		}
	})

	t.Run("longer first summary emits the tail once", func(t *testing.T) {
		source := terminalStreamCreated + twoItems
		source = strings.Replace(source,
			`{"type":"reasoning","id":"rs:1","summary":[{"type":"summary_text","text":"alpha"}]}`,
			`{"type":"reasoning","id":"rs:1","summary":[{"type":"summary_text","text":"alpha beta"}]}`, 1)
		for _, target := range terminalTargets {
			t.Run(target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := strings.Join(reasoningEmissionOrder(t, target.inbound, buf.String()), ""); got != "alpha betabeta" {
					t.Fatalf("reasoning = %q, want alpha betabeta: %s", got, buf.String())
				}
			})
		}
	})

	// A terminal document that never announced either item still carries both
	// summaries. The colon ids are distinct owners, so the merged text the
	// provider itself reported must appear once per item, not as a partition
	// duplicate.
	t.Run("terminal-only colon ids stay faithful", func(t *testing.T) {
		source := terminalStreamCreated + `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs:1","summary":[{"type":"summary_text","text":"alpha beta"}]},{"type":"reasoning","id":"rs:1:extra","summary":[{"type":"summary_text","text":"beta"}]}]}}

`
		for _, target := range terminalTargets {
			t.Run(target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := strings.Join(reasoningEmissionOrder(t, target.inbound, buf.String()), ""); got != "alpha betabeta" {
					t.Fatalf("reasoning = %q, want alpha betabeta: %s", got, buf.String())
				}
			})
		}
	})
}
