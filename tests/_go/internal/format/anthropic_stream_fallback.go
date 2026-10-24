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
