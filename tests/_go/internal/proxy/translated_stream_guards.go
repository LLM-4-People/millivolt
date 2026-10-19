package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestTranslatedStreamTerminalGuardKeepsOneDone pins the post-terminal branch:
// once the translator has written [DONE], a later malformed or oversized source
// line must not add a synthetic error frame or a second [DONE]. The translation
// error is real (the stream failed after the terminal), but the client already
// has its complete answer.
func TestTranslatedStreamTerminalGuardKeepsOneDone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"malformed", "event: message_start\ndata: {not-json\n\n"},
		{"oversized", "data: " + strings.Repeat("x", 1<<20+1) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := strings.Join([]string{
				`data: {"type":"message_start","message":{"id":"msg-terminal","model":"claude-test","usage":{"input_tokens":4}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"complete"}}`,
				`data: {"type":"message_stop"}`,
			}, "\n\n") + "\n\n" + tc.payload
			upstream, srv, _ := translatedSSEFixture(t, native)

			status, body := translatedSSERequest(t, srv, upstream)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want the committed 200 (body %s)", status, body)
			}
			payloads := translatedSSEPayloads(t, body)
			if got := translatedSSEErrorFrames(payloads); got != 0 {
				t.Fatalf("post-terminal failure emitted %d error frames, want 0: %s", got, body)
			}
			if got := strings.Count(body, "data: [DONE]"); got != 1 {
				t.Fatalf("[DONE] frames = %d, want exactly 1: %s", got, body)
			}
			if payloads[len(payloads)-1] != "[DONE]" {
				t.Fatalf("last frame = %q, want [DONE]: %s", payloads[len(payloads)-1], body)
			}
			if !strings.Contains(body, `"content":"complete"`) {
				t.Fatalf("terminal fixture lost its answer content: %s", body)
			}
		})
	}
}

// TestTranslatedStreamForwardedProviderErrorIsNotDuplicated pins the
// forwarded-error guard: when the provider already sent its own in-band error
// and translation then fails, the client gets that error once plus the one
// terminal [DONE], never a synthetic translation_error stacked on top.
func TestTranslatedStreamForwardedProviderErrorIsNotDuplicated(t *testing.T) {
	native := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg-provider-error","model":"claude-test","usage":{"input_tokens":4}}}`,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"provider overloaded"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-1","name":"lookup"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-2","name":"lookup"}}`,
	}, "\n\n") + "\n\n"
	upstream, srv, _ := translatedSSEFixture(t, native)

	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}
	payloads := translatedSSEPayloads(t, body)
	if got := translatedSSEErrorFrames(payloads); got != 1 {
		t.Fatalf("error frames = %d, want exactly the provider's one: %s", got, body)
	}
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] frames = %d, want exactly 1: %s", got, body)
	}
	if payloads[len(payloads)-1] != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE]: %s", payloads[len(payloads)-1], body)
	}
	for _, payload := range payloads {
		if payload == "[DONE]" || !strings.Contains(payload, `"error"`) {
			continue
		}
		var envelope struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			t.Fatalf("error frame is not JSON: %v (%q)", err, payload)
		}
		if envelope.Error.Type == translationErrorCode {
			t.Fatalf("synthetic translation_error duplicated the provider's forwarded error: %s", body)
		}
		if envelope.Error.Type != "overloaded_error" || envelope.Error.Message != "provider overloaded" {
			t.Fatalf("forwarded error = %+v, want the provider's overloaded_error payload", envelope.Error)
		}
	}
}
