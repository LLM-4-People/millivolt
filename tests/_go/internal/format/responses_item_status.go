package format

// Round 12 B2 regression: a non-completed Responses envelope must mark its
// non-stream message item incomplete. OutputMessage.status is required and
// admits only in_progress, completed and incomplete. Streamed items follow a
// separate per-item close rule.

import (
	"bytes"
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

// TestResponsesStreamClosedItemStatus pins the per-item contract: an item that
// closed normally keeps status completed even when the terminal envelope is
// incomplete. The envelope owns the turn outcome; each item owns its close
// status.
func TestResponsesStreamClosedItemStatus(t *testing.T) {
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
			if doneStatus != "completed" {
				t.Fatalf("streamed output_item.done status = %q, want completed for a normal close: %s", doneStatus, out)
			}
			if len(terminal.Output) != 1 || terminal.Output[0].Type != "message" {
				t.Fatalf("terminal output = %+v, want one message item: %s", terminal.Output, out)
			}
			if terminal.Output[0].Status != "completed" {
				t.Fatalf("terminal item status = %q, want completed for an item that closed: %s", terminal.Output[0].Status, out)
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

// responsesDoneItemStatuses returns the status of each output_item.done item
// by type. A translated stream emits one done frame per item.
func responsesDoneItemStatuses(t *testing.T, out string) map[string]string {
	t.Helper()
	statuses := map[string]string{}
	for _, frame := range errorCloseFrames(out) {
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
		statuses[event.Item.Type] = event.Item.Status
	}
	return statuses
}

// TestResponsesStreamMultiItemIncompleteKeepsClosedStatuses is the NEW-1
// multi-item regression: a completed message followed by a tool call under a
// length terminal must keep both items completed in the terminal output. Only
// an item still open at the terminal may be rendered incomplete.
func TestResponsesStreamMultiItemIncompleteKeepsClosedStatuses(t *testing.T) {
	chunk := func(extra string) string {
		return `data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,"choices":[{"index":0,` + extra + `}]}` + "\n\n"
	}
	stream := chunk(`"delta":{"role":"assistant","content":"hi"}`) +
		chunk(`"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`) +
		chunk(`"delta":{},"finish_reason":"length"`) +
		"data: [DONE]\n\n"

	out, err := translateStreamSource(t, ir.Chat, ir.Responses, stream)
	if err != nil {
		t.Fatal(err)
	}
	doneStatus, terminal := responsesStreamItemStatus(t, out)
	if terminal.Status != "incomplete" {
		t.Fatalf("terminal status = %q, want incomplete: %s", terminal.Status, out)
	}
	if doneStatus != "completed" {
		t.Fatalf("streamed message done status = %q, want completed: %s", doneStatus, out)
	}
	done := responsesDoneItemStatuses(t, out)
	if done["message"] != "completed" || done["function_call"] != "completed" {
		t.Fatalf("done statuses = %v, want completed message and function_call: %s", done, out)
	}
	if len(terminal.Output) != 2 {
		t.Fatalf("terminal output = %+v, want a message and a function_call: %s", terminal.Output, out)
	}
	for _, item := range terminal.Output {
		if item.Status != "completed" {
			t.Fatalf("terminal %s status = %q, want completed for an item that closed: %s", item.Type, item.Status, out)
		}
	}
}

// TestResponsesStreamOpenMessageAtTerminal pins the other half of the contract:
// an item that never closed is rendered incomplete in the terminal output and
// has no done frame, whether the terminal is incomplete or failed.
func TestResponsesStreamOpenMessageAtTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal []*ir.Event
		want     string
	}{
		{
			name: "incomplete",
			terminal: []*ir.Event{
				{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: ir.FinishLength}},
				{Type: ir.EventDone},
			},
			want: "incomplete",
		},
		{
			name: "failed",
			terminal: []*ir.Event{
				{Type: ir.EventError, Error: &ir.Error{Type: "server_error", Message: "boom"}},
			},
			want: "failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewResponsesStreamEncoder(&buf, nil)
			events := append([]*ir.Event{
				{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
				{Type: ir.EventBlockDelta, Index: 0, Delta: &ir.Delta{Text: "hi"}},
			}, tc.terminal...)
			for _, event := range events {
				if err := enc.Write(event); err != nil {
					t.Fatalf("Write(%v): %v", event.Type, err)
				}
			}
			_, terminal := responsesStreamItemStatus(t, buf.String())
			if terminal.Status != tc.want {
				t.Fatalf("terminal status = %q, want %q: %s", terminal.Status, tc.want, buf.String())
			}
			if len(terminal.Output) != 1 || terminal.Output[0].Type != "message" ||
				terminal.Output[0].Status != "incomplete" {
				t.Fatalf("terminal output = %+v, want one incomplete open message: %s", terminal.Output, buf.String())
			}
			for _, frame := range errorCloseFrames(buf.String()) {
				if frame.name == "response.output_item.done" {
					t.Fatalf("an open item emitted a done frame: %s", buf.String())
				}
			}
		})
	}
}

// TestResponsesNonStreamFunctionCallStatus pins the non-stream half: a message
// follows the envelope status, while a function call with validated arguments
// stays completed because it remains executable.
func TestResponsesNonStreamFunctionCallStatus(t *testing.T) {
	body := func(finish string) string {
		return `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
			`"finish_reason":"` + finish + `"}]}`
	}
	for _, tc := range []struct {
		name     string
		finish   string
		envelope string
		message  string
	}{
		{name: "length", finish: "length", envelope: "incomplete", message: "incomplete"},
		{name: "completed control", finish: "stop", envelope: "completed", message: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := TranslateResponseBetween(ir.Responses, ir.Chat, []byte(body(tc.finish)))
			if err != nil {
				t.Fatal(err)
			}
			doc := responsesItemStatus(t, string(out))
			if doc.Status != tc.envelope {
				t.Fatalf("envelope status = %q, want %q (%s)", doc.Status, tc.envelope, out)
			}
			if len(doc.Output) != 2 {
				t.Fatalf("output = %+v, want a message and a function_call (%s)", doc.Output, out)
			}
			if doc.Output[0].Type != "message" || doc.Output[0].Status != tc.message {
				t.Fatalf("message item = %+v, want status %q (%s)", doc.Output[0], tc.message, out)
			}
			if doc.Output[1].Type != "function_call" || doc.Output[1].Status != "completed" {
				t.Fatalf("function_call item = %+v, want status completed (%s)", doc.Output[1], out)
			}
		})
	}
}
