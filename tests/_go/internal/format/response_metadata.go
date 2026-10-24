package format

// Round 3 batch 15 regressions (B5, B6): response metadata and service_tier
// survive every envelope and stream terminal, and an omitted Anthropic
// stop_reason is an explicit approximation.

import (
	"bytes"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponseMetadataServiceTierRoundTrip(t *testing.T) {
	chatBody := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"metadata":{"k":"v"},"service_tier":"priority",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	resp, err := DecodeChatResponse([]byte(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Metadata["k"] != "v" || resp.ServiceTier != "priority" {
		t.Fatalf("Chat decode = metadata %v tier %q", resp.Metadata, resp.ServiceTier)
	}

	responsesBody, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, responsesBody)
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata["k"] != "v" || doc["service_tier"] != "priority" {
		t.Fatalf("Responses envelope = metadata %v tier %v (body %s)", metadata, doc["service_tier"], responsesBody)
	}

	back, err := DecodeResponsesResponse(responsesBody)
	if err != nil {
		t.Fatal(err)
	}
	if back.Metadata["k"] != "v" || back.ServiceTier != "priority" {
		t.Fatalf("Responses decode = metadata %v tier %q", back.Metadata, back.ServiceTier)
	}
	chatBack, err := EncodeChatResponse(back)
	if err != nil {
		t.Fatal(err)
	}
	chatDoc := decodeJSONMap(t, chatBack)
	chatMetadata, _ := chatDoc["metadata"].(map[string]any)
	if chatMetadata["k"] != "v" || chatDoc["service_tier"] != "priority" {
		t.Fatalf("Chat envelope = metadata %v tier %v (body %s)", chatMetadata, chatDoc["service_tier"], chatBack)
	}

	// The Responses stream terminal carries the same envelope fields the
	// start event declared.
	var buf bytes.Buffer
	enc := NewResponsesStreamEncoder(&buf, nil)
	if err := enc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "r1", Model: "m", Created: 1,
		Metadata: map[string]string{"k": "v"}, ServiceTier: "priority",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: ir.FinishStop}}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(&ir.Event{Type: ir.EventDone}); err != nil {
		t.Fatal(err)
	}
	terminal := ""
	for _, frame := range bytes.Split(buf.Bytes(), []byte("\n\n")) {
		if bytes.Contains(frame, []byte(`"type":"response.completed"`)) {
			terminal = string(frame)
		}
	}
	if terminal == "" {
		t.Fatalf("no response.completed terminal: %s", buf.String())
	}
	completed := decodeJSONMap(t, []byte(terminal[bytes.Index([]byte(terminal), []byte("{")):]))
	response, _ := completed["response"].(map[string]any)
	streamMetadata, _ := response["metadata"].(map[string]any)
	if streamMetadata["k"] != "v" || response["service_tier"] != "priority" {
		t.Fatalf("stream terminal = metadata %v tier %v", streamMetadata, response["service_tier"])
	}

	// A Chat stream chunk carries service_tier (metadata has no chunk slot).
	buf.Reset()
	chatEnc := NewChatStreamEncoder(&buf, nil, false)
	if err := chatEnc.Write(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
		ID: "c1", Model: "m", Created: 1, ServiceTier: "priority",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := chatEnc.Write(&ir.Event{Type: ir.EventDone}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"service_tier":"priority"`)) {
		t.Fatalf("Chat stream chunks lost service_tier: %s", buf.String())
	}
}

func TestAnthropicMissingStopReasonWarns(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	for _, target := range []ir.Protocol{ir.Chat, ir.Responses} {
		t.Run(string(target), func(t *testing.T) {
			out, warnings, err := TranslateResponseBetween(target, ir.Anthropic, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if !warningField(warnings, "stop_reason") {
				t.Fatalf("missing stop_reason warning: %+v", warnings)
			}
			doc := decodeJSONMap(t, out)
			switch target {
			case ir.Chat:
				choices, _ := doc["choices"].([]any)
				if len(choices) != 1 {
					t.Fatalf("choices = %v", doc["choices"])
				}
				if got := choices[0].(map[string]any)["finish_reason"]; got != "stop" {
					t.Fatalf("finish = %v, want the Chat enum stop", got)
				}
			case ir.Responses:
				if doc["status"] != "incomplete" {
					t.Fatalf("status = %v, want incomplete (body %s)", doc["status"], out)
				}
			}
		})
	}
}
