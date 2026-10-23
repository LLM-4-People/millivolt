package format

// Round 2 TC2-1 regression: every non-stream Responses output item carries the
// type-appropriate required id, even when the source protocol supplied none.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesOutputItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	raw, _ := root["output"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		item, _ := v.(map[string]any)
		items = append(items, item)
	}
	return items
}

func TestResponsesNonStreamOutputItemIDs(t *testing.T) {
	t.Run("chat message", func(t *testing.T) {
		body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
		out, _, err := TranslateResponseBetween(ir.Responses, ir.Chat, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		items := responsesOutputItems(t, out)
		if len(items) != 1 || items[0]["type"] != "message" {
			t.Fatalf("output items = %v", items)
		}
		id, _ := items[0]["id"].(string)
		if !strings.HasPrefix(id, "msg_") || len(id) == len("msg_") {
			t.Fatalf("message id = %q, want a generated msg_ id", id)
		}
	})

	t.Run("anthropic reasoning", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"thinking","thinking":"think","signature":"sig"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		out, _, err := TranslateResponseBetween(ir.Responses, ir.Anthropic, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		items := responsesOutputItems(t, out)
		if len(items) != 1 || items[0]["type"] != "reasoning" {
			t.Fatalf("output items = %v", items)
		}
		id, _ := items[0]["id"].(string)
		if !strings.HasPrefix(id, "rs_") || len(id) == len("rs_") {
			t.Fatalf("reasoning id = %q, want a generated rs_ id", id)
		}
	})

	t.Run("existing reasoning id preserved", func(t *testing.T) {
		out, err := EncodeResponsesResponse(&ir.Response{
			ID: "resp_1", Model: "m",
			Blocks: []ir.Block{{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{ID: "rs_keep", Text: "think"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		items := responsesOutputItems(t, out)
		if len(items) != 1 || items[0]["id"] != "rs_keep" {
			t.Fatalf("output items = %v, want the source reasoning id", items)
		}
	})
}
