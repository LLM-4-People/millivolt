package format

// Round 2 TC3-1/TC3-5 regressions: Responses deltas are incremental and must
// be appended verbatim, coverage must grow linearly, and aggregate retained
// text is bounded like the encoder budget.

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesIncrementalPrefixDeltaAppended(t *testing.T) {
	cases := []struct {
		name   string
		source string
		text   bool
	}{
		{
			name: "content",
			text: true,
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ab"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"a"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"c"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"abac"}]}]}}

`,
		},
		{
			name: "reasoning summary",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"ab"}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"a"}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"c"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"abac"}]}]}}

`,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf strings.Builder
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				var got string
				if tc.text {
					got = strings.Join(textEmissionOrder(t, target.inbound, buf.String()), "")
				} else {
					got = strings.Join(reasoningEmissionOrder(t, target.inbound, buf.String()), "")
				}
				if got != "abac" {
					t.Fatalf("emitted = %q, want abac: %s", got, buf.String())
				}
			})
		}
	}
}

func responsesManyDeltas(count int) string {
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

`)
	for i := 0; i < count; i++ {
		b.WriteString(`event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"wxyz"}

`)
	}
	b.WriteString(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`)
	return b.String()
}

func TestResponsesStreamCoverageLinearAllocation(t *testing.T) {
	const deltas = 20_000
	source := responsesManyDeltas(deltas)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	dec := NewResponsesStreamDecoder(strings.NewReader(source))
	for {
		_, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 64<<20 {
		t.Fatalf("%d incremental deltas allocated %d bytes, want under %d", deltas, allocated, 64<<20)
	}
}

func TestResponsesStreamRetainedTextCap(t *testing.T) {
	const block = 64 << 10
	count := ResponsesStreamRetainedMaxBytes/block + 1
	source := responsesManyDeltas(0)
	// Replace the empty tail with a large run of block-sized deltas so the
	// aggregate coverage crosses the retained-text bound.
	var b strings.Builder
	b.WriteString(source[:strings.LastIndex(source, "event: response.completed")])
	payload := strings.Repeat("x", block)
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", payload)
	}
	dec := NewResponsesStreamDecoder(strings.NewReader(b.String()))
	var err error
	for {
		_, err = dec.Next()
		if err != nil {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "retained text") {
		t.Fatalf("over-cap stream error = %v", err)
	}
}
