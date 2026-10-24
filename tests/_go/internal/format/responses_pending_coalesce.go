package format

// Round 6 C1 regression: repeated deltas for an unannounced part coalesce into
// one pending terminal emission keyed by part identity. Without the keyed
// owner every delta produced its own synthetic block, so the 8 MiB text bound
// still allowed millions of queued events.

import (
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func drainResponseEvents(t *testing.T, source string) []*ir.Event {
	t.Helper()
	dec := NewResponsesStreamDecoder(strings.NewReader(source))
	var events []*ir.Event
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return events
}

func unannouncedReasoningStream(parts []string) string {
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	for _, part := range parts {
		index, text, _ := strings.Cut(part, ":")
		b.WriteString(`event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":` + index + `,"delta":"` + text + `"}

`)
	}
	b.WriteString(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`)
	return b.String()
}

func TestResponsesUndeclaredReasoningDeltasCoalesce(t *testing.T) {
	parts := make([]string, 1000)
	for i := range parts {
		parts[i] = "0:x"
	}
	events := drainResponseEvents(t, unannouncedReasoningStream(parts))
	starts, deltasOut := 0, 0
	var reasoning strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case ir.EventBlockStart:
			starts++
		case ir.EventBlockDelta:
			if ev.Delta != nil && ev.Delta.Reasoning != "" {
				deltasOut++
				reasoning.WriteString(ev.Delta.Reasoning)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("synthetic block starts = %d, want 1 for one part", starts)
	}
	if deltasOut != 1 {
		t.Fatalf("reasoning deltas = %d, want one concatenated delta", deltasOut)
	}
	if got := reasoning.String(); got != strings.Repeat("x", 1000) {
		t.Fatalf("reasoning bytes = %d, want 1000", len(got))
	}
}

func TestResponsesUndeclaredReasoningPartsKeepOrder(t *testing.T) {
	events := drainResponseEvents(t, unannouncedReasoningStream([]string{"0:a", "1:b", "0:c", "1:d"}))
	var starts []int
	reasoning := map[int]string{}
	current := -1
	for _, ev := range events {
		switch ev.Type {
		case ir.EventBlockStart:
			starts = append(starts, ev.Index)
			current = ev.Index
		case ir.EventBlockDelta:
			if ev.Delta != nil && ev.Delta.Reasoning != "" {
				reasoning[current] += ev.Delta.Reasoning
			}
		}
	}
	if len(starts) != 2 {
		t.Fatalf("synthetic block starts = %d, want 2", len(starts))
	}
	if got := reasoning[starts[0]] + reasoning[starts[1]]; got != "acbd" {
		t.Fatalf("reasoning order = %q, want acbd", got)
	}
}
