package format

// Responses namespace tools: a Responses client (Codex) groups MCP server tools
// into a "namespace" tool with nested function members. A target without
// namespaces receives one flattened function per member, and a returned call
// names its group again so the client can dispatch it.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const namespaceRequest = `{"model":"m","input":"hi","tools":[` +
	`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},` +
	`{"type":"namespace","name":"mcp__context7","description":"docs server","tools":[` +
	`{"type":"function","name":"query_docs","description":"query","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]}`

const namespaceChatResponse = `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
	`{"id":"call_1","type":"function","function":{"name":"mcp__context7__query_docs","arguments":"{\"q\":\"auth\"}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

const namespaceChatStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"mcp__context7__query_docs","arguments":"{\"q\":\"auth\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

func decodeNamespaceRequest(t *testing.T) *ir.Request {
	t.Helper()
	req, err := DecodeResponsesRequest([]byte(namespaceRequest))
	if err != nil {
		t.Fatalf("namespace request was rejected: %v", err)
	}
	return req
}

// TestResponsesNamespaceToolsFlatten verifies the declared tools: the group is
// expanded, the member keeps its declared name and group, and the outbound wire
// receives the flat spelling the Chat class accepts.
func TestResponsesNamespaceToolsFlatten(t *testing.T) {
	req := decodeNamespaceRequest(t)
	if len(req.Tools) != 2 {
		t.Fatalf("expected 2 flattened tools, got %d: %+v", len(req.Tools), req.Tools)
	}
	member := req.Tools[1]
	if member.Name != "query_docs" || member.Namespace != "mcp__context7" {
		t.Fatalf("member identity was not preserved: %+v", member)
	}
	wire := member.ModelName()
	if wire != "mcp__context7__query_docs" {
		t.Fatalf("flat wire name = %q", wire)
	}
	if !strings.Contains(member.Description, "docs server") || !strings.Contains(member.Description, "query") {
		t.Fatalf("group and member descriptions were not folded: %q", member.Description)
	}
	if !openAIIdentifierPattern.MatchString(wire) {
		t.Fatalf("flat wire name leaves the Chat name class: %q", wire)
	}

	chat, _, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(namespaceRequest), 0, "")
	if err != nil {
		t.Fatalf("translate to Chat: %v", err)
	}
	var out struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(chat, &out); err != nil {
		t.Fatalf("decode translated tools: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("translated tool count = %d", len(out.Tools))
	}
	if got := out.Tools[1].Function.Name; got != wire {
		t.Fatalf("translated member name = %q, want %q", got, wire)
	}
	if out.Tools[1].Type != "function" {
		t.Fatalf("translated member type = %q", out.Tools[1].Type)
	}
}

// TestResponsesNamespaceNonStreamCall verifies the inbound response: a flat
// call from the Chat target names its group again in the client's wire.
func TestResponsesNamespaceNonStreamCall(t *testing.T) {
	req := decodeNamespaceRequest(t)
	out, _, _, err := TranslateResponseBetweenIR(ir.Responses, ir.Chat, []byte(namespaceChatResponse), nil,
		ResponseOptions{ToolNames: ir.ToolNamesByWireName(req.Tools)})
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	var resp struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode translated response: %v", err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" {
		t.Fatalf("unexpected output: %s", out)
	}
	call := resp.Output[0]
	if call.Name != "query_docs" || call.Namespace != "mcp__context7" {
		t.Fatalf("call did not name its group: %+v", call)
	}
	if call.Arguments != `{"q":"auth"}` {
		t.Fatalf("arguments changed: %q", call.Arguments)
	}
}

// TestResponsesNamespaceStreamCall verifies every streamed function-call item
// shape (added, done, terminal output) carries the namespace, because the
// client dispatches on it.
func TestResponsesNamespaceStreamCall(t *testing.T) {
	req := decodeNamespaceRequest(t)
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Responses, ir.Chat, &buf, strings.NewReader(namespaceChatStream), nil, nil,
		StreamOptions{ToolNames: ir.ToolNamesByWireName(req.Tools)})
	if err != nil {
		t.Fatalf("translate stream: %v", err)
	}
	wire := buf.String()
	if !strings.Contains(wire, `"namespace":"mcp__context7"`) {
		t.Fatalf("streamed call lost its namespace: %s", wire)
	}
	if strings.Contains(wire, `"name":"mcp__context7__query_docs"`) {
		t.Fatalf("streamed call kept the flat name: %s", wire)
	}
	if !strings.Contains(wire, `"name":"query_docs"`) {
		t.Fatalf("streamed call lost its declared name: %s", wire)
	}
}

// TestResponsesNamespaceFlatNameCollisions verifies two pairs that flatten to
// the same string stay separately addressable, and a pair longer than the Chat
// class stays inside it.
func TestResponsesNamespaceFlatNameCollisions(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[` +
		`{"type":"namespace","name":"a","tools":[{"type":"function","name":"b__c","parameters":{}}]},` +
		`{"type":"namespace","name":"a__b","tools":[{"type":"function","name":"c","parameters":{}}]}]}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("colliding namespaces were rejected: %v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tool count = %d", len(req.Tools))
	}
	if req.Tools[0].ModelName() == req.Tools[1].ModelName() {
		t.Fatalf("colliding pairs share one wire name: %q", req.Tools[0].ModelName())
	}
	for _, tool := range req.Tools {
		if !openAIIdentifierPattern.MatchString(tool.ModelName()) {
			t.Fatalf("wire name leaves the Chat class: %q", tool.ModelName())
		}
	}

	long := strings.Repeat("n", 40)
	longTool := strings.Repeat("t", 40)
	longBody := `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"` + long +
		`","tools":[{"type":"function","name":"` + longTool + `","parameters":{}}]}]}`
	longReq, err := DecodeResponsesRequest([]byte(longBody))
	if err != nil {
		t.Fatalf("long pair was rejected: %v", err)
	}
	if name := longReq.Tools[0].ModelName(); !openAIIdentifierPattern.MatchString(name) {
		t.Fatalf("truncated wire name leaves the Chat class (%d): %q", len(name), name)
	}
}

// TestResponsesNamespaceNeverDisplacesPlainTool verifies a namespace pair never
// takes a plain tool's spelling, in either declaration order, and that a named
// choice and a replayed call resolve to the pair's real suffix.
func TestResponsesNamespaceNeverDisplacesPlainTool(t *testing.T) {
	plain := `{"type":"function","name":"a__b__c","parameters":{}}`
	group := `{"type":"namespace","name":"a__b","tools":[{"type":"function","name":"c","parameters":{}}]}`
	for name, tools := range map[string]string{
		"plain first": plain + "," + group,
		"pair first":  group + "," + plain,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","input":[` +
				`{"type":"function_call","call_id":"call_1","name":"c","namespace":"a__b","arguments":"{}"}],` +
				`"tool_choice":{"type":"function","name":"c","namespace":"a__b"},` +
				`"tools":[` + tools + `]}`
			req, err := DecodeResponsesRequest([]byte(body))
			if err != nil {
				t.Fatalf("request was rejected: %v", err)
			}
			plainWire, pairWire := "", ""
			for _, tool := range req.Tools {
				if tool.Namespace == "" {
					plainWire = tool.ModelName()
				} else {
					pairWire = tool.ModelName()
				}
			}
			if plainWire != "a__b__c" {
				t.Fatalf("plain tool wire name = %q", plainWire)
			}
			if pairWire != "a__b__c_2" {
				t.Fatalf("pair wire name = %q, want the suffix so it cannot displace the plain tool", pairWire)
			}
			if req.ToolChoice == nil || req.ToolChoice.Name != "a__b__c_2" {
				t.Fatalf("tool choice = %+v, want the pair's suffix", req.ToolChoice)
			}
			var call *ir.ToolCall
			for _, message := range req.Messages {
				for _, block := range message.Content {
					if block.Type == ir.BlockToolCall {
						call = block.ToolCall
					}
				}
			}
			if call == nil || call.Name != "a__b__c_2" {
				t.Fatalf("history call = %+v, want the pair's suffix", call)
			}
		})
	}
}

// TestResponsesNamespaceHistoryCall verifies a replayed namespaced call decodes
// to the flat spelling, so a Chat target history validates.
func TestResponsesNamespaceHistoryCall(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"call_1","name":"query_docs","namespace":"mcp__context7","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"done"}],"tools":[` +
		`{"type":"namespace","name":"mcp__context7","tools":[{"type":"function","name":"query_docs","parameters":{}}]}]}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("namespaced history was rejected: %v", err)
	}
	var call *ir.ToolCall
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == ir.BlockToolCall {
				call = block.ToolCall
			}
		}
	}
	if call == nil {
		t.Fatal("history call was dropped")
	}
	if call.Name != "mcp__context7__query_docs" {
		t.Fatalf("history call name = %q", call.Name)
	}
}

// TestResponsesNamespaceToolChoice verifies a named choice inside a group is
// flattened for the outbound wire.
func TestResponsesNamespaceToolChoice(t *testing.T) {
	body := strings.Replace(namespaceRequest, `"input":"hi"`,
		`"input":"hi","tool_choice":{"type":"function","name":"query_docs","namespace":"mcp__context7"}`, 1)
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("namespaced tool_choice was rejected: %v", err)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ToolChoiceTool {
		t.Fatalf("tool choice was not decoded: %+v", req.ToolChoice)
	}
	if req.ToolChoice.Name != "mcp__context7__query_docs" {
		t.Fatalf("tool choice name = %q", req.ToolChoice.Name)
	}
}

// TestResponsesAdditionalToolsDecoded verifies the tool list a client carries
// as additional_tools input items reaches the outbound wire, including a
// namespace member, and that an oversized member array is rejected before the
// typed decode.
func TestResponsesAdditionalToolsDecoded(t *testing.T) {
	body := `{"model":"m","input":[` +
		`{"type":"additional_tools","role":"developer","tools":[` +
		`{"type":"namespace","name":"functions","tools":[` +
		`{"type":"function","name":"wait","parameters":{}},` +
		`{"type":"function","name":"exec","parameters":{}}]}]},` +
		`{"type":"message","role":"user","content":"hi"}]}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("additional_tools was rejected: %v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tool count = %d, want 2: %+v", len(req.Tools), req.Tools)
	}
	if req.Tools[0].ModelName() != "functions__wait" || req.Tools[0].Namespace != "functions" {
		t.Fatalf("first tool = %+v", req.Tools[0])
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
}

// TestResponsesToolArraysBounded verifies a nested member array is bounded
// before the typed decode, at the same ceiling as the top-level array.
func TestResponsesToolArraysBounded(t *testing.T) {
	member := `{"type":"function","name":"f","parameters":{}}`
	var b strings.Builder
	b.WriteString(`{"type":"namespace","name":"ns","tools":[`)
	for i := 0; i <= WireCollectionMax; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(member)
	}
	b.WriteString(`]}`)
	body := `{"model":"m","input":"hi","tools":[` + b.String() + `]}`
	if _, err := DecodeResponsesRequest([]byte(body)); err == nil {
		t.Fatal("an over-bound namespace member array was accepted")
	}
}

// TestResponsesProviderToolDropped verifies a provider-executed tool (the
// web_search a Responses client always sends) is dropped with a warning instead
// of failing the whole turn, at the top level and inside a group.
func TestResponsesProviderToolDropped(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[` +
		`{"type":"function","name":"exec_command","parameters":{}},` +
		`{"type":"web_search"},` +
		`{"type":"custom","name":"exec","format":{"type":"grammar"}},` +
		`{"type":"tool_search"},` +
		`{"type":"namespace","name":"ns","tools":[{"type":"web_search"}]}]}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("provider tool failed the request: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "exec_command" {
		t.Fatalf("provider tools were not dropped: %+v", req.Tools)
	}
	for _, field := range []string{"tool.web_search", "tool.custom", "tool.tool_search"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("%s drop was not reported: %+v", field, req.Warnings)
		}
	}
}
