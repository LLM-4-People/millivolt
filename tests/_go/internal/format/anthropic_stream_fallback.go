package format

// Round 6 C3 regression: the Anthropic stream encoder ignored
// StreamOptions.FallbackModel, so a Chat source stream whose first chunk
// omitted model opened message_start with model "". Anthropic requires
// Message.model. A late start must also not rewrite the identity already sent.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicStreamUsesFallbackModel(t *testing.T) {
	source := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &out, strings.NewReader(source), nil, nil,
		StreamOptions{FallbackModel: "req-model"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"model":"req-model"`) {
		t.Fatalf("message_start did not carry the fallback model: %s", out.String())
	}
	if strings.Contains(out.String(), `"model":""`) {
		t.Fatalf("message_start carried an empty required model: %s", out.String())
	}
}

func TestAnthropicStreamLateStartCannotRewriteIdentity(t *testing.T) {
	var out bytes.Buffer
	enc := NewAnthropicStreamEncoder(&out, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "msg_first", Model: "first-model", Created: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "late-id", Model: "late-model", Created: 99,
	}}); err != nil {
		t.Fatal(err)
	}
	if enc.model != "first-model" || enc.id != "msg_first" || enc.created != 1 {
		t.Fatalf("a late start rewrote committed identity: model=%q id=%q created=%d",
			enc.model, enc.id, enc.created)
	}
	if !strings.Contains(out.String(), `"model":"first-model"`) {
		t.Fatalf("committed message_start lost its model: %s", out.String())
	}
}

// A late start cannot rewrite committed identity, but its usage still belongs
// in the target's terminal frame. Negative late usage is rejected like every
// other usage value rather than silently ignored.
func TestAnthropicStreamLateStartMergesUsage(t *testing.T) {
	var out bytes.Buffer
	enc := NewAnthropicStreamEncoder(&out, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "msg_first", Model: "first-model", Created: 1,
		Usage: ir.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "late-id", Model: "late-model", Created: 99,
		Usage: ir.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: ir.FinishStop}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventDone}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"model":"first-model"`) || strings.Contains(got, "late-model") {
		t.Fatalf("late start rewrote the committed model: %s", got)
	}
	if !strings.Contains(got, `"input_tokens":10`) || !strings.Contains(got, `"output_tokens":5`) {
		t.Fatalf("late-start usage was dropped from the terminal frame: %s", got)
	}

	var invalid bytes.Buffer
	enc = NewAnthropicStreamEncoder(&invalid, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "msg_first", Model: "m", Created: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "late", Model: "late", Usage: ir.Usage{InputTokens: -1},
	}})
	if err == nil {
		t.Fatalf("negative late-start usage was accepted: %s", invalid.String())
	}
}
