package format

// Batch 11 regressions: prompt-cache breakpoints survive every target that
// supports them, and every request-side loss reports a warning.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestToolAndBlockCacheControlRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":8,` +
		`"cache_control":{"type":"ephemeral","ttl":"5m"},` +
		`"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{},` +
		`"cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok",` +
		`"cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.CacheControl == nil || req.CacheControl.Type != "ephemeral" || req.CacheControl.TTL != "5m" {
		t.Fatalf("top-level cache_control = %+v", req.CacheControl)
	}
	toolUse := req.Messages[1].Content[0]
	if toolUse.CacheControl == nil || toolUse.CacheControl.Type != "ephemeral" {
		t.Fatalf("tool_use cache_control = %+v", toolUse.CacheControl)
	}
	toolResult := req.Messages[2].Content[0]
	if toolResult.CacheControl == nil || toolResult.CacheControl.TTL != "1h" {
		t.Fatalf("tool_result cache_control = %+v", toolResult.CacheControl)
	}

	out, err := EncodeAnthropicRequest(req, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, out)
	top := doc["cache_control"].(map[string]any)
	if top["type"] != "ephemeral" || top["ttl"] != "5m" {
		t.Fatalf("encoded top-level cache_control = %v", top)
	}
	encodedToolUse := doc["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if encodedToolUse["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("encoded tool_use = %v", encodedToolUse)
	}
	encodedToolResult := doc["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if encodedToolResult["cache_control"].(map[string]any)["ttl"] != "1h" {
		t.Fatalf("encoded tool_result = %v", encodedToolResult)
	}
}

func TestTopLevelAnthropicCacheControlWarns(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"cache_control":{"type":"ephemeral"},` +
		`"messages":[{"role":"user","content":"hi"}]}`

	chatReq, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeChatRequest(chatReq); err != nil {
		t.Fatal(err)
	}
	if !warningField(chatReq.Warnings, "cache_control") {
		t.Fatalf("Chat did not report the lost request cache breakpoint: %+v", chatReq.Warnings)
	}

	responsesReq, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeResponsesRequest(responsesReq); err != nil {
		t.Fatal(err)
	}
	if !warningField(responsesReq.Warnings, "cache_control") {
		t.Fatalf("Responses did not report the lost request cache breakpoint: %+v", responsesReq.Warnings)
	}
}

func TestFoldedSystemLossesWarn(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{
			Role: ir.RoleSystem,
			Name: "ops",
			Content: []ir.Block{
				{
					Type:         ir.BlockText,
					Text:         "rules",
					CacheControl: &ir.CacheControl{Type: "ephemeral"},
					Citations:    []ir.Citation{{Type: "char_location", CitedText: "x"}},
				},
				{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
			},
		},
		{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
	}}
	out, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, out)
	if doc["instructions"] != "rules" {
		t.Fatalf("instructions = %v, want the folded text", doc["instructions"])
	}
	input := doc["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %v, want the leading system message folded away", input)
	}
	for _, field := range []string{"system.cache_control", "system.citations", "system.image", "messages.name"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("missing folded-system warning %s: %+v", field, req.Warnings)
		}
	}
}

func TestMessageNameWarns(t *testing.T) {
	newRequest := func() *ir.Request {
		return &ir.Request{Model: "m", Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Name:    "alice",
			Content: []ir.Block{ir.TextBlock("hi")},
		}}}
	}

	chatReq := newRequest()
	chatBody, err := EncodeChatRequest(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	message := decodeJSONMap(t, chatBody)["messages"].([]any)[0].(map[string]any)
	if message["name"] != "alice" {
		t.Fatalf("Chat message name = %v", message["name"])
	}
	if warningField(chatReq.Warnings, "messages.name") {
		t.Fatalf("Chat warned about a name it emits: %+v", chatReq.Warnings)
	}

	anthropicReq := newRequest()
	anthropicBody, err := EncodeAnthropicRequest(anthropicReq, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(anthropicReq.Warnings, "messages.name") {
		t.Fatalf("Anthropic did not report the lost message name: %+v", anthropicReq.Warnings)
	}
	if _, present := decodeJSONMap(t, anthropicBody)["messages"].([]any)[0].(map[string]any)["name"]; present {
		t.Fatalf("Anthropic emitted a participant name: %s", anthropicBody)
	}

	responsesReq := newRequest()
	responsesBody, err := EncodeResponsesRequest(responsesReq)
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(responsesReq.Warnings, "messages.name") {
		t.Fatalf("Responses did not report the lost message name: %+v", responsesReq.Warnings)
	}
	if _, present := decodeJSONMap(t, responsesBody)["input"].([]any)[0].(map[string]any)["name"]; present {
		t.Fatalf("Responses emitted a participant name: %s", responsesBody)
	}
}

func TestResponsesToolCacheControlWarns(t *testing.T) {
	newRequest := func() *ir.Request {
		return &ir.Request{
			Model:    "m",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
			Tools: []ir.Tool{{
				Name:         "f",
				Parameters:   json.RawMessage(`{"type":"object"}`),
				CacheControl: &ir.CacheControl{Type: "ephemeral"},
			}},
		}
	}

	responsesReq := newRequest()
	body, err := EncodeResponsesRequest(responsesReq)
	if err != nil {
		t.Fatal(err)
	}
	tool := decodeJSONMap(t, body)["tools"].([]any)[0].(map[string]any)
	if _, present := tool["cache_control"]; present {
		t.Fatalf("Responses tool carries cache_control: %v", tool)
	}
	if !warningField(responsesReq.Warnings, "tools.cache_control") {
		t.Fatalf("missing tool cache-control warning: %+v", responsesReq.Warnings)
	}

	anthropicReq := newRequest()
	body, err = EncodeAnthropicRequest(anthropicReq, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	tool = decodeJSONMap(t, body)["tools"].([]any)[0].(map[string]any)
	if tool["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("Anthropic tool cache_control = %v", tool)
	}
	if warningField(anthropicReq.Warnings, "tools.cache_control") {
		t.Fatalf("Anthropic warned about a cache breakpoint it emits: %+v", anthropicReq.Warnings)
	}
}
