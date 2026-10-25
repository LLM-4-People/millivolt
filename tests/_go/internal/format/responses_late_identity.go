package format

// Round 5 C1 and C-U-b regressions: a late Responses start event cannot rewrite
// identity already committed by the lazily emitted response.created pair, and a
// source stream without its own start names the request model via
// StreamOptions.FallbackModel.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesLateStartKeepsCommittedIdentity(t *testing.T) {
	var buf bytes.Buffer
	enc := NewResponsesStreamEncoder(&buf, nil)
	enc.fallbackModel = "request-model"
	events := []*ir.Event{
		{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText, Text: "hi"}},
		{Type: ir.EventStart, Response: &ir.Response{
			ID: "resp_late", Model: "m2", Created: 99,
			Usage: ir.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		}},
		{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: ir.FinishStop}},
		{Type: ir.EventDone},
	}
	for _, ev := range events {
		if err := enc.Write(ev); err != nil {
			t.Fatalf("Write(%v): %v", ev.Type, err)
		}
	}
	payloads := mustResponsePayloads(t, buf.String())
	var firstID string
	var firstCreated float64
	created, inProgress, completed := 0, 0, 0
	for _, payload := range payloads {
		typ, _ := payload["type"].(string)
		response, _ := payload["response"].(map[string]any)
		switch typ {
		case "response.created":
			created++
			firstID, _ = response["id"].(string)
			firstCreated, _ = response["created_at"].(float64)
			if firstID == "" || firstID == "resp_late" {
				t.Fatalf("created id = %q, want a synthesized identity", firstID)
			}
			if response["model"] != "request-model" {
				t.Fatalf("created model = %v, want request-model", response["model"])
			}
		case "response.in_progress":
			inProgress++
			if got, _ := response["created_at"].(float64); got != firstCreated || got == 99 {
				t.Fatalf("in_progress created_at = %v, want the committed %v (not the late 99)", got, firstCreated)
			}
		case "response.completed":
			completed++
			if response["id"] != firstID || response["model"] != "request-model" {
				t.Fatalf("terminal identity = %v/%v, want %s/request-model: %s",
					response["id"], response["model"], firstID, buf.String())
			}
			if got, _ := response["created_at"].(float64); got != firstCreated || got == 99 {
				t.Fatalf("completed created_at = %v, want the committed %v (not the late 99)", got, firstCreated)
			}
			usage, _ := response["usage"].(map[string]any)
			if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
				t.Fatalf("late-start usage was not merged: %v", usage)
			}
		}
	}
	if firstCreated == 0 || firstCreated == 99 {
		t.Fatalf("created_at = %v, want a synthesized non-99 value", firstCreated)
	}
	if created != 1 || inProgress != 1 || completed != 1 {
		t.Fatalf("start/terminal counts = %d/%d/%d, want 1/1/1: %s", created, inProgress, completed, buf.String())
	}
	if strings.Contains(buf.String(), "resp_late") || strings.Contains(buf.String(), `"model":"m2"`) {
		t.Fatalf("late start rewrote committed identity: %s", buf.String())
	}
}

func TestResponsesLazyStartUsesRequestModel(t *testing.T) {
	const source = `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Chat, &buf, strings.NewReader(source), nil, nil,
		StreamOptions{FallbackModel: "request-model"}); err != nil {
		t.Fatal(err)
	}
	var created, completed bool
	for _, payload := range mustResponsePayloads(t, buf.String()) {
		response, _ := payload["response"].(map[string]any)
		switch payload["type"] {
		case "response.created":
			created = true
			if response["model"] != "request-model" {
				t.Fatalf("created model = %v, want request-model: %s", response["model"], buf.String())
			}
		case "response.completed":
			completed = true
			if response["model"] != "request-model" {
				t.Fatalf("terminal model = %v, want request-model: %s", response["model"], buf.String())
			}
		}
	}
	if !created || !completed {
		t.Fatalf("missing lazy start or terminal (created=%v completed=%v): %s", created, completed, buf.String())
	}
}
