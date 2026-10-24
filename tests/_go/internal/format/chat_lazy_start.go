package format

// Round 4 batch 8 regression (C5): a content-bearing stream without its start
// event still opens a valid Chat stream with one role chunk, and a bare
// [DONE] stream keeps its content-less shape.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicNoStartStream = `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func TestChatStreamLazyStartForContentWithoutStart(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(anthropicNoStartStream), nil, nil, StreamOptions{FallbackModel: "request-model"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	payloads := chatPayloads(t, out)
	if len(payloads) == 0 {
		t.Fatalf("no Chat chunks: %s", out)
	}
	id := ""
	for i, payload := range payloads {
		if payload["id"] == "" || !strings.HasPrefix(payload["id"].(string), "chatcmpl-") {
			t.Fatalf("chunk %d has no synthesized id: %s", i, out)
		}
		if payload["model"] != "request-model" {
			t.Fatalf("chunk %d model = %v, want the fallback model: %s", i, payload["model"], out)
		}
		if payload["created"] == float64(0) {
			t.Fatalf("chunk %d has no created stamp: %s", i, out)
		}
		if id == "" {
			id = payload["id"].(string)
			choices, _ := payload["choices"].([]any)
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			if delta["role"] != "assistant" {
				t.Fatalf("first chunk carries no role delta: %s", out)
			}
		} else if payload["id"] != id {
			t.Fatalf("chunk identity changed: %s", out)
		}
	}
	if strings.Count(out, `"role":"assistant"`) != 1 {
		t.Fatalf("role chunk emitted more than once: %s", out)
	}
}

func TestChatStreamLateStartCannotRewriteLazyIdentity(t *testing.T) {
	stream := anthropicNoStartStream
	late := `event: message_start
data: {"type":"message_start","message":{"id":"msg_b","model":"m2"}}

`
	stream = strings.Replace(stream, "event: message_delta", late+"event: message_delta", 1)
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(stream), nil, nil, StreamOptions{FallbackModel: "request-model"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "msg_b") || strings.Contains(out, `"model":"m2"`) {
		t.Fatalf("late start rewrote the lazy identity: %s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("lazy start emitted no role chunk: %s", out)
	}
}

func TestChatStreamBareDoneStaysBare(t *testing.T) {
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader("data: [DONE]\n\n"), nil, nil, StreamOptions{FallbackModel: "m"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, `"role"`) || strings.Contains(out, `"choices"`) {
		t.Fatalf("content-less [DONE] stream fabricated a chunk: %s", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("bare [DONE] shape changed: %q", out)
	}
}
