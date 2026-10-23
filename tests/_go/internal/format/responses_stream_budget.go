package format

// Batch 18 regression: the Responses terminal document must stay inside the
// relay frame bound even when the source opens many empty items. Per-item
// envelopes and provider identity are part of the aggregate retention, not
// free bytes.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesStreamItemBudgetFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		idLen   int
		limit   int
		errPart string
	}{
		{"item count", 8, WireCollectionMax + 1, "items"},
		{"identity amplification", 500, 45000, "retained bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewResponsesStreamEncoder(&buf, nil)
			if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{ID: "resp_1", Model: "m", Created: 1}}); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("c", tc.idLen)
			name := strings.Repeat("n", tc.idLen)
			var failure error
			for i := 0; i < tc.limit; i++ {
				start := &ir.Event{Type: ir.EventBlockStart, Index: i, Block: &ir.Block{
					Type: ir.BlockToolCall,
					ToolCall: &ir.ToolCall{
						ID: id, Name: name, Arguments: []byte(`{}`),
					},
				}}
				if err := enc.Write(start); err != nil {
					failure = err
					break
				}
				if err := enc.Write(&ir.Event{Type: ir.EventBlockEnd, Index: i}); err != nil {
					failure = err
					break
				}
			}
			if failure == nil {
				t.Fatalf("%d items were accepted without a bound failure", tc.limit)
			}
			if !strings.Contains(failure.Error(), tc.errPart) {
				t.Fatalf("failure = %v, want it to name %q", failure, tc.errPart)
			}
			if strings.Contains(buf.String(), "event: response.completed") {
				t.Fatal("a failed stream emitted a clean terminal frame")
			}
			for _, frame := range strings.Split(buf.String(), "\n\n") {
				if len(frame) > ResponsesStreamFrameMaxBytes {
					t.Fatalf("frame = %d bytes, want <= %d", len(frame), ResponsesStreamFrameMaxBytes)
				}
			}
		})
	}
}
