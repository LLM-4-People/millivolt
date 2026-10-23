package format

// C7/N11/N13 regressions: a target encoder that fails mid-stream must close
// the blocks, items and content parts it actually opened, without flushing
// payload its own retention budget refused. Chat has no close frame but must
// leave only valid chunks behind.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesEncoderRetentionFailureAbortsTarget(t *testing.T) {
	chunk := strings.Repeat("a", 900<<10)
	var src strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&src, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`+"\n\n", chunk)
	}
	src.WriteString("data: [DONE]\n\n")

	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Responses, ir.Chat, &buf, strings.NewReader(src.String()), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "retained bytes") {
		t.Fatalf("error = %v, want the Responses retention bound", err)
	}
	out := buf.String()
	if strings.Contains(out, "event: response.completed") {
		t.Fatalf("a failed translation emitted a clean terminal frame: %s", out)
	}
	partDone := strings.Index(out, "event: response.content_part.done")
	itemDone := strings.Index(out, "event: response.output_item.done")
	if partDone < 0 || itemDone < 0 {
		t.Fatalf("target blocks were left open: content_part.done=%d output_item.done=%d", partDone, itemDone)
	}
	if partDone > itemDone {
		t.Fatalf("content part closed after its item: part=%d item=%d", partDone, itemDone)
	}
	frames := parseWireFrames(t, out)
	for _, frame := range frames {
		switch frame.event {
		case "response.content_part.done":
			part, _ := frame.data["part"].(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				t.Fatalf("aborted part flushed retained text (%d bytes)", len(text))
			}
		case "response.output_item.done":
			item, _ := frame.data["item"].(map[string]any)
			if item["status"] != "incomplete" {
				t.Fatalf("aborted item status = %v", item["status"])
			}
		}
	}
}

func TestResponsesEncoderFrameCapAbortsOpenPart(t *testing.T) {
	var buf bytes.Buffer
	enc := NewResponsesStreamEncoder(&buf, nil)
	for _, ev := range []*ir.Event{
		{Type: ir.EventStart, Response: &ir.Response{ID: "resp_1", Model: "m", Created: 1}},
		{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EventBlockDelta, Index: 0, Delta: &ir.Delta{Text: "hello"}},
	} {
		if err := enc.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	err := enc.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "delta": strings.Repeat("a", ResponsesStreamFrameMaxBytes),
	})
	if err == nil || !strings.Contains(err.Error(), "frame exceeds") {
		t.Fatalf("emit error = %v, want the frame cap", err)
	}
	if err := enc.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "event: response.completed") {
		t.Fatalf("a failed encoder emitted a clean terminal frame: %s", out)
	}
	partDone := strings.Index(out, "event: response.content_part.done")
	itemDone := strings.Index(out, "event: response.output_item.done")
	if partDone < 0 || itemDone < 0 || partDone > itemDone {
		t.Fatalf("content part was not closed before the item: %s", out)
	}
	if !strings.Contains(out, `"text":""`) || !strings.Contains(out, `"status":"incomplete"`) {
		t.Fatalf("abort flushed retained payload or missed the incomplete status: %s", out)
	}
}

func TestAnthropicAbortClosesOnlyOpenedBlocks(t *testing.T) {
	var buf bytes.Buffer
	enc := NewAnthropicStreamEncoder(&buf, nil)
	for _, ev := range []*ir.Event{
		{Type: ir.EventStart, Response: &ir.Response{ID: "m1", Model: "m", Created: 1}},
		{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EventBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.test/i.png"}}},
		{Type: ir.EventBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
			ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":1}`),
		}}},
	} {
		if err := enc.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	frames := parseWireFrames(t, buf.String())
	var stops []int
	for _, frame := range frames {
		switch frame.event {
		case "content_block_stop":
			stops = append(stops, intField(t, frame, "index"))
		case "content_block_delta":
			t.Fatalf("abort flushed retained payload: %v", frame.data)
		case "message_delta", "message_stop":
			t.Fatalf("abort emitted a terminal frame: %v", frame.data)
		}
	}
	if len(stops) != 2 || stops[0] != 0 || stops[1] != 2 {
		t.Fatalf("closed block indices = %v, want only the opened blocks [0 2]", stops)
	}
}

func TestChatEncoderAbortKeepsValidChunks(t *testing.T) {
	var buf bytes.Buffer
	enc := NewChatStreamEncoder(&buf, nil)
	for _, ev := range []*ir.Event{
		{Type: ir.EventStart, Response: &ir.Response{ID: "c1", Model: "m", Created: 1}},
		{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EventBlockDelta, Index: 0, Delta: &ir.Delta{Text: "hello"}},
	} {
		if err := enc.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("abort emitted a clean terminal frame: %s", out)
	}
	for _, frame := range strings.Split(strings.TrimSpace(out), "\n\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok {
			t.Fatalf("frame is not a data frame: %q", frame)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("aborted stream left invalid JSON %q: %v", payload, err)
		}
	}
	if !strings.Contains(out, `"content":"hello"`) {
		t.Fatalf("content written before the abort was lost: %s", out)
	}
}
