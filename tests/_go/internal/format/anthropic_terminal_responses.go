package format

// Round 2 TC2-2 regressions: Anthropic terminal reasons that have no clean
// Responses equivalent map to an incomplete Responses status and record an
// approximation, without changing the Chat echo of the raw reason.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicNonStreamWithReason(reason string) []byte {
	return []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"` + reason + `",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`)
}

func responsesStatus(t *testing.T, body []byte) (status, reason string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	status, _ = root["status"].(string)
	if details, ok := root["incomplete_details"].(map[string]any); ok {
		reason, _ = details["reason"].(string)
	}
	return status, reason
}

func TestAnthropicTerminalReasonsToResponses(t *testing.T) {
	cases := []struct {
		reason      string
		wantStatus  string
		wantReason  string
		wantWarning string
	}{
		{reason: "end_turn", wantStatus: "completed"},
		{reason: "max_tokens", wantStatus: "incomplete", wantReason: "max_output_tokens"},
		{reason: "model_context_window_exceeded", wantStatus: "incomplete", wantReason: "max_output_tokens", wantWarning: "stop_reason"},
		{reason: "pause_turn", wantStatus: "incomplete", wantWarning: "finish_reason"},
		{reason: "refusal", wantStatus: "incomplete", wantReason: "content_filter", wantWarning: "finish_reason"},
		{reason: "provider_unknown", wantStatus: "incomplete", wantWarning: "finish_reason"},
	}
	for _, tc := range cases {
		t.Run("non-stream/"+tc.reason, func(t *testing.T) {
			body, warnings, err := TranslateResponseBetween(ir.Responses, ir.Anthropic, anthropicNonStreamWithReason(tc.reason))
			if err != nil {
				t.Fatal(err)
			}
			status, reason := responsesStatus(t, body)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("status/reason = %q/%q, want %q/%q", status, reason, tc.wantStatus, tc.wantReason)
			}
			found := tc.wantWarning == ""
			for _, warning := range warnings {
				if warning.Field == tc.wantWarning {
					found = true
				}
			}
			if !found {
				t.Fatalf("warnings = %+v, want field %q", warnings, tc.wantWarning)
			}
		})
	}

	for _, tc := range cases {
		t.Run("stream/"+tc.reason, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(anthropicStreamWithReason(tc.reason)), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, payload := range mustResponsePayloads(t, buf.String()) {
				typ, _ := payload["type"].(string)
				if typ != "response.completed" && typ != "response.incomplete" && typ != "response.cancelled" && typ != "response.failed" {
					continue
				}
				response, _ := payload["response"].(map[string]any)
				status, _ := response["status"].(string)
				reason := ""
				if details, ok := response["incomplete_details"].(map[string]any); ok {
					reason, _ = details["reason"].(string)
				}
				if status != tc.wantStatus || reason != tc.wantReason {
					t.Fatalf("%s status/reason = %q/%q, want %q/%q", typ, status, reason, tc.wantStatus, tc.wantReason)
				}
				found = true
			}
			if !found {
				t.Fatalf("no terminal Responses frame: %s", buf.String())
			}
		})
	}
}

func anthropicStreamWithReason(reason string) string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"` + reason + `"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
}

// TestAnthropicStreamContextWindowWarningOmitted pins NEW-6: the non-streaming
// decoder records the context-window approximation, while the stream applies
// the same terminal mapping without a warning. The stream status line is
// already committed and TranslateStreamBetween exposes no warning channel, so
// the message_delta owner documents the diagnostic-only gap. This test keeps
// the stream mapping pinned to the warned non-stream result.
func TestAnthropicStreamContextWindowWarningOmitted(t *testing.T) {
	_, warnings, err := TranslateResponseBetween(ir.Responses, ir.Anthropic,
		anthropicNonStreamWithReason("model_context_window_exceeded"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, warning := range warnings {
		if warning.Field == "stop_reason" {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-stream warnings = %+v, want the stop_reason approximation", warnings)
	}

	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(
		anthropicStreamWithReason("model_context_window_exceeded")), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	terminal := false
	for _, payload := range mustResponsePayloads(t, buf.String()) {
		typ, _ := payload["type"].(string)
		if typ != "response.completed" && typ != "response.incomplete" &&
			typ != "response.cancelled" && typ != "response.failed" {
			continue
		}
		response, _ := payload["response"].(map[string]any)
		status, _ := response["status"].(string)
		reason := ""
		if details, ok := response["incomplete_details"].(map[string]any); ok {
			reason, _ = details["reason"].(string)
		}
		if status != "incomplete" || reason != "max_output_tokens" {
			t.Fatalf("stream status/reason = %q/%q, want incomplete/max_output_tokens", status, reason)
		}
		terminal = true
	}
	if !terminal {
		t.Fatalf("no terminal Responses frame: %s", buf.String())
	}
	if strings.Contains(buf.String(), "model_context_window_exceeded") {
		t.Fatalf("provider-specific reason leaked into the stream: %s", buf.String())
	}
}
