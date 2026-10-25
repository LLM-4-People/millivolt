package format

// Round 12 A1 regression: request-history tool ids share one request-scoped
// namespace. Synthesized ids must be unique across messages and cannot collide
// with an explicit id; duplicate explicit ids fail closed.

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicToolUseBlockJSON(id, name string) string {
	if id == "" {
		return `{"type":"tool_use","name":"` + name + `","input":{}}`
	}
	return `{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{}}`
}

func anthropicRequestWithToolUses(blocks ...string) string {
	return `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"assistant","content":[` + blocks[0] + `]},` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[` + blocks[1] + `]}]}`
}

func requestToolCallIDs(req *ir.Request) []string {
	var ids []string
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Type == ir.BlockToolCall && block.ToolCall != nil {
				ids = append(ids, block.ToolCall.ID)
			}
		}
	}
	return ids
}

func TestRequestToolIDsAreUnique(t *testing.T) {
	t.Run("two id-less Anthropic turns", func(t *testing.T) {
		body := anthropicRequestWithToolUses(anthropicToolUseBlockJSON("", "f"), anthropicToolUseBlockJSON("", "g"))
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		ids := requestToolCallIDs(req)
		if len(ids) != 2 || ids[0] != "toolu_proxy_0" || ids[1] != "toolu_proxy_0_1" {
			t.Fatalf("IR ids = %v, want toolu_proxy_0 and toolu_proxy_0_1", ids)
		}
		if !warningField(req.Warnings, "tool_use.id") {
			t.Fatalf("generated id approximation warning missing: %+v", req.Warnings)
		}
		for _, target := range []ir.Protocol{ir.Chat, ir.Responses} {
			out, err := EncodeRequest(target, req, 16, "")
			if err != nil {
				t.Fatalf("encode %s: %v", target, err)
			}
			for _, id := range ids {
				if strings.Count(string(out), `"`+id+`"`) != 1 {
					t.Fatalf("%s wire does not carry %q exactly once: %s", target, id, out)
				}
			}
		}
	})

	t.Run("explicit then generated Chat collision", func(t *testing.T) {
		body := `{"model":"m","messages":[` +
			`{"role":"user","content":"start"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_3_0","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
			`{"role":"user","content":"x"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"g","arguments":"{}"}}]}]}`
		req, err := DecodeChatRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		ids := requestToolCallIDs(req)
		if len(ids) != 2 || ids[0] != "call_3_0" || ids[1] != "call_3_0_1" {
			t.Fatalf("Chat IR ids = %v, want call_3_0 and call_3_0_1", ids)
		}
		if !warningField(req.Warnings, "tool_calls.id") {
			t.Fatalf("generated Chat id warning missing: %+v", req.Warnings)
		}
		out, err := EncodeRequest(ir.Anthropic, req, 16, "")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(out), "call_3_0\"") != 1 || !strings.Contains(string(out), "call_3_0_1") {
			t.Fatalf("Anthropic wire ids = %s", out)
		}
	})

	// The explicit id appears after the id-less call. The pre-scan must reserve
	// it before the first call synthesizes, or a valid request is rejected with
	// a false duplicate.
	t.Run("generated then explicit Chat collision", func(t *testing.T) {
		body := `{"model":"m","messages":[` +
			`{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},` +
			`{"role":"user","content":"x"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_0_0","type":"function","function":{"name":"g","arguments":"{}"}}]}]}`
		req, err := DecodeChatRequest([]byte(body))
		if err != nil {
			t.Fatalf("generated-then-explicit Chat request became a failure: %v", err)
		}
		ids := requestToolCallIDs(req)
		if len(ids) != 2 || ids[0] != "call_0_0_1" || ids[1] != "call_0_0" {
			t.Fatalf("Chat IR ids = %v, want call_0_0_1 and call_0_0", ids)
		}
		if !warningField(req.Warnings, "tool_calls.id") {
			t.Fatalf("generated Chat id warning missing: %+v", req.Warnings)
		}
	})

	t.Run("generated then explicit Anthropic collision", func(t *testing.T) {
		body := anthropicRequestWithToolUses(anthropicToolUseBlockJSON("", "f"), anthropicToolUseBlockJSON("toolu_proxy_0", "g"))
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatalf("generated-then-explicit Anthropic request became a failure: %v", err)
		}
		ids := requestToolCallIDs(req)
		if len(ids) != 2 || ids[0] != "toolu_proxy_0_1" || ids[1] != "toolu_proxy_0" {
			t.Fatalf("Anthropic IR ids = %v, want toolu_proxy_0_1 and toolu_proxy_0", ids)
		}
		if !warningField(req.Warnings, "tool_use.id") {
			t.Fatalf("generated Anthropic id warning missing: %+v", req.Warnings)
		}
	})

	t.Run("base64 document content decoy is ignored", func(t *testing.T) {
		data := base64.StdEncoding.EncodeToString([]byte(
			`{"type":"tool_use","id":"embedded","name":"decoy","input":{}}`))
		// A base64 document carries a stray content array whose tool id equals
		// the later real call. Only a content-source document decodes that
		// array, so the pre-scan must ignore it. The unscoped pre-scan on
		// bb060c0 walked every document source and rejected this body.
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":[{"type":"text","text":"read"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + data + `","content":[{"type":"tool_use","id":"call_real","name":"decoy","input":{}}]}}]},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"call_real","name":"f","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_real","content":"ok"}]}]}`
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatalf("base64 document decoy rejected: %v", err)
		}
		if ids := requestToolCallIDs(req); len(ids) != 1 || ids[0] != "call_real" {
			t.Fatalf("IR ids = %v, want the real id unchanged with no suffix", ids)
		}
	})

	for _, tc := range []struct {
		name   string
		body   string
		decode func([]byte) (*ir.Request, error)
	}{
		{
			name: "duplicate explicit Chat id",
			body: `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}},` +
				`{"id":"call_x","type":"function","function":{"name":"g","arguments":"{}"}}]}]}`,
			decode: DecodeChatRequest,
		},
		{
			name:   "duplicate explicit Anthropic id",
			body:   anthropicRequestWithToolUses(anthropicToolUseBlockJSON("toolu_x", "f"), anthropicToolUseBlockJSON("toolu_x", "g")),
			decode: DecodeAnthropicRequest,
		},
		{
			name: "duplicate explicit Responses id",
			body: `{"model":"m","input":[` +
				`{"type":"function_call","call_id":"call_x","name":"f","arguments":"{}"},` +
				`{"type":"function_call","call_id":"call_x","name":"g","arguments":"{}"}]}`,
			decode: DecodeResponsesRequest,
		},
		{
			name: "content-source document duplicate",
			body: `{"model":"m","max_tokens":16,"messages":[` +
				`{"role":"user","content":[{"type":"document","source":{"type":"content","media_type":"text/plain","content":[{"type":"text","text":"x"},{"type":"tool_use","id":"call_real","name":"nested","input":{}}]}}]},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"call_real","name":"f","input":{}}]}]}`,
			decode: DecodeAnthropicRequest,
		},
		{
			name: "nested tool-result duplicate",
			body: `{"model":"m","max_tokens":16,"messages":[` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"outer","name":"outer","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"outer","content":[{"type":"tool_use","id":"call_real","name":"nested","input":{}}]}]},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"call_real","name":"f","input":{}}]}]}`,
			decode: DecodeAnthropicRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.decode([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), `duplicate tool call id "`) {
				t.Fatalf("error = %v, want the duplicate tool id rejection", err)
			}
		})
	}

	t.Run("distinct explicit ids unchanged", func(t *testing.T) {
		body := anthropicRequestWithToolUses(anthropicToolUseBlockJSON("toolu_a", "f"), anthropicToolUseBlockJSON("toolu_b", "g"))
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if ids := requestToolCallIDs(req); len(ids) != 2 || ids[0] != "toolu_a" || ids[1] != "toolu_b" {
			t.Fatalf("distinct ids = %v, want toolu_a/toolu_b", ids)
		}
		if warningField(req.Warnings, "tool_use.id") {
			t.Fatalf("explicit ids warned: %+v", req.Warnings)
		}
	})
}
