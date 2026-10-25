package format

// Round 11 B3 regression: a translated Anthropic Message carries every
// required-nullable member of the current schema, and the streaming
// MessageDeltaUsage keeps only the members its own type declares.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func requireNullMember(t *testing.T, obj map[string]json.RawMessage, key string) {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("required nullable member %q is absent", key)
	}
	if string(raw) != "null" {
		t.Fatalf("member %q = %s, want null", key, raw)
	}
}

func TestAnthropicMessageEnvelopeMembers(t *testing.T) {
	chat := `{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	out, _, err := TranslateResponseBetween(ir.Anthropic, ir.Chat, []byte(chat))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"container", "diagnostics", "stop_details"} {
		requireNullMember(t, top, key)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(top["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "cache_creation", "inference_geo",
		"output_tokens_details", "server_tool_use", "service_tier", "speed",
	} {
		if _, ok := usage[key]; !ok {
			t.Fatalf("Message.usage member %q is absent: %s", key, out)
		}
	}
	for _, key := range []string{"cache_creation", "inference_geo", "output_tokens_details", "server_tool_use", "service_tier", "speed"} {
		requireNullMember(t, usage, key)
	}

	// Reasoning maps into output_tokens_details; an explicit tier maps to
	// service_tier while speed stays null.
	chat = `{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"service_tier":"priority",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,` +
		`"completion_tokens_details":{"reasoning_tokens":2}}}`
	out, _, err = TranslateResponseBetween(ir.Anthropic, ir.Chat, []byte(chat))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(top["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	var details map[string]int
	if err := json.Unmarshal(usage["output_tokens_details"], &details); err != nil {
		t.Fatalf("output_tokens_details = %s: %v", usage["output_tokens_details"], err)
	}
	if details["thinking_tokens"] != 2 {
		t.Fatalf("thinking_tokens = %d, want 2", details["thinking_tokens"])
	}
	if string(usage["service_tier"]) != `"priority"` {
		t.Fatalf("service_tier = %s, want priority", usage["service_tier"])
	}
	requireNullMember(t, usage, "speed")
}

// TestAnthropicStreamUsageShapes pins the message_start (Usage) and
// message_delta (MessageDeltaUsage) shapes separately.
func TestAnthropicStreamUsageShapes(t *testing.T) {
	usage := `"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,` +
		`"completion_tokens_details":{"reasoning_tokens":2}}`
	source := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"created\":1," +
		"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]," + usage + "}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"created\":1," +
		"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," + usage + "}\n\n" +
		"data: [DONE]\n\n"
	out, err := translateStreamSource(t, ir.Chat, ir.Anthropic, source)
	if err != nil {
		t.Fatal(err)
	}
	var startUsage, deltaUsage map[string]json.RawMessage
	startMessage := map[string]json.RawMessage{}
	for _, frame := range strings.Split(out, "\n\n") {
		var data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch string(ev["type"]) {
		case `"message_start"`:
			if err := json.Unmarshal(ev["message"], &startMessage); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(startMessage["usage"], &startUsage); err != nil {
				t.Fatal(err)
			}
		case `"message_delta"`:
			if err := json.Unmarshal(ev["usage"], &deltaUsage); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(startUsage) == 0 || len(deltaUsage) == 0 {
		t.Fatalf("missing usage shapes: %s", out)
	}
	for _, key := range []string{"container", "diagnostics", "stop_details"} {
		requireNullMember(t, startMessage, key)
	}
	for _, key := range []string{
		"input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "cache_creation", "inference_geo",
		"output_tokens_details", "server_tool_use", "service_tier", "speed",
	} {
		if _, ok := startUsage[key]; !ok {
			t.Fatalf("message_start usage member %q is absent: %s", key, out)
		}
	}
	deltaKeys := []string{
		"input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "output_tokens_details", "server_tool_use",
	}
	if len(deltaUsage) != len(deltaKeys) {
		t.Fatalf("message_delta usage has %d members, want %d: %s", len(deltaUsage), len(deltaKeys), out)
	}
	for _, key := range deltaKeys {
		if _, ok := deltaUsage[key]; !ok {
			t.Fatalf("message_delta usage member %q is absent: %s", key, out)
		}
	}
	for _, key := range []string{"cache_creation", "inference_geo", "service_tier", "speed"} {
		if _, ok := deltaUsage[key]; ok {
			t.Fatalf("message_delta usage gained %q, which its type does not declare: %s", key, out)
		}
	}
	// The final delta must carry the source's reasoning split, not the null
	// placeholder: MessageDeltaUsage owns output_tokens_details too.
	var deltaDetails map[string]int
	if err := json.Unmarshal(deltaUsage["output_tokens_details"], &deltaDetails); err != nil {
		t.Fatalf("message_delta output_tokens_details = %s: %v", deltaUsage["output_tokens_details"], err)
	}
	if deltaDetails["thinking_tokens"] != 2 {
		t.Fatalf("message_delta thinking_tokens = %d, want 2: %s", deltaDetails["thinking_tokens"], out)
	}
}
