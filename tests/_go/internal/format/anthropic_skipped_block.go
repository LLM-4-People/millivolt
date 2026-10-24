package format

// Round 2 TC3-7 regression: an unsupported provider block is skipped without
// announcing a target block, so closing open blocks must not emit a stop for
// an index the client never saw opened.

import (
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicSkippedBlockEmitsNoStop(t *testing.T) {
	source := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search"}}

event: message_stop
data: {"type":"message_stop"}

`
	dec := NewAnthropicStreamDecoder(strings.NewReader(source), nil)
	sawStart, sawDone := false, false
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Type {
		case ir.EventStart:
			sawStart = true
		case ir.EventBlockStart, ir.EventBlockEnd:
			t.Fatalf("skipped block produced %v: %+v", ev.Type, ev)
		case ir.EventDone:
			sawDone = true
		}
	}
	if !sawStart || !sawDone {
		t.Fatalf("start/done = %v/%v, want both", sawStart, sawDone)
	}
}
