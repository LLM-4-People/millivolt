package format

// Round 2 B5 regression: a terminal-only message sibling that reuses an
// announced item id must be synthesized at its own terminal position. The
// both-announced shape already worked; this pins the sibling that never had an
// announcement of its own.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalOnlyMessageSiblingIsSynthesized(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_shared","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"alpha"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_shared","role":"assistant","content":[{"type":"output_text","text":"alpha"}]},{"type":"message","id":"msg_shared","role":"assistant","content":[{"type":"output_text","text":"beta"}]}]}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("TranslateStreamBetween: %v", err)
	}
	if got := strings.Join(textEmissionOrder(t, ir.Anthropic, buf.String()), ""); got != "alphabeta" {
		t.Fatalf("text = %q, want alphabeta: %s", got, buf.String())
	}
	for _, value := range []string{"alpha", "beta"} {
		if got := textDeltaCount(t, ir.Anthropic, buf.String(), value); got != 1 {
			t.Fatalf("%q emitted %d times, want once: %s", value, got, buf.String())
		}
	}
}

func TestResponsesBothAnnouncedMessageSiblingsStayDistinct(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf,
		strings.NewReader(responsesSiblingSharedMessageIDStream), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("TranslateStreamBetween: %v", err)
	}
	if got := strings.Join(textEmissionOrder(t, ir.Anthropic, buf.String()), ""); got != "alphabeta" {
		t.Fatalf("text = %q, want alphabeta: %s", got, buf.String())
	}
}
