package format

// Batch 7 regressions: the Responses encoder keeps one aggregate retained
// budget so its terminal frame stays inside the relay scanner, provider
// identity cannot amplify a frame, and the Chat encoder releases per-block
// state when a block closes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesStreamAggregateRetention(t *testing.T) {
	var buf bytes.Buffer
	enc := NewResponsesStreamEncoder(&buf, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{ID: "r1", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("a", (ResponsesStreamRetainedMaxBytes/5)+1)
	for i := 0; i < 5; i++ {
		if err := enc.Write(&ir.Event{Type: ir.EventBlockStart, Index: i, Block: &ir.Block{Type: ir.BlockText}}); err != nil {
			t.Fatalf("block %d start: %v", i, err)
		}
		err := enc.Write(&ir.Event{Type: ir.EventBlockDelta, Index: i, Delta: &ir.Delta{Text: chunk}})
		if i < 4 {
			if err != nil {
				t.Fatalf("block %d delta within the aggregate budget: %v", i, err)
			}
			if err := enc.Write(&ir.Event{Type: ir.EventBlockEnd, Index: i}); err != nil {
				t.Fatalf("block %d end: %v", i, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "retained bytes") {
			t.Fatalf("over-budget delta error = %v, want the aggregate retention rejection", err)
		}
	}
	if strings.Contains(buf.String(), "response.completed") {
		t.Fatal("an over-budget stream still wrote a terminal frame")
	}
}

func TestResponsesStreamControlByteEscapeWithinRelayCap(t *testing.T) {
	var buf bytes.Buffer
	enc := NewResponsesStreamEncoder(&buf, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{ID: "r1", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	control := strings.Repeat("\x01", ResponsesStreamRetainedMaxBytes-responsesStreamItemEnvelopeBytes-8)
	if err := enc.Write(&ir.Event{Type: ir.EventBlockDelta, Index: 0, Delta: &ir.Delta{Text: control}}); err != nil {
		t.Fatalf("control-byte delta at the retained budget rejected: %v", err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventBlockEnd, Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	maxFrame := 0
	for _, frame := range strings.Split(buf.String(), "\n\n") {
		if len(frame) > maxFrame {
			maxFrame = len(frame)
		}
	}
	if maxFrame > ResponsesStreamFrameMaxBytes {
		t.Fatalf("largest frame = %d bytes, want <= ResponsesStreamFrameMaxBytes (%d)",
			maxFrame, ResponsesStreamFrameMaxBytes)
	}
}

func TestTranslatedHugeIdentityRejected(t *testing.T) {
	huge := strings.Repeat("x", nativeIdentityMaxBytes+1)
	t.Run("chat response id", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"id": huge, "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
		})
		if _, err := DecodeChatResponse(body); err == nil {
			t.Fatal("over-long Chat response id accepted")
		}
	})
	t.Run("chat stream model", func(t *testing.T) {
		line := `data: {"id":"c1","model":"` + huge + `","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"
		if err := drainChatStream(strings.NewReader(line)); err == nil {
			t.Fatal("over-long Chat stream model accepted")
		}
	})
	t.Run("chat tool name", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"id": "c1", "model": "m",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": huge, "arguments": "{}"}},
				}},
			}},
		})
		if _, err := DecodeChatResponse(body); err == nil {
			t.Fatal("over-long Chat tool name accepted")
		}
	})
	t.Run("responses response id", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"id": huge, "model": "m", "status": "completed", "output": []any{}})
		if _, err := DecodeResponsesResponse(body); err == nil {
			t.Fatal("over-long Responses id accepted")
		}
	})
	t.Run("responses stream id", func(t *testing.T) {
		line := `data: {"type":"response.created","response":{"id":"` + huge + `","model":"m","status":"in_progress"}}` + "\n\n"
		if err := drainResponsesStream(strings.NewReader(line)); err == nil {
			t.Fatal("over-long Responses stream id accepted")
		}
	})
}

func drainChatStream(r *strings.Reader) error {
	dec := NewChatStreamDecoder(r)
	for {
		if _, err := dec.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func drainResponsesStream(r *strings.Reader) error {
	dec := NewResponsesStreamDecoder(r)
	for {
		if _, err := dec.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func TestChatEncoderReleasesClosedBlockState(t *testing.T) {
	var buf bytes.Buffer
	enc := NewChatStreamEncoder(&buf, nil)
	for i := 0; i < 50; i++ {
		if err := enc.Write(&ir.Event{Type: ir.EventBlockStart, Index: i, Block: &ir.Block{
			Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: "c", Name: "f", Arguments: json.RawMessage(`{"q":1}`)},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Write(&ir.Event{Type: ir.EventBlockEnd, Index: i}); err != nil {
			t.Fatal(err)
		}
		if len(enc.toolIndex) != 0 || len(enc.blockTypes) != 0 || len(enc.blockArgs) != 0 || len(enc.argsEmitted) != 0 {
			t.Fatalf("block %d left encoder state: toolIndex=%d blockTypes=%d blockArgs=%d argsEmitted=%d",
				i, len(enc.toolIndex), len(enc.blockTypes), len(enc.blockArgs), len(enc.argsEmitted))
		}
	}
}
