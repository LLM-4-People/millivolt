package format

// Batch 12 regressions: reasoning text and signatures, URL annotations and
// matched stop sequences are decoded or explicitly reported.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesReasoningTextDecoded(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		body := `{"model":"m","input":[` +
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"sum"}],` +
			`"content":[{"type":"reasoning_text","text":"raw"},{"type":"mystery"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) == 0 || len(req.Messages[0].Content) == 0 {
			t.Fatalf("decoded messages = %+v", req.Messages)
		}
		reasoning := req.Messages[0].Content[0].Reasoning
		if reasoning == nil || reasoning.Text != "raw" {
			t.Fatalf("reasoning = %+v, want the reasoning_text content", reasoning)
		}
		if len(reasoning.Summary) != 1 || reasoning.Summary[0] != "sum" {
			t.Fatalf("reasoning summary = %v", reasoning.Summary)
		}
		if !warningField(req.Warnings, "content.mystery") {
			t.Fatalf("nested reasoning warning was discarded: %+v", req.Warnings)
		}

		out, err := EncodeAnthropicRequest(req, 8, "")
		if err != nil {
			t.Fatal(err)
		}
		blocks := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "raw" {
			t.Fatalf("Anthropic reasoning blocks = %v, want visible text", blocks)
		}
		if !warningField(req.Warnings, "reasoning") {
			t.Fatalf("cross-protocol reasoning was not reported: %+v", req.Warnings)
		}
	})

	t.Run("response", func(t *testing.T) {
		body := `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[` +
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"sum"}],` +
			`"content":[{"type":"reasoning_text","text":"raw"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
		resp, err := DecodeResponsesResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		var reasoning *ir.Reasoning
		for _, block := range resp.Blocks {
			if block.Type == ir.BlockReasoning {
				reasoning = block.Reasoning
			}
		}
		if reasoning == nil || reasoning.Text != "raw" {
			t.Fatalf("decoded reasoning = %+v, want the reasoning_text content", reasoning)
		}
		out, err := EncodeChatResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		message := decodeJSONMap(t, out)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if reasoning, _ := message["reasoning_content"].(string); !strings.Contains(reasoning, "raw") {
			t.Fatalf("Chat reasoning_content = %v, want the decoded reasoning_text", message["reasoning_content"])
		}
	})
}

func TestAnthropicSignatureWarnsOnResponses(t *testing.T) {
	resp := &ir.Response{
		ID: "r1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{{
			Type: ir.BlockReasoning,
			Reasoning: &ir.Reasoning{
				Text: "thinking", Signature: "sig", Provider: ir.Anthropic,
			},
		}},
	}
	body, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	item := decodeJSONMap(t, body)["output"].([]any)[0].(map[string]any)
	if _, present := item["signature"]; present {
		t.Fatalf("Responses reasoning carries a signature: %v", item)
	}
	if _, present := item["encrypted_content"]; present {
		t.Fatalf("Responses reasoning carries foreign encrypted content: %v", item)
	}
	found := false
	for _, w := range resp.Warnings {
		if w.Field == "reasoning" && w.Kind == ir.WarningOpaque {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing opaque reasoning warning: %+v", resp.Warnings)
	}
}

func TestChatAndResponsesAnnotationsPreserved(t *testing.T) {
	t.Run("chat to responses and back", func(t *testing.T) {
		body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"cited answer","annotations":[` +
			`{"type":"url_citation","url_citation":{"url":"https://example.com/a","title":"Example",` +
			`"start_index":0,"end_index":5}}]},"finish_reason":"stop"}]}`
		resp, err := DecodeChatResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Blocks) != 1 || len(resp.Blocks[0].Citations) != 1 {
			t.Fatalf("decoded citations = %+v", resp.Blocks)
		}
		if citation := resp.Blocks[0].Citations[0]; citation.URL != "https://example.com/a" || citation.Title != "Example" {
			t.Fatalf("citation = %+v", citation)
		}

		responsesBody, err := EncodeResponsesResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		part := decodeJSONMap(t, responsesBody)["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		annotations := part["annotations"].([]any)
		if len(annotations) != 1 {
			t.Fatalf("Responses annotations = %v", annotations)
		}
		annotation := annotations[0].(map[string]any)
		if annotation["type"] != "url_citation" || annotation["url"] != "https://example.com/a" || annotation["title"] != "Example" {
			t.Fatalf("Responses annotation = %v", annotation)
		}

		roundTrip, err := DecodeResponsesResponse(responsesBody)
		if err != nil {
			t.Fatal(err)
		}
		chatBody, err := EncodeChatResponse(roundTrip)
		if err != nil {
			t.Fatal(err)
		}
		message := decodeJSONMap(t, chatBody)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		chatAnnotations := message["annotations"].([]any)
		wrapper := chatAnnotations[0].(map[string]any)
		if wrapper["type"] != "url_citation" {
			t.Fatalf("Chat annotation wrapper = %v", wrapper)
		}
		if wrapper["url_citation"].(map[string]any)["url"] != "https://example.com/a" {
			t.Fatalf("Chat annotation = %v", wrapper)
		}
	})

	t.Run("annotations without a text block warn", func(t *testing.T) {
		body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":null,"annotations":[` +
			`{"type":"url_citation","url_citation":{"url":"https://example.com/a","title":"Example",` +
			`"start_index":0,"end_index":5}}],"tool_calls":[{"id":"call_1","type":"function",` +
			`"function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
		resp, err := DecodeChatResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if !warningField(resp.Warnings, "annotations") {
			t.Fatalf("annotation with no text block did not warn: %+v", resp.Warnings)
		}
	})

	t.Run("unmappable anthropic citation warns", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"cited","citations":[{"type":"char_location","cited_text":"x"}]}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		resp, err := DecodeAnthropicResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		chatBody, err := EncodeChatResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		message := decodeJSONMap(t, chatBody)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if _, present := message["annotations"]; present {
			t.Fatalf("Chat emitted an unmappable citation: %v", message)
		}
		if !warningField(resp.Warnings, "citations") {
			t.Fatalf("missing unmappable citation warning: %+v", resp.Warnings)
		}

		responsesResp, err := DecodeAnthropicResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		responsesBody, err := EncodeResponsesResponse(responsesResp)
		if err != nil {
			t.Fatal(err)
		}
		part := decodeJSONMap(t, responsesBody)["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		if len(part["annotations"].([]any)) != 0 {
			t.Fatalf("Responses emitted an unmappable citation: %v", part)
		}
		if !warningField(responsesResp.Warnings, "citations") {
			t.Fatalf("missing Responses citation warning: %+v", responsesResp.Warnings)
		}
	})
}

func TestAnthropicStopSequenceWarns(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"done"}],"stop_reason":"stop_sequence",` +
		`"stop_sequence":"END","usage":{"input_tokens":1,"output_tokens":1}}`

	chatResp, err := DecodeAnthropicResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if chatResp.StopSequence != "END" {
		t.Fatalf("decoded stop_sequence = %q", chatResp.StopSequence)
	}
	if _, err := EncodeChatResponse(chatResp); err != nil {
		t.Fatal(err)
	}
	if !warningField(chatResp.Warnings, "stop_sequence") {
		t.Fatalf("Chat did not report the lost stop sequence: %+v", chatResp.Warnings)
	}

	responsesResp, err := DecodeAnthropicResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeResponsesResponse(responsesResp); err != nil {
		t.Fatal(err)
	}
	if !warningField(responsesResp.Warnings, "stop_sequence") {
		t.Fatalf("Responses did not report the lost stop sequence: %+v", responsesResp.Warnings)
	}

	anthropicResp, err := DecodeAnthropicResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicResponse(anthropicResp)
	if err != nil {
		t.Fatal(err)
	}
	if decodeJSONMap(t, out)["stop_sequence"] != "END" {
		t.Fatalf("Anthropic stop_sequence was not preserved: %s", out)
	}
	if warningField(anthropicResp.Warnings, "stop_sequence") {
		t.Fatalf("Anthropic warned about a stop sequence it emits: %+v", anthropicResp.Warnings)
	}
}

func TestEmptyReasoningBlockWarnsAnthropic(t *testing.T) {
	resp := &ir.Response{
		ID: "r1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{{
			Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses},
		}},
	}
	out, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if blocks := decodeJSONMap(t, out)["content"].([]any); len(blocks) != 0 {
		t.Fatalf("Anthropic content = %v, want the empty reasoning block dropped", blocks)
	}
	if !warningField(resp.Warnings, "reasoning") {
		t.Fatalf("empty reasoning block was not reported: %+v", resp.Warnings)
	}
}
