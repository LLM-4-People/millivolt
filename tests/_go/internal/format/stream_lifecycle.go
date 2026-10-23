package format

// Batch 1 regressions: a translated stream must open each client block exactly
// once and close it exactly once before any terminal or error event, for every
// source/target combination and on every abnormal termination path.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// anthropicLifecycle records the ordered Anthropic event types and the
// content_block_start/stop indexes of a translated stream.
type anthropicLifecycle struct {
	starts []int
	stops  []int
	events []string
}

func parseAnthropicLifecycle(t *testing.T, out string) anthropicLifecycle {
	t.Helper()
	var got anthropicLifecycle
	for _, frame := range strings.Split(out, "\n\n") {
		data, ok := "", false
		for _, line := range strings.Split(frame, "\n") {
			if payload, found := strings.CutPrefix(strings.TrimSpace(line), "data: "); found {
				data, ok = payload, true
				break
			}
		}
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		if ev.Type == "" {
			continue
		}
		got.events = append(got.events, ev.Type)
		switch ev.Type {
		case "content_block_start":
			got.starts = append(got.starts, ev.Index)
		case "content_block_stop":
			got.stops = append(got.stops, ev.Index)
		}
	}
	return got
}

// assertSingleLifecycle checks that every started index is stopped exactly once
// and that no stop refers to an index that never started.
func assertSingleLifecycle(t *testing.T, got anthropicLifecycle) {
	t.Helper()
	seenStart := map[int]bool{}
	stopped := map[int]int{}
	for _, idx := range got.starts {
		if seenStart[idx] {
			t.Fatalf("duplicate content_block_start for index %d: %v", idx, got.events)
		}
		seenStart[idx] = true
	}
	for _, idx := range got.stops {
		if !seenStart[idx] {
			t.Fatalf("content_block_stop for index %d before its start: %v", idx, got.events)
		}
		stopped[idx]++
		if stopped[idx] > 1 {
			t.Fatalf("duplicate content_block_stop for index %d: %v", idx, got.events)
		}
	}
	for idx := range seenStart {
		if stopped[idx] != 1 {
			t.Fatalf("index %d has %d stops, want 1: %v", idx, stopped[idx], got.events)
		}
	}
}

// TestChatStreamToAnthropicClosesEachIndexOnce pins C1: closeAll closes
// text/reasoning/refusal indexes through the helpers, and the remaining open
// set must not close them a second time.
func TestChatStreamToAnthropicClosesEachIndexOnce(t *testing.T) {
	in := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think","content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var buf bytes.Buffer
	collectStream(t, NewChatStreamDecoder(strings.NewReader(in)), NewAnthropicStreamEncoder(&buf, nil))
	got := parseAnthropicLifecycle(t, buf.String())
	if len(got.starts) != 2 {
		t.Fatalf("starts = %v, want two blocks: %s", got.starts, buf.String())
	}
	assertSingleLifecycle(t, got)
}

// TestResponsesUnsupportedPartDoesNotEmitStrayStop pins C5: an unsupported
// content part must not consume a block index, so the accepted part starts at 0
// and no stop is emitted for an index the client never saw start.
func TestResponsesUnsupportedPartDoesNotEmitStrayStop(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":"hello"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":" world"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[]}}

`
	var buf bytes.Buffer
	collectStream(t, NewResponsesStreamDecoder(strings.NewReader(in)), NewAnthropicStreamEncoder(&buf, nil))
	got := parseAnthropicLifecycle(t, buf.String())
	if len(got.starts) != 1 || got.starts[0] != 0 {
		t.Fatalf("starts = %v, want [0] (unsupported part must not consume an index): %s", got.starts, buf.String())
	}
	assertSingleLifecycle(t, got)
	if !strings.Contains(buf.String(), `"text":" world"`) {
		t.Fatalf("accepted text part did not reach the client: %s", buf.String())
	}
}

// TestResponsesReasoningTextDeltaUsesPartBlock pins C12: reasoning_text is a
// content part, so its delta must land on the part block rather than the item
// block that holds the reasoning summary.
func TestResponsesReasoningTextDeltaUsesPartBlock(t *testing.T) {
	in := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":""}}

event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"step one"}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":"step one"}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[]}}

`
	dec := NewResponsesStreamDecoder(strings.NewReader(in))
	itemStart := -1
	partStart := -1
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		switch ev.Type {
		case ir.EventBlockStart:
			if ev.Block != nil && ev.Block.Type == ir.BlockReasoning && ev.Index != 0 {
				partStart = ev.Index
			}
			if ev.Index == 0 {
				itemStart = ev.Index
			}
		case ir.EventBlockDelta:
			if ev.Delta != nil && ev.Delta.Reasoning == "step one" {
				if partStart < 0 || ev.Index != partStart {
					t.Fatalf("reasoning_text delta index = %d, want the part block %d (item block %d)",
						ev.Index, partStart, itemStart)
				}
				return
			}
		}
	}
	t.Fatal("reasoning_text delta never reached a block")
}

// TestTranslatedTruncationClosesOpenBlocks pins C7: when the upstream ends
// without a terminal event, every block that started is stopped before the
// in-band error and the target's terminal frame.
func TestTranslatedTruncationClosesOpenBlocks(t *testing.T) {
	chatIn := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}

`
	responsesIn := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hi"}}

`
	anthropicIn := `data: {"type":"message_start","message":{"id":"m1","model":"m"}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

`
	cases := []struct {
		name     string
		inbound  ir.Protocol
		outbound ir.Protocol
		in       string
	}{
		{"chat upstream to anthropic client", ir.Anthropic, ir.Chat, chatIn},
		{"responses upstream to anthropic client", ir.Anthropic, ir.Responses, responsesIn},
		{"anthropic upstream to responses client", ir.Responses, ir.Anthropic, anthropicIn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(tc.inbound, tc.outbound, &buf, strings.NewReader(tc.in), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween = %v", err)
			}
			if tc.inbound == ir.Anthropic {
				got := parseAnthropicLifecycle(t, buf.String())
				if len(got.starts) == 0 {
					t.Fatalf("no block started: %s", buf.String())
				}
				assertSingleLifecycle(t, got)
				firstErr := -1
				for i, name := range got.events {
					if name == "error" {
						firstErr = i
						break
					}
				}
				if firstErr < 0 {
					t.Fatalf("truncated Anthropic stream has no error event: %s", buf.String())
				}
				for i, name := range got.events {
					if name == "content_block_stop" && i > firstErr {
						t.Fatalf("block stop after the error event: %v", got.events)
					}
				}
			} else {
				if !strings.Contains(buf.String(), "event: response.failed") {
					t.Fatalf("truncated Responses stream has no response.failed: %s", buf.String())
				}
				if strings.Contains(buf.String(), "data: [DONE]") {
					t.Fatalf("Responses stream leaked a Chat sentinel: %s", buf.String())
				}
			}
		})
	}
}

// TestHardDecoderErrorClosesBlocks pins C7: a malformed source chunk after a
// block opened must close that block for the client, but the fail-closed flush
// must not emit retained payload or a clean terminal frame.
func TestHardDecoderErrorClosesBlocks(t *testing.T) {
	cases := []struct {
		name     string
		inbound  ir.Protocol
		outbound ir.Protocol
		in       string
	}{
		{
			name:     "chat source to anthropic client",
			inbound:  ir.Anthropic,
			outbound: ir.Chat,
			in: `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: not-json

`,
		},
		{
			name:     "responses source to anthropic client",
			inbound:  ir.Anthropic,
			outbound: ir.Responses,
			in: `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}

event: response.output_text.delta
data: not-json

`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(tc.inbound, tc.outbound, &buf, strings.NewReader(tc.in), nil, nil)
			if err == nil {
				t.Fatal("TranslateStreamBetween returned nil for a malformed source")
			}
			got := parseAnthropicLifecycle(t, buf.String())
			if len(got.starts) == 0 {
				t.Fatalf("no block started before the failure: %s", buf.String())
			}
			assertSingleLifecycle(t, got)
			if strings.Contains(buf.String(), "event: message_stop") {
				t.Fatalf("hard failure emitted a clean terminal frame: %s", buf.String())
			}
		})
	}

	t.Run("anthropic source to responses client", func(t *testing.T) {
		in := `data: {"type":"message_start","message":{"id":"m1","model":"m"}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"secret"}}

data: not-json

`
		var buf bytes.Buffer
		if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(in), nil, nil); err == nil {
			t.Fatal("TranslateStreamBetween returned nil for a malformed source")
		}
		out := buf.String()
		if strings.Contains(out, "event: response.completed") {
			t.Fatalf("hard failure emitted a clean terminal frame: %s", out)
		}
		frames := parseWireFrames(t, out)
		done := -1
		for i, frame := range frames {
			if frame.event != "response.output_item.done" {
				continue
			}
			done = i
			item, _ := frame.data["item"].(map[string]any)
			if item["status"] != "incomplete" {
				t.Fatalf("aborted item status = %v: %v", item["status"], frame.data)
			}
			if content, _ := item["content"].([]any); len(content) != 0 {
				t.Fatalf("aborted item flushed retained content: %v", content)
			}
		}
		if done < 0 {
			t.Fatalf("no output_item.done for the aborted block: %s", out)
		}
	})
}
