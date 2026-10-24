package format

// Round 4 batch 8 regression (C4): a duplicate message_start never rewrites
// the stream identity already committed to the wire.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicDuplicateStartStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_a","model":"m","usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_start
data: {"type":"message_start","message":{"id":"msg_b","model":"m2","usage":{"output_tokens":5}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicDuplicateMessageStartKeepsFirstIdentity(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		var buf bytes.Buffer
		if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(anthropicDuplicateStartStream), nil, nil, StreamOptions{IncludeUsage: true}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, "msg_b") || strings.Contains(out, "m2") {
			t.Fatalf("duplicate start rewrote the stream identity: %s", out)
		}
		payloads := chatPayloads(t, out)
		sawUsage := false
		for _, payload := range payloads {
			if payload["id"] != "msg_a" || payload["model"] != "m" {
				t.Fatalf("chunk identity = %v/%v, want msg_a/m: %s", payload["id"], payload["model"], out)
			}
			usage, _ := payload["usage"].(map[string]any)
			if usage != nil {
				sawUsage = true
				if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
					t.Fatalf("merged usage = %v, want 10/5: %s", usage, out)
				}
			}
		}
		if !sawUsage {
			t.Fatalf("no merged usage chunk: %s", out)
		}
	})

	t.Run("responses", func(t *testing.T) {
		var buf bytes.Buffer
		if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(anthropicDuplicateStartStream), nil, nil, StreamOptions{}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, "msg_b") || strings.Contains(out, "m2") {
			t.Fatalf("duplicate start rewrote the stream identity: %s", out)
		}
		var created, completed bool
		for _, payload := range mustResponsePayloads(t, out) {
			response, _ := payload["response"].(map[string]any)
			switch payload["type"] {
			case "response.created":
				created = true
				if response["id"] != "msg_a" || response["model"] != "m" {
					t.Fatalf("response.created identity = %v/%v", response["id"], response["model"])
				}
			case "response.completed":
				completed = true
				if response["id"] != "msg_a" || response["model"] != "m" {
					t.Fatalf("terminal identity = %v/%v", response["id"], response["model"])
				}
			}
		}
		if !created || !completed {
			t.Fatalf("missing start or terminal (created=%v completed=%v): %s", created, completed, out)
		}
	})
}
