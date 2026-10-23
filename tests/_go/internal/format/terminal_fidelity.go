package format

// Batch 4 regressions: a Responses terminal state is never fabricated, an
// in-band provider error is decoded through the shared decoy-aware parser, and
// a cancelled or incomplete stop is not rewritten as a clean completion.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesMissingStatusRejected(t *testing.T) {
	body := `{"id":"r1","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`
	if _, err := DecodeResponsesResponse([]byte(body)); err == nil || !strings.Contains(err.Error(), "terminal status") {
		t.Fatalf("missing status error = %v, want a terminal-status rejection", err)
	}
}

func TestResponsesInProgressRejected(t *testing.T) {
	for _, status := range []string{"in_progress", "queued", "searching"} {
		body := `{"id":"r1","status":"` + status + `","output":[]}`
		if _, err := DecodeResponsesResponse([]byte(body)); err == nil || !strings.Contains(err.Error(), "not terminal") {
			t.Fatalf("%s error = %v, want a non-terminal rejection", status, err)
		}
	}
}

func TestResponsesCancelledIsNotCleanStop(t *testing.T) {
	body := `{"id":"r1","status":"cancelled","output":[]}`
	resp, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != ir.FinishCancelled {
		t.Fatalf("finish = %q, want cancelled", resp.Finish)
	}
	out, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"cancelled"`) {
		t.Fatalf("Responses cancelled status rewritten: %s", out)
	}
	chat, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chat), `"finish_reason":"stop"`) {
		t.Fatalf("cancelled terminal rewritten as a clean Chat stop: %s", chat)
	}
}

func TestResponsesIncompleteMaxMessagesIsNotRewritten(t *testing.T) {
	body := `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_messages"},"output":[]}`
	resp, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != ir.FinishOther || resp.IncompleteReason != "max_messages" {
		t.Fatalf("finish = %q reason = %q, want other/max_messages", resp.Finish, resp.IncompleteReason)
	}
	out, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"incomplete"`) ||
		!strings.Contains(string(out), `"reason":"max_messages"`) {
		t.Fatalf("incomplete reason was rewritten: %s", out)
	}
	if strings.Contains(string(out), `"reason":"max_output_tokens"`) {
		t.Fatalf("incomplete max_messages was relabelled max_output_tokens: %s", out)
	}
}

func TestResponsesEmptyErrorObjectIsSuccess(t *testing.T) {
	body := `{"id":"r1","status":"completed","error":{},"output":[]}`
	resp, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatalf("empty error object rejected: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("empty error object treated as a failure: %+v", resp.Error)
	}
	if resp.Finish != ir.FinishStop {
		t.Fatalf("finish = %q, want stop", resp.Finish)
	}
}

func TestResponsesErrorEnvelopeShapesDecoded(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		typ  string
		msg  string
	}{
		{"object", `{"error":{"type":"server_error","message":"boom"}}`, "server_error", "boom"},
		{"string", `{"error":"plain failure"}`, "provider_error", "plain failure"},
		{"numeric code", `{"error":{"code":503,"message":"unavailable"}}`, "provider_error", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := DecodeResponsesError([]byte(tc.body))
			if err != nil {
				t.Fatalf("DecodeResponsesError = %v", err)
			}
			if e.Type != tc.typ || e.Message != tc.msg {
				t.Fatalf("error = %+v, want type %q message %q", e, tc.typ, tc.msg)
			}
		})
	}
	if _, err := DecodeResponsesError([]byte(`{"error":{}}`)); err == nil {
		t.Fatal("empty error object accepted as an error envelope")
	}
}

func TestMissingFinishMarkersAreNotCleanStops(t *testing.T) {
	chat := map[string]any{
		"id": "c1",
		"choices": []any{map[string]any{
			"index":   0,
			"message": map[string]any{"role": "assistant", "content": "hi"},
		}},
	}
	body, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DecodeChatResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != ir.FinishOther {
		t.Fatalf("Chat missing finish_reason = %q, want other", resp.Finish)
	}
	if !warned(resp.Warnings, "finish_reason") {
		t.Fatalf("Chat missing finish_reason recorded no warning: %+v", resp.Warnings)
	}
	encoded, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"finish_reason":"stop"`) {
		t.Fatalf("missing marker was rewritten as a clean stop: %s", encoded)
	}

	anthropic := `{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	aresp, err := DecodeAnthropicResponse([]byte(anthropic))
	if err != nil {
		t.Fatal(err)
	}
	if aresp.Finish != ir.FinishOther {
		t.Fatalf("Anthropic missing stop_reason = %q, want other", aresp.Finish)
	}
	if _, err := EncodeAnthropicResponse(aresp); err != nil {
		t.Fatal(err)
	}
	if !warned(aresp.Warnings, "finish_reason") {
		t.Fatalf("Anthropic terminal approximation recorded no warning: %+v", aresp.Warnings)
	}
}

func TestAnthropicStopDetailsWarns(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"refusal",` +
		`"stop_details":{"type":"refusal"},"usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := DecodeAnthropicResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !warned(resp.Warnings, "stop_details") {
		t.Fatalf("stop_details dropped with no warning: %+v", resp.Warnings)
	}
}

func warned(warnings ir.Warnings, field string) bool {
	for _, w := range warnings {
		if w.Field == field {
			return true
		}
	}
	return false
}

func TestAnthropicDocumentTitlePreserved(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[` +
		`{"type":"document","title":"spec.pdf","source":{"type":"base64",` +
		`"media_type":"application/pdf","data":"JVBERi0="}}]}]}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
		t.Fatalf("decoded content = %+v", req.Messages)
	}
	file := req.Messages[0].Content[0].File
	if file == nil || file.Filename != "spec.pdf" {
		t.Fatalf("document title = %+v, want spec.pdf", file)
	}
	out, err := EncodeAnthropicRequest(req, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"title":"spec.pdf"`) {
		t.Fatalf("document title was not re-emitted: %s", out)
	}
}
