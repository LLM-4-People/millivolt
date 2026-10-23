package format

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func decodeJSONMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode json: %v\n%s", err, b)
	}
	return out
}

func TestChatToAnthropicRequestCodec(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"system","content":"be nice"},
			{"role":"user","content":[{"type":"text","text":"weather?"},{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}]},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NY\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"sunny"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"},"strict":true}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"parallel_tool_calls":false,
		"max_completion_tokens":100,
		"temperature":0.7,
		"top_p":0.9,
		"stop":["END"],
		"reasoning_effort":"medium",
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":true}}
	}`
	req, err := DecodeChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 4096, "cache-key")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["system"] != "be nice" {
		t.Fatalf("system = %v", got["system"])
	}
	if got["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	if got["temperature"] != 0.7 || got["top_p"] != 0.9 {
		t.Fatalf("sampling = %v %v", got["temperature"], got["top_p"])
	}
	if got["prompt_cache_key"] != "cache-key" {
		t.Fatalf("prompt_cache_key = %v", got["prompt_cache_key"])
	}
	stops := got["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop_sequences = %v", stops)
	}
	tc := got["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v", tc)
	}
	if tc["disable_parallel_tool_use"] != true {
		t.Fatalf("disable_parallel_tool_use = %v", tc["disable_parallel_tool_use"])
	}
	tools := got["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["input_schema"] == nil || tool["strict"] != true {
		t.Fatalf("tool = %v", tool)
	}
	oc := got["output_config"].(map[string]any)
	if oc["effort"] != "medium" {
		t.Fatalf("output_config.effort = %v", oc["effort"])
	}
	format := oc["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output_config.format = %v", format)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d: %v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant block = %v", blocks[0])
	}
	toolResult := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_1" || toolResult["content"] != "sunny" {
		t.Fatalf("tool_result = %v", toolResult)
	}
}

func TestAnthropicToChatRequestCodec(t *testing.T) {
	in := `{
		"model":"claude",
		"max_tokens":512,
		"system":[{"type":"text","text":"be careful","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"NY"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny","is_error":false}]}
		],
		"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"tool","name":"get_weather","disable_parallel_tool_use":true},
		"thinking":{"type":"enabled","budget_tokens":2048},
		"stop_sequences":["END"],
		"metadata":{"user_id":"u1"},
		"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}}
	}`
	req, err := DecodeAnthropicRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	if got["user"] != "u1" {
		t.Fatalf("user = %v", got["user"])
	}
	if got["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %v", got["parallel_tool_calls"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", got["reasoning_effort"])
	}
	if got["response_format"].(map[string]any)["type"] != "json_schema" {
		t.Fatalf("response_format = %v", got["response_format"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message = %v", msgs[0])
	}
	user := msgs[1].(map[string]any)
	parts := user["content"].([]any)
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("image part = %v", parts[1])
	}
	assistant := msgs[2].(map[string]any)
	if assistant["reasoning_content"] != "hmm" {
		t.Fatalf("reasoning_content = %v", assistant["reasoning_content"])
	}
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "toolu_1" || call["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool call = %v", call)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "sunny" {
		t.Fatalf("tool message = %v", tool)
	}
}

func TestResponsesToChatRequestCodec(t *testing.T) {
	in := `{
		"model":"gpt",
		"instructions":"be brief",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"enc"},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"NY\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		],
		"max_output_tokens":256,
		"tools":[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"},"strict":true}],
		"tool_choice":"required",
		"reasoning":{"effort":"low","summary":"auto"},
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}}
	}`
	req, err := DecodeResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["max_tokens"] != float64(256) {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", got["tool_choice"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v", got["reasoning_effort"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "be brief" {
		t.Fatalf("instructions message = %v", msgs[0])
	}
	assistant := msgs[2].(map[string]any)
	if assistant["reasoning_content"] != "thinking" {
		t.Fatalf("reasoning_content = %v", assistant["reasoning_content"])
	}
	calls := assistant["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool call = %v", calls[0])
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", tool)
	}
}

func TestChatToResponsesRequestCodec(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[
			{"role":"system","content":"be nice"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"max_tokens":64,
		"stop":["END"],
		"seed":7,
		"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"}}}
	}`
	req, err := DecodeChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["instructions"] != "be nice" {
		t.Fatalf("instructions = %v", got["instructions"])
	}
	if got["max_output_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens = %v", got["max_output_tokens"])
	}
	if got["text"].(map[string]any)["format"].(map[string]any)["type"] != "json_schema" {
		t.Fatalf("text.format = %v", got["text"])
	}
	tools := got["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "f" {
		t.Fatalf("tools = %v", tools)
	}
	input := got["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input len = %d: %v", len(input), input)
	}
	if input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input[0] = %v", input[0])
	}
	if input[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("input[1] = %v", input[1])
	}
	if input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input[2] = %v", input[2])
	}
	if !strings.Contains(req.Warnings.Header(), "seed") || !strings.Contains(req.Warnings.Header(), "stop") {
		t.Fatalf("warnings = %v", req.Warnings)
	}
}

func TestResponsesResponseToChat(t *testing.T) {
	in := `{
		"id":"resp_1","object":"response","created_at":123,"status":"completed","model":"gpt",
		"output":[
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"enc"},
			{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f","arguments":"{\"a\":1}","status":"completed"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1},"output_tokens_details":{"reasoning_tokens":3}}
	}`
	resp, err := DecodeResponsesResponse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	choice := got["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hello" || msg["reasoning_content"] != "think" {
		t.Fatalf("message = %v", msg)
	}
	calls := msg["tool_calls"].([]any)
	if calls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool_calls = %v", calls)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
	details := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(2) || details["cache_write_tokens"] != float64(1) {
		t.Fatalf("prompt details = %v", details)
	}
}

func TestAnthropicResponseToChatUsage(t *testing.T) {
	in := `{
		"id":"msg_1","type":"message","role":"assistant","model":"claude",
		"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens_details":{"thinking_tokens":1}}
	}`
	resp, err := DecodeAnthropicResponse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(15) {
		t.Fatalf("prompt_tokens = %v, want 15 (input + cache read + cache write)", usage["prompt_tokens"])
	}
	if usage["total_tokens"] != float64(20) {
		t.Fatalf("total_tokens = %v", usage["total_tokens"])
	}
}

func TestChatResponseToAnthropicUsage(t *testing.T) {
	in := `{
		"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":15,"completion_tokens":5,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}
	}`
	resp, err := DecodeChatResponse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) {
		t.Fatalf("input_tokens = %v, want 10 (total minus cache)", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(3) || usage["cache_creation_input_tokens"] != float64(2) {
		t.Fatalf("cache usage = %v", usage)
	}
	if got["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", got["stop_reason"])
	}
}

func TestResponsesToAnthropicRequestCodec(t *testing.T) {
	in := `{
		"model":"gpt",
		"instructions":"be brief",
		"input":[
			{"role":"user","content":"hi"},
			{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		],
		"max_output_tokens":128,
		"reasoning":{"effort":"high"}
	}`
	req, err := DecodeResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["system"] != "be brief" {
		t.Fatalf("system = %v", got["system"])
	}
	if got["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	oc := got["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("effort = %v", oc["effort"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	assistant := msgs[1].(map[string]any)
	if assistant["content"].([]any)[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("assistant = %v", assistant)
	}
	toolResult := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result = %v", toolResult)
	}
}

func TestAnthropicToResponsesRequestCodec(t *testing.T) {
	in := `{
		"model":"claude","max_tokens":128,
		"system":"be brief",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		],
		"thinking":{"type":"enabled","budget_tokens":6000}
	}`
	req, err := DecodeAnthropicRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["instructions"] != "be brief" {
		t.Fatalf("instructions = %v", got["instructions"])
	}
	if got["max_output_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens = %v", got["max_output_tokens"])
	}
	reasoning := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" {
		t.Fatalf("reasoning.effort = %v", reasoning["effort"])
	}
	input := got["input"].([]any)
	if input[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("input[1] = %v", input[1])
	}
	if input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input[2] = %v", input[2])
	}
}

// TestDecodeChatResponseErrorEnvelope pins how a Chat body's top-level error
// key is classified: real object/string envelopes become a canonical error,
// while a null or empty object is the decoy that must not mask a valid
// completion (the same rules metrics.ParseErrorEnvelope owns for every wire).
func TestDecodeChatResponseErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
		typ     string
		code    string
	}{
		{"object envelope", `{"error":{"message":"quota","type":"insufficient_quota","code":"insufficient_quota"}}`, true, "insufficient_quota", "insufficient_quota"},
		{"code only", `{"error":{"message":"boom","code":"server_error"}}`, true, "provider_error", "server_error"},
		{"string envelope", `{"error":"plain failure"}`, true, "provider_error", ""},
		{"null decoy", `{"error":null,"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, false, "", ""},
		{"empty object decoy", `{"error":{},"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := DecodeChatResponse([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantErr {
				if resp.Error == nil {
					t.Fatalf("missing canonical error: %+v", resp)
				}
				if resp.Error.Type != tc.typ || resp.Error.Code != tc.code {
					t.Fatalf("error = %+v, want type %q code %q", resp.Error, tc.typ, tc.code)
				}
				if resp.Finish != ir.FinishError {
					t.Fatalf("finish = %q, want error", resp.Finish)
				}
				return
			}
			if resp.Error != nil {
				t.Fatalf("decoy produced an error: %+v", resp.Error)
			}
			if resp.Finish != ir.FinishStop || len(resp.Blocks) != 1 {
				t.Fatalf("decoy completion = %+v", resp)
			}
		})
	}
}

func TestCodecRejectsStatefulReplay(t *testing.T) {
	in := `{"model":"gpt","input":"hi","previous_response_id":"resp_1"}`
	req, err := DecodeResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeChatRequest(req); err == nil {
		t.Fatal("chat target accepted previous_response_id")
	}
	if _, err := EncodeAnthropicRequest(req, 4096, ""); err == nil {
		t.Fatal("anthropic target accepted previous_response_id")
	}
	stored := `{"model":"gpt","input":"hi","store":true}`
	req, err = DecodeResponsesRequest([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeChatRequest(req); err == nil {
		t.Fatal("chat target accepted store=true")
	}
	if _, err := EncodeAnthropicRequest(req, 4096, ""); err == nil {
		t.Fatal("anthropic target accepted store=true")
	}
	chatStored := `{"model":"gpt","messages":[{"role":"user","content":"hi"}],"store":true}`
	req, err = DecodeChatRequest([]byte(chatStored))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeAnthropicRequest(req, 4096, ""); err == nil {
		t.Fatal("chat store=true reached a stateless anthropic target")
	}
	out, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONMap(t, out)["store"]; got != true {
		t.Fatalf("responses store = %v, want true", got)
	}
}

func TestCodecRejectsInvalidToolArguments(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"not json"}}]}]}`
	if _, err := DecodeChatRequest([]byte(in)); err == nil {
		t.Fatal("invalid tool arguments were accepted")
	}
	in = `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"[1,2]"}}]}]}`
	if _, err := DecodeChatRequest([]byte(in)); err == nil {
		t.Fatal("non-object tool arguments were accepted")
	}
	in = `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f","input":[1,2]}]}]}`
	if _, err := DecodeAnthropicRequest([]byte(in)); err == nil {
		t.Fatal("non-object anthropic tool input was accepted")
	}
	if _, err := EncodeChatRequest(&ir.Request{
		Model: "m",
		Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{
			Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{Name: "f", Arguments: json.RawMessage(`[1,2]`)},
		}}}},
	}); err == nil {
		t.Fatal("IR validation accepted non-object tool arguments")
	}
}

func TestCodecPreservesInlineMedia(t *testing.T) {
	in := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"look"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	req, err := DecodeAnthropicRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	chatDoc := decodeJSONMap(t, chat)
	parts := chatDoc["messages"].([]any)[0].(map[string]any)["content"].([]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)
	if img["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("chat image url = %v", img["url"])
	}

	responses, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	respDoc := decodeJSONMap(t, responses)
	items := respDoc["input"].([]any)
	content := items[0].(map[string]any)["content"].([]any)
	part := content[1].(map[string]any)
	if part["type"] != "input_image" || part["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("responses image = %v", part)
	}

	// A Chat data URL carries the same inline bytes; Anthropic's URL source
	// expects a hosted URL, so the decoder folds it into the base64 source.
	chatImageBody := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]}]}`
	chatReq, err := DecodeChatRequest([]byte(chatImageBody))
	if err != nil {
		t.Fatal(err)
	}
	if img := chatReq.Messages[0].Content[0].Image; img.Data != "BBBB" || img.MediaType != "image/png" || img.URL != "" {
		t.Fatalf("chat data URL = %+v", img)
	}
	anthropicOut, err := EncodeAnthropicRequest(chatReq, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	source := decodeJSONMap(t, anthropicOut)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "BBBB" {
		t.Fatalf("anthropic source = %v", source)
	}

	// A provider file id has no Chat image_url equivalent: the block is
	// dropped with a warning rather than emitted as an empty URL.
	fileReq := &ir.Request{Model: "m", Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockImage, Image: &ir.Image{FileID: "file_1"}}},
	}}}
	if _, err := EncodeChatRequest(fileReq); err != nil {
		t.Fatal(err)
	}
	if len(fileReq.Warnings) == 0 || fileReq.Warnings[0].Kind != ir.WarningDropped {
		t.Fatalf("missing image file-id warning: %+v", fileReq.Warnings)
	}
}

func warningField(warnings ir.Warnings, field string) bool {
	for _, w := range warnings {
		if w.Field == field {
			return true
		}
	}
	return false
}

func TestCodecWarnsOnUnrepresentableFields(t *testing.T) {
	in := `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":true,"content":"x"}]}]}`
	req, err := DecodeAnthropicRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeChatRequest(req); err != nil {
		t.Fatal(err)
	}
	if !warningField(req.Warnings, "cache_control") || !warningField(req.Warnings, "tool_result.is_error") {
		t.Fatalf("chat warnings = %+v", req.Warnings)
	}

	audio := &ir.Request{Model: "m", Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockAudio, Audio: &ir.Audio{Data: "AAAA", Format: "wav"}}},
	}}}
	if _, err := EncodeAnthropicRequest(audio, 8, ""); err != nil {
		t.Fatal(err)
	}
	if !warningField(audio.Warnings, "audio") {
		t.Fatalf("anthropic warnings = %+v", audio.Warnings)
	}

	budget := 1024
	reasoning := &ir.Request{Model: "m", Reasoning: &ir.ReasoningConfig{BudgetTokens: &budget, Display: "omitted"}}
	if _, err := EncodeChatRequest(reasoning); err != nil {
		t.Fatal(err)
	}
	if !warningField(reasoning.Warnings, "reasoning.budget_tokens") || !warningField(reasoning.Warnings, "reasoning.display") {
		t.Fatalf("reasoning warnings = %+v", reasoning.Warnings)
	}
}

// TestCodecReportsMetadataLossOnce pins the single-emit contract for Chat
// metadata warnings: the decoder used to append them inside the metadata
// branch and again with the shared warning accumulator.
func TestCodecReportsMetadataLossOnce(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u1","attempts":3}}`
	req, err := DecodeChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, w := range req.Warnings {
		if w.Field == "metadata.attempts" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("metadata.attempts warnings = %d, want 1 (%+v)", count, req.Warnings)
	}
	if req.Metadata["user_id"] != "u1" {
		t.Fatalf("metadata = %v", req.Metadata)
	}
}

func TestChatReasoningIsDecoded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
	}{
		{"reasoning_content", "reasoning_content"},
		{"reasoning", "reasoning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			respBody := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
				`"message":{"role":"assistant","content":"answer","` + tc.field + `":"thought"},"finish_reason":"stop"}]}`
			resp, err := DecodeChatResponse([]byte(respBody))
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Blocks) != 2 || resp.Blocks[0].Type != ir.BlockReasoning || resp.Blocks[0].Reasoning.Text != "thought" {
				t.Fatalf("response blocks = %+v", resp.Blocks)
			}
			if resp.Blocks[1].Type != ir.BlockText || resp.Blocks[1].Text != "answer" {
				t.Fatalf("response answer = %+v", resp.Blocks[1])
			}

			reqBody := `{"model":"m","messages":[` +
				`{"role":"assistant","content":"answer","` + tc.field + `":"thought"},` +
				`{"role":"user","content":"next"}]}`
			req, err := DecodeChatRequest([]byte(reqBody))
			if err != nil {
				t.Fatal(err)
			}
			first := req.Messages[0].Content
			if len(first) != 2 || first[0].Type != ir.BlockReasoning || first[1].Type != ir.BlockText {
				t.Fatalf("request blocks = %+v", first)
			}
			if _, err := EncodeAnthropicRequest(req, 8, ""); err != nil {
				t.Fatal(err)
			}
			if !warningField(req.Warnings, "reasoning") {
				t.Fatalf("foreign reasoning did not warn: %+v", req.Warnings)
			}
		})
	}
}

func TestToolResultDropsAudioWithWarning(t *testing.T) {
	for name, encode := range map[string]func(*ir.Request) ([]byte, error){
		"chat": func(r *ir.Request) ([]byte, error) { return EncodeChatRequest(r) },
		"anthropic": func(r *ir.Request) ([]byte, error) {
			return EncodeAnthropicRequest(r, 8, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := &ir.Request{Model: "m", Messages: []ir.Message{{
				Role: ir.RoleUser,
				Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					CallID: "c1",
					Content: []ir.Block{
						ir.TextBlock("heard"),
						{Type: ir.BlockAudio, Audio: &ir.Audio{Data: "AAAA", Format: "wav"}},
					},
				}}},
			}}}
			if _, err := encode(req); err != nil {
				t.Fatal(err)
			}
			if !warningField(req.Warnings, "tool_result.audio") {
				t.Fatalf("missing audio warning: %+v", req.Warnings)
			}
		})
	}
}

func TestInlineFileDataURLRoundTrip(t *testing.T) {
	chatBody := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBER"}}]}]}`
	req, err := DecodeChatRequest([]byte(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	file := req.Messages[0].Content[0].File
	if file.FileData != "JVBER" || file.MediaType != "application/pdf" {
		t.Fatalf("decoded file = %+v", file)
	}
	anthropicOut, err := EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	source := decodeJSONMap(t, anthropicOut)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "application/pdf" || source["data"] != "JVBER" {
		t.Fatalf("anthropic source = %v", source)
	}

	anthropicBody := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBER"},"title":"a.pdf"}]}]}`
	anthropicReq, err := DecodeAnthropicRequest([]byte(anthropicBody))
	if err != nil {
		t.Fatal(err)
	}
	chatOut, err := EncodeChatRequest(anthropicReq)
	if err != nil {
		t.Fatal(err)
	}
	part := decodeJSONMap(t, chatOut)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	filePart, _ := part["file"].(map[string]any)
	if filePart["file_data"] != "data:application/pdf;base64,JVBER" {
		t.Fatalf("chat file = %v", part)
	}

	if _, err := EncodeChatRequest(&ir.Request{Model: "m", Messages: []ir.Message{{
		Role:    ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockFile, File: &ir.File{FileData: "JVBER"}}},
	}}}); err == nil {
		t.Fatal("inline file data without a media type was accepted")
	}
}

func TestAnthropicLiftsLateSystemMessage(t *testing.T) {
	in := `{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":"late rules"},` +
		`{"role":"user","content":"more"}]}`
	req, err := DecodeChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, out)
	if doc["system"] != "late rules" {
		t.Fatalf("system = %v, want the lifted text", doc["system"])
	}
	msgs, _ := doc["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the two adjacent user turns merged", doc["messages"])
	}
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("non-user role reached Anthropic messages: %v", doc["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("merged content = %v, want both user texts", content)
	}
	if !warningField(req.Warnings, "system") {
		t.Fatalf("missing late-system warning: %+v", req.Warnings)
	}
}

func TestAnthropicTemperatureClamp(t *testing.T) {
	temp := 1.7
	req := &ir.Request{Model: "m", Temperature: &temp,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}}}
	out, err := EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONMap(t, out)["temperature"]; got != float64(1) {
		t.Fatalf("temperature = %v, want the Anthropic ceiling 1", got)
	}
	if !warningField(req.Warnings, "temperature") {
		t.Fatalf("missing clamp warning: %+v", req.Warnings)
	}
	temp = 1
	req = &ir.Request{Model: "m", Temperature: &temp,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}}}
	out, err = EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONMap(t, out)["temperature"]; got != float64(1) {
		t.Fatalf("temperature = %v, want 1 unchanged", got)
	}
	if len(req.Warnings) != 0 {
		t.Fatalf("exact ceiling warned: %+v", req.Warnings)
	}
}

func TestAnthropicEffortMapping(t *testing.T) {
	for _, tc := range []struct {
		effort string
		want   string
		warn   bool
	}{
		{"minimal", "low", true},
		{"low", "low", false},
		{"xhigh", "xhigh", false},
	} {
		t.Run(tc.effort, func(t *testing.T) {
			req := &ir.Request{Model: "m",
				Reasoning: &ir.ReasoningConfig{Effort: tc.effort},
				Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}}}
			out, err := EncodeAnthropicRequest(req, 8, "")
			if err != nil {
				t.Fatal(err)
			}
			oc, _ := decodeJSONMap(t, out)["output_config"].(map[string]any)
			if oc["effort"] != tc.want {
				t.Fatalf("effort = %v, want %q", oc["effort"], tc.want)
			}
			if got := warningField(req.Warnings, "reasoning.effort"); got != tc.warn {
				t.Fatalf("warning = %v, want %v (%+v)", got, tc.warn, req.Warnings)
			}
		})
	}
}

func TestResponsesIncludeLogprobs(t *testing.T) {
	in := `{"model":"m","input":"hi","include":["message.output_text.logprobs"]}`
	req, err := DecodeResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if warningField(req.Warnings, "include") {
		t.Fatalf("a mapped logprobs include was reported as dropped: %+v", req.Warnings)
	}
	out, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONMap(t, out)["logprobs"]; got != true {
		t.Fatalf("chat logprobs = %v, want true", got)
	}
	if !warningField(req.Warnings, "logprobs") {
		t.Fatalf("missing response-side logprobs warning: %+v", req.Warnings)
	}

	in = `{"model":"m","input":"hi","include":["message.output_text.logprobs","reasoning.encrypted_content"]}`
	req, err = DecodeResponsesRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(req.Warnings, "include") {
		t.Fatalf("an unmapped include was not reported: %+v", req.Warnings)
	}
}

func TestLegacyChatFunctionsTranslate(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"lookup","description":"find","parameters":{"type":"object"}}],` +
		`"function_call":{"name":"lookup"}}`
	req, err := DecodeChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" || req.Tools[0].Description != "find" {
		t.Fatalf("legacy functions = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceTool || req.ToolChoice.Name != "lookup" {
		t.Fatalf("legacy function_call = %+v", req.ToolChoice)
	}
	out, err := EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, out)
	tools, _ := doc["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("translated tools = %v", doc["tools"])
	}
	tc, _ := doc["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "lookup" {
		t.Fatalf("translated tool_choice = %v", doc["tool_choice"])
	}
}

func TestLegacyChatFunctionCallPairsWithResult(t *testing.T) {
	in := `{"model":"m","messages":[` +
		`{"role":"assistant","content":"","function_call":{"name":"lookup","arguments":"{\"q\":1}"}},` +
		`{"role":"function","name":"lookup","content":"ok"}]}`
	req, err := DecodeChatRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	call := req.Messages[0].Content[len(req.Messages[0].Content)-1]
	if call.Type != ir.BlockToolCall || call.ToolCall.ID != "lookup" || call.ToolCall.Name != "lookup" {
		t.Fatalf("legacy call = %+v", call)
	}
	result := req.Messages[1].Content[0]
	if result.Type != ir.BlockToolResult || result.ToolResult.CallID != "lookup" {
		t.Fatalf("legacy result = %+v", result)
	}
	if _, err := EncodeAnthropicRequest(req, 8, ""); err != nil {
		t.Fatalf("paired legacy turns did not translate: %v", err)
	}
}

func TestLegacyChatResponseFunctionCall(t *testing.T) {
	resp, err := DecodeChatResponse([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","function_call":{"name":"lookup","arguments":"{\"q\":1}"}},"finish_reason":"function_call"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Type != ir.BlockToolCall {
		t.Fatalf("blocks = %+v", resp.Blocks)
	}
	call := resp.Blocks[0].ToolCall
	if call.ID != "lookup" || call.Name != "lookup" || string(call.Arguments) != `{"q":1}` {
		t.Fatalf("call = %+v", call)
	}
	if resp.Finish != ir.FinishToolCalls {
		t.Fatalf("finish = %q, want tool_calls", resp.Finish)
	}
}

func TestChatWarnsOnUnrepresentableFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		field string
		want  bool
	}{
		{"prediction", `{"model":"m","messages":[{"role":"user","content":"hi"}],"prediction":{"type":"content","content":"x"}}`, "prediction", true},
		{"audio output", `{"model":"m","messages":[{"role":"user","content":"hi"}],"audio":{"format":"wav","voice":"alloy"}}`, "audio", true},
		{"audio modality", `{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":["text","audio"]}`, "modalities", true},
		{"text modality only", `{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":["text"]}`, "modalities", false},
		{"web search options", `{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":{}}`, "web_search_options", true},
		{"tool choice beats legacy selector", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"none","function_call":"auto"}`, "function_call", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := DecodeChatRequest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := warningField(req.Warnings, tc.field); got != tc.want {
				t.Fatalf("%s warning = %v, want %v (%+v)", tc.field, got, tc.want, req.Warnings)
			}
		})
	}
}

func TestCodecRejectsNonFunctionTool(t *testing.T) {
	chat := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`
	if _, err := DecodeChatRequest([]byte(chat)); err == nil {
		t.Fatal("chat non-function tool was accepted")
	}
	anthropic := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"ws"}]}`
	if _, err := DecodeAnthropicRequest([]byte(anthropic)); err == nil {
		t.Fatal("anthropic provider tool was accepted")
	}
	responses := `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`
	if _, err := DecodeResponsesRequest([]byte(responses)); err == nil {
		t.Fatal("responses provider tool was accepted")
	}
}

func TestErrorEnvelopes(t *testing.T) {
	anthropicErr := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	e, err := DecodeAnthropicError(anthropicErr)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicError(e)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got["type"] != "error" || got["error"].(map[string]any)["type"] != "overloaded_error" {
		t.Fatalf("anthropic error = %v", got)
	}

	openaiErr := []byte(`{"error":{"message":"bad","type":"invalid_request_error","code":"x"}}`)
	e, err = DecodeResponsesError(openaiErr)
	if err != nil {
		t.Fatal(err)
	}
	if e.Code != "x" || e.Message != "bad" {
		t.Fatalf("openai error = %+v", e)
	}
}

func TestErrorTypeTranslation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		from ir.Protocol
		to   ir.Protocol
		want string
	}{
		// OpenAI-family clients historically receive the provider's own type;
		// only the new inbound Anthropic contract maps into Anthropic's
		// documented vocabulary.
		{"anthropic overloaded to openai", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, ir.Anthropic, ir.Chat, "overloaded_error"},
		{"anthropic billing to responses", `{"type":"error","error":{"type":"billing_error","message":"pay"}}`, ir.Anthropic, ir.Responses, "billing_error"},
		{"openai quota to anthropic", `{"error":{"type":"insufficient_quota","message":"pay"}}`, ir.Chat, ir.Anthropic, "billing_error"},
		{"openai server to anthropic", `{"error":{"type":"server_error","message":"boom"}}`, ir.Chat, ir.Anthropic, "api_error"},
		{"shared rate limit passes through", `{"error":{"type":"rate_limit_error","message":"slow"}}`, ir.Chat, ir.Anthropic, "rate_limit_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e *ir.Error
			var err error
			if tc.from == ir.Anthropic {
				e, err = DecodeAnthropicError([]byte(tc.in))
			} else {
				e, err = DecodeResponsesError([]byte(tc.in))
			}
			if err != nil {
				t.Fatal(err)
			}
			var out []byte
			if tc.to == ir.Anthropic {
				out, err = EncodeAnthropicError(e)
			} else {
				out, err = EncodeOpenAIError(e)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := decodeJSONMap(t, out)
			if got["error"].(map[string]any)["type"] != tc.want {
				t.Fatalf("type = %v, want %q (body %s)", got["error"], tc.want, out)
			}
		})
	}
}

func TestAnthropicThinkingRoundTrip(t *testing.T) {
	in := `{
		"model":"claude","max_tokens":1024,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"redacted_thinking","data":"enc"},{"type":"text","text":"hello"}]}
		]
	}`
	req, err := DecodeAnthropicRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	msgs := got["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "thinking" || blocks[0].(map[string]any)["signature"] != "sig" {
		t.Fatalf("thinking block = %v", blocks[0])
	}
	if blocks[1].(map[string]any)["type"] != "redacted_thinking" || blocks[1].(map[string]any)["data"] != "enc" {
		t.Fatalf("redacted block = %v", blocks[1])
	}
}

func TestAnthropicCacheControlPreserved(t *testing.T) {
	in := `{
		"model":"claude","max_tokens":1024,
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]
	}`
	req, err := DecodeAnthropicRequest([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 4096, "")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	system := got["system"].([]any)[0].(map[string]any)
	if system["cache_control"].(map[string]any)["ttl"] != "1h" {
		t.Fatalf("system cache_control = %v", system)
	}
	msg := got["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if msg["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("message cache_control = %v", msg)
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	if tool["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("tool cache_control = %v", tool)
	}
}

func TestIRValidateRejectsUnknownRole(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: "robot"}}}
	if err := ir.ValidateRequest(req); err == nil {
		t.Fatal("unknown role accepted")
	}
}

// TestResponseArraysAreBoundedBeforeDecode pins the response-side array bound:
// a provider body with an oversized choices/output array must be rejected
// before the typed decode materializes it, while the exact limit still
// decodes.
func TestResponseArraysAreBoundedBeforeDecode(t *testing.T) {
	array := emptyJSONArray

	t.Run("chat choices at limit", func(t *testing.T) {
		body := `{"id":"c1","choices":` + array(metrics.ContentPartMax) + `}`
		if _, err := DecodeChatResponse([]byte(body)); err != nil {
			t.Fatalf("body at the limit was rejected: %v", err)
		}
	})
	t.Run("chat choices over limit", func(t *testing.T) {
		body := `{"id":"c1","choices":` + array(metrics.ContentPartMax+1) + `}`
		_, err := DecodeChatResponse([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "exceed") {
			t.Fatalf("over-bound choices error = %v", err)
		}
	})
	t.Run("responses output at limit", func(t *testing.T) {
		body := `{"id":"r1","status":"completed","output":` + array(metrics.ContentPartMax) + `}`
		if _, err := DecodeResponsesResponse([]byte(body)); err != nil {
			t.Fatalf("body at the limit was rejected: %v", err)
		}
	})
	t.Run("responses output over limit", func(t *testing.T) {
		body := `{"id":"r1","output":` + array(metrics.ContentPartMax+1) + `}`
		_, err := DecodeResponsesResponse([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "exceed") {
			t.Fatalf("over-bound output error = %v", err)
		}
	})
}

// emptyJSONArray builds the cheapest amplifying array: n empty objects.
func emptyJSONArray(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteByte(']')
	return b.String()
}

// TestWireCollectionsAreBoundedBeforeDecode pins every collection preflight
// the codecs run themselves: they are reachable without the proxy's request
// metadata scan, and a typed decode must never be the first size check.
func TestWireCollectionsAreBoundedBeforeDecode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		decode func([]byte) error
		want   string
	}{
		{
			"chat request messages", `{"model":"m","messages":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeChatRequest(b); return err }, "messages exceeds",
		},
		{
			"chat request tools", `{"model":"m","messages":[],"tools":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeChatRequest(b); return err }, "tools exceeds",
		},
		{
			"chat request legacy functions", `{"model":"m","messages":[],"functions":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeChatRequest(b); return err }, "functions exceeds",
		},
		{
			"chat request stop", `{"model":"m","messages":[],"stop":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeChatRequest(b); return err }, "stop exceeds",
		},
		{
			"chat response tool calls", `{"id":"c1","choices":[{"message":{"role":"assistant","tool_calls":` + emptyJSONArray(metrics.ContentPartMax+1) + `}}]}`,
			func(b []byte) error { _, err := DecodeChatResponse(b); return err }, "message.tool_calls exceeds",
		},
		{
			"responses request tools", `{"model":"m","input":"hi","tools":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeResponsesRequest(b); return err }, "tools exceeds",
		},
		{
			"responses request include", `{"model":"m","input":"hi","include":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeResponsesRequest(b); return err }, "include exceeds",
		},
		{
			"responses input summary", `{"model":"m","input":[{"type":"reasoning","summary":` + emptyJSONArray(metrics.ContentPartMax+1) + `}]}`,
			func(b []byte) error { _, err := DecodeResponsesRequest(b); return err }, "input.summary exceeds",
		},
		{
			"responses output summary", `{"id":"r1","output":[{"type":"reasoning","summary":` + emptyJSONArray(metrics.ContentPartMax+1) + `}]}`,
			func(b []byte) error { _, err := DecodeResponsesResponse(b); return err }, "output.summary exceeds",
		},
		{
			"anthropic request messages", `{"model":"m","max_tokens":8,"messages":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeAnthropicRequest(b); return err }, "messages exceeds",
		},
		{
			"anthropic stop sequences", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stop_sequences":` + emptyJSONArray(WireCollectionMax+1) + `}`,
			func(b []byte) error { _, err := DecodeAnthropicRequest(b); return err }, "stop_sequences exceeds",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.decode([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
