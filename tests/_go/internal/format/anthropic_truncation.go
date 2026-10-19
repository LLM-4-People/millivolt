package format

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestStreamToOpenAITruncatedSignalsError: an upstream Anthropic stream that
// dies at EOF without stop_reason AND without message_stop is a truncation -
// the translator must emit an in-band error chunk, never a clean [DONE], so
// clients see a retryable failure instead of a silently truncated answer.
func TestStreamToOpenAITruncatedSignalsError(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half the"}}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"half the"`) {
		t.Fatalf("partial content must still relay: %q", got)
	}
	if !strings.Contains(got, `"code":"truncated"`) {
		t.Fatalf("in-band truncated error missing: %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
	}
}

// TestStreamToOpenAIMessageStopOnlyNotTruncated: message_stop without a
// message_delta is the SERVER closing the turn (an empty-turn shape); it must
// not be flagged as a truncation.
func TestStreamToOpenAIMessageStopOnlyNotTruncated(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, `"truncated"`) {
		t.Fatalf("message_stop-only turn must not be flagged: %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
	}
}

// TestStreamToOpenAIStopReasonWithoutMessageStopNotTruncated: message_delta
// with stop_reason but no message_stop means everything the model produced did
// arrive - clean [DONE], no error.
func TestStreamToOpenAIStopReasonWithoutMessageStopNotTruncated(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, `"truncated"`) {
		t.Fatalf("stop_reason-complete stream must not be flagged: %q", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason missing: %q", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
	}
}

// TestStreamToOpenAIErrorEventNotDoubleSignaled: a provider-sent event: error
// (which terminates the stream without message_stop) must surface alone - the
// truncation signal never stacks on top of the provider's own failure.
func TestStreamToOpenAIErrorEventNotDoubleSignaled(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"",
	}, "\n") + "\n"

	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "overloaded_error") {
		t.Fatalf("provider error not forwarded: %q", got)
	}
	if strings.Contains(got, `"truncated"`) {
		t.Fatalf("truncation must not stack on a provider error: %q", got)
	}
	if n := strings.Count(got, `"error"`); n != 1 {
		t.Fatalf("exactly one error chunk expected, got %d: %q", n, got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
	}
}

// TestStreamToOpenAIProviderErrorFrame covers both provider error shapes: an
// error event with an empty error object must not emit an empty error chunk
// (which also left the truncation guard armed, producing a second error), so
// the truncation path supplies the one canonical failure frame; a non-empty
// provider error is forwarded exactly once. Both end with one [DONE].
func TestStreamToOpenAIProviderErrorFrame(t *testing.T) {
	for _, tc := range []struct {
		name        string
		payload     string
		wantType    string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "empty error object falls back to truncation",
			payload:     `{"type":"error","error":{}}`,
			wantType:    "upstream_error",
			wantCode:    metrics.CodeTruncated,
			wantMessage: metrics.DegenerateMessage(metrics.CodeTruncated),
		},
		{
			name:        "non-empty provider error control",
			payload:     `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantType:    "overloaded_error",
			wantMessage: "Overloaded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join([]string{
				"event: message_start",
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
				"",
				"event: error",
				"data: " + tc.payload,
				"",
			}, "\n") + "\n"

			var out strings.Builder
			if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
				t.Fatal(err)
			}
			body := out.String()
			if n := strings.Count(body, "data: [DONE]"); n != 1 {
				t.Fatalf("[DONE] count = %d, want 1: %q", n, body)
			}

			type errorFrame struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			}
			var errorFrames []errorFrame
			for _, line := range strings.Split(body, "\n") {
				if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
					continue
				}
				var frame struct {
					Error *errorFrame `json:"error"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
					t.Fatalf("bad chunk %q: %v", line, err)
				}
				if frame.Error != nil {
					errorFrames = append(errorFrames, *frame.Error)
				}
			}
			if len(errorFrames) != 1 {
				t.Fatalf("error frames = %d, want exactly 1: %q", len(errorFrames), body)
			}
			got := errorFrames[0]
			if got.Type != tc.wantType || got.Code != tc.wantCode || got.Message != tc.wantMessage {
				t.Fatalf("error frame = %+v, want type %q code %q message %q: %q",
					got, tc.wantType, tc.wantCode, tc.wantMessage, body)
			}
		})
	}
}
