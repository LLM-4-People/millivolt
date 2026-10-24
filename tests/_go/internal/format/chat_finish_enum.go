package format

// Round 3 batch 3 regressions (B1/C2): the Chat target wire only accepts its
// documented finish_reason enum, while the canonical record keeps the raw
// source reason.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicStreamFinishReasonIsChatEnum(t *testing.T) {
	for reason, want := range map[string]string{
		"end_turn":   "stop",
		"max_tokens": "length",
		"tool_use":   "tool_calls",
		"pause_turn": "stop",
		"refusal":    "content_filter",
	} {
		source := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
		var buf bytes.Buffer
		if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		finishes := 0
		for _, payload := range chatPayloads(t, buf.String()) {
			choices, _ := payload["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			reasonValue, present := choices[0].(map[string]any)["finish_reason"]
			if !present || reasonValue == nil {
				continue
			}
			got, _ := reasonValue.(string)
			if got != want {
				t.Fatalf("%s -> finish_reason %q, want %q", reason, got, want)
			}
			finishes++
		}
		if finishes != 1 {
			t.Fatalf("%s emitted %d finish reasons, want 1: %s", reason, finishes, buf.String())
		}
	}
}

func TestChatFinishReasonAlwaysEnum(t *testing.T) {
	valid := map[string]bool{
		"stop": true, "length": true, "tool_calls": true,
		"content_filter": true, "function_call": true,
	}
	for _, reason := range []ir.FinishReason{
		ir.FinishStop, ir.FinishLength, ir.FinishToolCalls, ir.FinishContentFilter,
		ir.FinishPause, ir.FinishCancelled, ir.FinishError, ir.FinishOther,
		"pause_turn", "refusal", "provider_unknown",
	} {
		got := ChatFinishReason(reason)
		if !valid[got] {
			t.Fatalf("ChatFinishReason(%q) = %q, outside the Chat enum", reason, got)
		}
	}
}
