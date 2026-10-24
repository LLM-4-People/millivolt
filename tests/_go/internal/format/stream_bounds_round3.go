package format

// Round 3 batch 1 regressions: a stream cannot reuse an Anthropic block index,
// retain an over-bound reasoning identity from a done payload, or grow summary
// part keys without the shared admission cap.

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicReusedIndexStream(pairs int) string {
	var b strings.Builder
	b.WriteString(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

`)
	for i := 0; i < pairs; i++ {
		b.WriteString(`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`)
	}
	b.WriteString(`event: message_stop
data: {"type":"message_stop"}

`)
	return b.String()
}

func TestAnthropicReusedBlockIndexRejected(t *testing.T) {
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(anthropicReusedIndexStream(10)), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "reused native content block index") {
		t.Fatalf("reused block index error = %v", err)
	}
	if buf.Len() > 1<<20 {
		t.Fatalf("rejected stream wrote %d bytes, want bounded output", buf.Len())
	}
}

func TestAnthropicMonotonicBlockIndexesTranslate(t *testing.T) {
	source := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"b"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_stop
data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(textEmissionOrder(t, ir.Chat, buf.String()), ""); got != "ab" {
		t.Fatalf("text = %q, want ab", got)
	}
}

func responsesReasoningDoneStream(id string) string {
	return terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"` + id + `","summary":[],"content":[{"type":"reasoning_text","text":"deep"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
}

func TestResponsesReasoningDoneHugeIDRejected(t *testing.T) {
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(
		responsesReasoningDoneStream(strings.Repeat("r", 513))), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "reasoning id") {
		t.Fatalf("huge reasoning done id error = %v", err)
	}

	buf.Reset()
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(
		responsesReasoningDoneStream(strings.Repeat("r", 512))), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("512-byte reasoning id rejected: %v", err)
	}
}

func TestResponsesStreamSummaryKeysBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(terminalStreamCreated)
	b.WriteString(`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

`)
	for i := 0; i < WireNestedCollectionMax+1; i++ {
		fmt.Fprintf(&b, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"summary_index\":%d,\"delta\":\"x\"}\n\n", i)
	}
	b.WriteString(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`)
	source := b.String()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var out bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Responses, &out, strings.NewReader(source), nil, nil, StreamOptions{})
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "announced content parts") {
		t.Fatalf("summary key bound error = %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Fatalf("summary key bound allocated %d bytes, want <64 MiB", alloc)
	}

	repeated := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"a"}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"b"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	var repeatedOut bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &repeatedOut, strings.NewReader(repeated), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reasoningEmissionOrder(t, ir.Chat, repeatedOut.String()), ""); got != "ab" {
		t.Fatalf("repeated summary deltas = %q, want ab", got)
	}
}
