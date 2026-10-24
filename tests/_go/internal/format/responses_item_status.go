package format

// Round 12 B2 regression: a non-completed Responses envelope must not describe
// its message item as completed. OutputMessage.status is required and admits
// only in_progress, completed and incomplete.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

type responsesItemStatusDoc struct {
	Status string `json:"status"`
	Output []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"output"`
}

func responsesItemStatus(t *testing.T, body string) responsesItemStatusDoc {
	t.Helper()
	var doc responsesItemStatusDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestResponsesIncompleteItemStatus(t *testing.T) {
	cases := []struct {
		name     string
		source   ir.Protocol
		body     string
		envelope string
	}{
		{
			name:     "chat length",
			source:   ir.Chat,
			body:     `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}]}`,
			envelope: "incomplete",
		},
		{
			name:     "chat content filter",
			source:   ir.Chat,
			body:     `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"content_filter"}]}`,
			envelope: "incomplete",
		},
		{
			name:     "anthropic max tokens",
			source:   ir.Anthropic,
			body:     `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`,
			envelope: "incomplete",
		},
		{
			name:     "responses incomplete",
			source:   ir.Responses,
			body:     `{"id":"r","object":"response","created_at":1,"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`,
			envelope: "incomplete",
		},
		{
			name:     "completed control",
			source:   ir.Chat,
			body:     `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			envelope: "completed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := TranslateResponseBetween(ir.Responses, tc.source, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			doc := responsesItemStatus(t, string(out))
			if doc.Status != tc.envelope {
				t.Fatalf("envelope status = %q, want %q (%s)", doc.Status, tc.envelope, out)
			}
			if len(doc.Output) != 1 || doc.Output[0].Type != "message" {
				t.Fatalf("output = %+v, want one message item", doc.Output)
			}
			want := "completed"
			if tc.envelope != "completed" {
				want = "incomplete"
			}
			if doc.Output[0].Status != want {
				t.Fatalf("item status = %q, want %q (%s)", doc.Output[0].Status, want, out)
			}
		})
	}
}
