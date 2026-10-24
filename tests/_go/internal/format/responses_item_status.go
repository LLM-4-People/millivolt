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

// responsesStreamItemStatus returns the status on the streamed message
// output_item.done frame and the status on the message item inside the terminal
// response.* envelope.
func responsesStreamItemStatus(t *testing.T, out string) (doneStatus string, terminal responsesItemStatusDoc) {
	t.Helper()
	frames := errorCloseFrames(out)
	if len(frames) == 0 {
		t.Fatalf("no client frames: %q", out)
	}
	for _, frame := range frames {
		if frame.name != "response.output_item.done" {
			continue
		}
		var event struct {
			Item struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(frame.data), &event); err != nil {
			t.Fatalf("parse output_item.done: %v", err)
		}
		if event.Item.Type == "message" {
			doneStatus = event.Item.Status
		}
	}
	last := frames[len(frames)-1]
	if last.name != "response.completed" && last.name != "response.incomplete" && last.name != "response.failed" {
		t.Fatalf("last frame = %q, want a terminal response event: %s", last.name, out)
	}
	var event struct {
		Response responsesItemStatusDoc `json:"response"`
	}
	if err := json.Unmarshal([]byte(last.data), &event); err != nil {
		t.Fatalf("parse terminal response: %v", err)
	}
	return doneStatus, event.Response
}

func TestResponsesStreamIncompleteItemStatus(t *testing.T) {
	chat := func(finish string) string {
		chunk := func(extra string) string {
			return `data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,"choices":[{"index":0,` + extra + `}]}` + "\n\n"
		}
		return chunk(`"delta":{"role":"assistant","content":"hi"}`) +
			chunk(`"delta":{},"finish_reason":"`+finish+`"`) +
			"data: [DONE]\n\n"
	}
	anthropic := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	for _, tc := range []struct {
		name   string
		source ir.Protocol
		stream string
	}{
		{name: "chat length", source: ir.Chat, stream: chat("length")},
		{name: "anthropic max tokens", source: ir.Anthropic, stream: anthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := translateStreamSource(t, tc.source, ir.Responses, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			doneStatus, terminal := responsesStreamItemStatus(t, out)
			if terminal.Status != "incomplete" {
				t.Fatalf("terminal status = %q, want incomplete: %s", terminal.Status, out)
			}
			if doneStatus != "incomplete" {
				t.Fatalf("streamed output_item.done status = %q, want incomplete: %s", doneStatus, out)
			}
			if len(terminal.Output) != 1 || terminal.Output[0].Type != "message" {
				t.Fatalf("terminal output = %+v, want one message item: %s", terminal.Output, out)
			}
			if terminal.Output[0].Status != "incomplete" {
				t.Fatalf("terminal item status = %q, want incomplete: %s", terminal.Output[0].Status, out)
			}
		})
	}

	t.Run("completed control", func(t *testing.T) {
		out, err := translateStreamSource(t, ir.Chat, ir.Responses, chat("stop"))
		if err != nil {
			t.Fatal(err)
		}
		doneStatus, terminal := responsesStreamItemStatus(t, out)
		if terminal.Status != "completed" {
			t.Fatalf("terminal status = %q, want completed: %s", terminal.Status, out)
		}
		if doneStatus != "completed" {
			t.Fatalf("streamed output_item.done status = %q, want completed: %s", doneStatus, out)
		}
		if len(terminal.Output) != 1 || terminal.Output[0].Status != "completed" {
			t.Fatalf("terminal output = %+v, want one completed message: %s", terminal.Output, out)
		}
	})
}
