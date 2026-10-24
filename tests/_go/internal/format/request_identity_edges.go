package format

// Round 4 batch 10 regressions (A3, A4, A-U1, A-U2): an empty Chat target
// message list fails closed, a merged user turn keeps its name, an absent
// Responses input is distinct from null, and response_format names stay inside
// the target identifier class.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatTargetRejectsEmptyMessages(t *testing.T) {
	for _, input := range []string{`[]`, `""`} {
		body := `{"model":"m","input":` + input + `}`
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatalf("decode %s: %v", input, err)
		}
		if _, err := EncodeChatRequest(req); err == nil || !strings.Contains(err.Error(), "chat target requires") {
			t.Fatalf("input %s -> Chat error = %v, want the empty-message guard", input, err)
		}
	}

	// The direct Anthropic guard remains the twin contract.
	if _, err := EncodeAnthropicRequest(&ir.Request{Model: "m"}, 16, ""); err == nil {
		t.Fatal("anthropic target accepted an empty message list")
	}
}

func TestChatMergedUserTurnKeepsName(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"sunny"},` +
		`{"role":"user","name":"alice","content":"thanks"}]}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var merged *ir.Message
	for i := range req.Messages {
		if req.Messages[i].Role == ir.RoleUser {
			merged = &req.Messages[i]
		}
	}
	if merged == nil || merged.Name != "alice" {
		t.Fatalf("merged user name = %+v, want alice", merged)
	}

	chatBody, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeJSONMap(t, chatBody)["messages"].([]any)
	user := messages[len(messages)-1].(map[string]any)
	if user["role"] != "user" || user["name"] != "alice" {
		t.Fatalf("Chat user turn lost its name: %v", user)
	}

	anthropicBody, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeAnthropicRequest(anthropicBody, 16, ""); err != nil {
		t.Fatal(err)
	}
	if !warningField(anthropicBody.Warnings, "messages.name") {
		t.Fatalf("Anthropic target did not record the name loss: %+v", anthropicBody.Warnings)
	}
}

func TestChatConflictingMergedUserNamesWarnOnce(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"sunny"},` +
		`{"role":"user","name":"alice","content":"thanks"},` +
		`{"role":"user","name":"bob","content":"more"}]}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var merged *ir.Message
	for i := range req.Messages {
		if req.Messages[i].Role == ir.RoleUser {
			merged = &req.Messages[i]
		}
	}
	if merged == nil || merged.Name != "alice" {
		t.Fatalf("merged user name = %+v, want the first name", merged)
	}
	count := 0
	for _, warning := range req.Warnings {
		if warning.Field == "messages.name" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("conflicting-name warnings = %d, want one: %+v", count, req.Warnings)
	}
}

func TestResponsesInstructionsOnlyTranslates(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","instructions":"be nice"}`))
	if err != nil {
		t.Fatalf("absent input was rejected: %v", err)
	}
	if len(req.Messages) != 0 {
		t.Fatalf("instructions-only request decoded messages: %+v", req.Messages)
	}
	chatBody, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeJSONMap(t, chatBody)["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("Chat messages = %v, want the instructions system message", messages)
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be nice" {
		t.Fatalf("Chat system message = %v", system)
	}

	responsesBody, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := decodeJSONMap(t, responsesBody)["input"].([]any)
	if len(input) != 0 {
		t.Fatalf("Responses input = %v, want an empty array", input)
	}

	if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":null}`)); err == nil {
		t.Fatal("explicit null input was accepted")
	}
}

func TestResponseFormatNameTargetVocabulary(t *testing.T) {
	base := func(name string) *ir.Request {
		return &ir.Request{
			Model:    "m",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
			ResponseFormat: &ir.ResponseFormat{
				Type: "json_schema", Name: name, Schema: []byte(`{"type":"object"}`),
			},
		}
	}
	encoders := map[string]func(*ir.Request) ([]byte, error){
		"chat":      EncodeChatRequest,
		"responses": EncodeResponsesRequest,
	}
	for name, encode := range encoders {
		t.Run(name, func(t *testing.T) {
			for _, accepted := range []string{"a", strings.Repeat("a", 64), "name_1-2"} {
				if _, err := encode(base(accepted)); err != nil {
					t.Fatalf("name %q rejected: %v", accepted, err)
				}
			}
			for _, rejected := range []string{strings.Repeat("a", 65), "bad name", "bad.name"} {
				if _, err := encode(base(rejected)); err == nil || !strings.Contains(err.Error(), "response_format.name") {
					t.Fatalf("name %q error = %v, want the identifier rejection", rejected, err)
				}
			}
			body, err := encode(base(""))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"name":"response"`) {
				t.Fatalf("empty name did not synthesize the default: %s", body)
			}
		})
	}
}
