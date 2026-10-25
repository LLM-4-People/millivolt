package format

// Round 2 B6 regression: a terminal Responses frame may repeat a one MiB
// answer. The source line is slightly larger than the answer, so the SSE line
// bound must admit it while the overall retained-response bounds stay in place.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesOneMiBTerminalAnswerParses(t *testing.T) {
	answer := strings.Repeat("a", 1<<20)
	source := terminalStreamCreated + `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"` +
		answer + `"}]}]}}

`
	var out bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &out, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("one MiB terminal answer failed: %v", err)
	}
	if got := strings.Count(out.String(), answer); got != 1 {
		t.Fatalf("one MiB answer emitted %d times, want once (output %d bytes)", got, out.Len())
	}
}
