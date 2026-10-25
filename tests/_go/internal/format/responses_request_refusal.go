package format

// Round 7 verifier NEW-3 regression: a request-side refusal part must not use
// the OutputMessage refusal type. The Responses input content union is
// input_text, input_image and input_file, so the refusal text becomes input_text
// with one loss warning. The response encoder still owns the valid refusal part.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesRequestRefusalUsesInputText(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockRefusal, Refusal: "I cannot help with that"},
				{Type: ir.BlockRefusal, Refusal: "still no"},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("next")}},
		},
	}
	out, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"type":"refusal"`) {
		t.Fatalf("request input contains the invalid refusal part: %s", out)
	}
	input, _ := decodeJSONMap(t, out)["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %v, want user, assistant and user", input)
	}
	assistant, _ := input[1].(map[string]any)
	parts, _ := assistant["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("assistant parts = %v, want the two refusal texts", parts)
	}
	for i, want := range []string{"I cannot help with that", "still no"} {
		part, _ := parts[i].(map[string]any)
		if part["type"] != "input_text" || part["text"] != want {
			t.Fatalf("assistant part %d = %v, want input_text %q", i, part, want)
		}
	}
	if got := warningCount(req.Warnings, ir.WarningDropped, "content.refusal"); got != 1 {
		t.Fatalf("content.refusal warnings = %d, want 1 (%+v)", got, req.Warnings)
	}
	if !strings.Contains(req.Warnings.Header(), "refusal text is sent as input text") {
		t.Fatalf("warning does not explain the approximation: %+v", req.Warnings)
	}

	// The response encoder still emits the valid OutputMessage refusal part.
	respBody, err := EncodeResponsesResponse(&ir.Response{
		ID: "resp_1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{{Type: ir.BlockRefusal, Refusal: "no"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(respBody), `"type":"refusal"`) {
		t.Fatalf("response lost its refusal part: %s", respBody)
	}
}
