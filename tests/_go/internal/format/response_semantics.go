package format

// Batch 12 regressions: reasoning text and signatures, URL annotations and
// matched stop sequences are decoded or explicitly reported.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesReasoningTextDecoded(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		body := `{"model":"m","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]},` +
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"sum"}],` +
			`"content":[{"type":"reasoning_text","text":"raw"},{"type":"mystery"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) < 2 || len(req.Messages[1].Content) == 0 {
			t.Fatalf("decoded messages = %+v", req.Messages)
		}
		reasoning := req.Messages[1].Content[0].Reasoning
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
		blocks := decodeJSONMap(t, out)["messages"].([]any)[1].(map[string]any)["content"].([]any)
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

// TestEncodeResponsesResponseTable pins the terminal mapping and every output
// item shape the Responses encoder can emit. A status regression (failed,
// incomplete, cancelled) or a misplaced usage block would otherwise pass the
// happy-path integration test.
func TestEncodeResponsesResponseTable(t *testing.T) {
	usage := ir.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}
	status := func(doc map[string]any) string {
		s, _ := doc["status"].(string)
		return s
	}
	output := func(doc map[string]any) []any {
		items, _ := doc["output"].([]any)
		return items
	}
	cases := []struct {
		name   string
		resp   *ir.Response
		verify func(t *testing.T, doc map[string]any)
	}{
		{
			name: "completed text with usage",
			resp: &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishStop, Usage: usage,
				Blocks: []ir.Block{{Type: ir.BlockText, Text: "hello"}},
			},
			verify: func(t *testing.T, doc map[string]any) {
				if status(doc) != "completed" {
					t.Fatalf("status = %v", doc["status"])
				}
				if doc["error"] != nil || doc["incomplete_details"] != nil {
					t.Fatalf("completed response carries a failure: %v", doc)
				}
				items := output(doc)
				if len(items) != 1 {
					t.Fatalf("output = %v", items)
				}
				message := items[0].(map[string]any)
				if message["type"] != "message" || message["status"] != "completed" {
					t.Fatalf("message item = %v", message)
				}
				part := message["content"].([]any)[0].(map[string]any)
				if part["type"] != "output_text" || part["text"] != "hello" {
					t.Fatalf("content part = %v", part)
				}
				u := doc["usage"].(map[string]any)
				if u["input_tokens"] != float64(3) || u["output_tokens"] != float64(2) || u["total_tokens"] != float64(5) {
					t.Fatalf("usage = %v", u)
				}
			},
		},
		{
			name: "failed error",
			resp: &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishStop,
				Error: &ir.Error{Code: "server_error", Message: "boom"},
			},
			verify: func(t *testing.T, doc map[string]any) {
				if status(doc) != "failed" {
					t.Fatalf("status = %v", doc["status"])
				}
				failure := doc["error"].(map[string]any)
				if failure["code"] != "server_error" || failure["message"] != "boom" {
					t.Fatalf("error = %v", failure)
				}
				if items := output(doc); len(items) != 0 {
					t.Fatalf("failed response output = %v", items)
				}
				if _, present := doc["usage"]; present {
					t.Fatalf("failed response carries usage: %v", doc["usage"])
				}
			},
		},
		{
			name: "incomplete length",
			resp: &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishLength,
				Blocks: []ir.Block{{Type: ir.BlockText, Text: "cut"}},
			},
			verify: func(t *testing.T, doc map[string]any) {
				if status(doc) != "incomplete" {
					t.Fatalf("status = %v", doc["status"])
				}
				details := doc["incomplete_details"].(map[string]any)
				if details["reason"] != "max_output_tokens" {
					t.Fatalf("incomplete_details = %v", details)
				}
			},
		},
		{
			name: "incomplete content filter",
			resp: &ir.Response{ID: "r1", Model: "m", Finish: ir.FinishContentFilter},
			verify: func(t *testing.T, doc map[string]any) {
				details := doc["incomplete_details"].(map[string]any)
				if details["reason"] != "content_filter" {
					t.Fatalf("incomplete_details = %v", details)
				}
			},
		},
		{
			name: "incomplete explicit reason",
			resp: &ir.Response{ID: "r1", Model: "m", Finish: ir.FinishOther, IncompleteReason: "custom_reason"},
			verify: func(t *testing.T, doc map[string]any) {
				details := doc["incomplete_details"].(map[string]any)
				if details["reason"] != "custom_reason" {
					t.Fatalf("incomplete_details = %v", details)
				}
			},
		},
		{
			name: "cancelled",
			resp: &ir.Response{ID: "r1", Model: "m", Finish: ir.FinishCancelled},
			verify: func(t *testing.T, doc map[string]any) {
				if status(doc) != "cancelled" {
					t.Fatalf("status = %v", doc["status"])
				}
			},
		},
		{
			name: "explicit source status",
			resp: &ir.Response{ID: "r1", Model: "m", Finish: ir.FinishStop, Status: "in_progress",
				Blocks: []ir.Block{{Type: ir.BlockText, Text: "partial"}}},
			verify: func(t *testing.T, doc map[string]any) {
				if status(doc) != "in_progress" {
					t.Fatalf("status = %v", doc["status"])
				}
			},
		},
		{
			name: "tool call",
			resp: &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishToolCalls,
				Blocks: []ir.Block{{
					Type: ir.BlockToolCall,
					ToolCall: &ir.ToolCall{
						ID: "call_1", Name: "f", Arguments: json.RawMessage(`{"a":1}`),
					},
				}},
			},
			verify: func(t *testing.T, doc map[string]any) {
				items := output(doc)
				if len(items) != 1 {
					t.Fatalf("output = %v", items)
				}
				call := items[0].(map[string]any)
				if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "f" {
					t.Fatalf("function_call = %v", call)
				}
				if call["arguments"] != `{"a":1}` {
					t.Fatalf("arguments = %v", call["arguments"])
				}
			},
		},
		{
			name: "reasoning",
			resp: &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishStop,
				Blocks: []ir.Block{{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: "think"}}},
			},
			verify: func(t *testing.T, doc map[string]any) {
				items := output(doc)
				if len(items) != 1 || items[0].(map[string]any)["type"] != "reasoning" {
					t.Fatalf("output = %v", items)
				}
				summary := items[0].(map[string]any)["summary"].([]any)
				if len(summary) != 1 || summary[0].(map[string]any)["text"] != "think" {
					t.Fatalf("reasoning summary = %v", summary)
				}
			},
		},
		{
			name: "refusal",
			resp: &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishStop,
				Blocks: []ir.Block{{Type: ir.BlockRefusal, Refusal: "no"}},
			},
			verify: func(t *testing.T, doc map[string]any) {
				items := output(doc)
				if len(items) != 1 || items[0].(map[string]any)["type"] != "message" {
					t.Fatalf("output = %v", items)
				}
				part := items[0].(map[string]any)["content"].([]any)[0].(map[string]any)
				if part["type"] != "refusal" || part["refusal"] != "no" {
					t.Fatalf("refusal part = %v", part)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := EncodeResponsesResponse(tc.resp)
			if err != nil {
				t.Fatal(err)
			}
			tc.verify(t, decodeJSONMap(t, body))
		})
	}
}

// TestResponsesRequestRejectsUnsupportedState pins the decoder's fail-closed
// boundary for fields that need provider-side state or a stored prompt: each
// must name the rejected feature instead of being dropped or forwarded.
func TestResponsesRequestRejectsUnsupportedState(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"background", `{"model":"m","input":"hi","background":true}`, "background"},
		{"conversation", `{"model":"m","input":"hi","conversation":"c1"}`, "conversation"},
		{"prompt", `{"model":"m","prompt":{"id":"p1"}}`, "prompt"},
		{"item_reference", `{"model":"m","input":[{"type":"item_reference","id":"ref_1"}]}`, "item_reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeResponsesRequest([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("DecodeResponsesRequest() = %v, want a rejection naming %q", err, tc.field)
			}
		})
	}
}
