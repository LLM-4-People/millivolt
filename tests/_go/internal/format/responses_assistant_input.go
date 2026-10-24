package format

// Round 7 A-U regression: the Responses request encoder must emit the
// EasyInputMessage shape for an assistant input turn. EasyInputMessage's
// content union accepts input_text for every role; output_text belongs to the
// response OutputMessage and made the item match none of the valid input
// variants.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesInputParts(t *testing.T, out []byte) map[string][]string {
	t.Helper()
	var doc struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			ID      string `json:"id"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	parts := map[string][]string{}
	for _, item := range doc.Input {
		key := item.Role
		for _, part := range item.Content {
			parts[key] = append(parts[key], part.Type+":"+part.Text)
			if item.Role == "assistant" && (item.ID != "" || item.Status != "") {
				t.Fatalf("assistant input item is not the minimal EasyInputMessage shape: id=%q status=%q", item.ID, item.Status)
			}
		}
	}
	return parts
}

func TestResponsesAssistantInputUsesEasyInputMessageShape(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"hello"},` +
		`{"role":"user","content":"again"}]}`
	out, _, err := TranslateRequestBetween(ir.Anthropic, ir.Responses, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	parts := responsesInputParts(t, out)
	if got := parts["assistant"]; len(got) != 1 || got[0] != "input_text:hello" {
		t.Fatalf("assistant input parts = %v, want input_text:hello (%s)", got, out)
	}
	if got := parts["user"]; len(got) != 2 || got[0] != "input_text:hi" || got[1] != "input_text:again" {
		t.Fatalf("user input parts = %v, want input_text (%s)", got, out)
	}
	var envelope struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Instructions != "sys" {
		t.Fatalf("system text = %q, want the lifted instructions field: %s", envelope.Instructions, out)
	}

	// The emitted request must decode back into the canonical request.
	decoded, err := DecodeResponsesRequest(out)
	if err != nil {
		t.Fatalf("translated request did not round-trip: %v (%s)", err, out)
	}
	found := false
	for _, message := range decoded.Messages {
		if message.Role == ir.RoleAssistant {
			for _, block := range message.Content {
				if block.Type == ir.BlockText && block.Text == "hello" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("assistant text did not round-trip: %+v", decoded.Messages)
	}

	// The response encoder still owns output_text.
	respBody, err := EncodeResponsesResponse(&ir.Response{
		ID: "resp_1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{ir.TextBlock("answer")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsJSONTextType(t, respBody, "output_text", "answer") {
		t.Fatalf("response output lost output_text: %s", respBody)
	}
}

func containsJSONTextType(t *testing.T, body []byte, partType, text string) bool {
	t.Helper()
	var doc struct {
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, item := range doc.Output {
		for _, part := range item.Content {
			if part.Type == partType && part.Text == text {
				return true
			}
		}
	}
	return false
}
