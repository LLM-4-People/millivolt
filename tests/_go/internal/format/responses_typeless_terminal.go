package format

// Round 2 TC3-3 regression: a translated Responses terminal whose data omits
// the type is classified by its SSE event name, not treated as debris.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTypeLessTerminalTranslates(t *testing.T) {
	source := terminalStreamCreated + `event: response.completed
data: {"response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v (%s)", err, buf.String())
			}
			if got := strings.Join(textEmissionOrder(t, target.inbound, buf.String()), ""); got != "hello" {
				t.Fatalf("text = %q, want hello: %s", got, buf.String())
			}
		})
	}
}
