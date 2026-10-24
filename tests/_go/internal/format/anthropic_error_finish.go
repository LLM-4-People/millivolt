package format

// Round 5 B2 regressions: an Anthropic stop_reason of "error" is a provider
// failure even without an error object. The non-stream response synthesizes the
// canonical upstream_error, and the stream decoder emits one error event before
// the terminal instead of a clean done.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestAnthropicStopReasonErrorSynthesizesFailure(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"partial"}],"stop_reason":"error","usage":{"input_tokens":1,"output_tokens":1}}`
	resp, err := DecodeResponse(ir.Anthropic, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != ir.FinishError {
		t.Fatalf("finish = %q, want error", resp.Finish)
	}
	if resp.Error == nil || resp.Error.Type != "upstream_error" || resp.Error.Message == "" {
		t.Fatalf("error = %+v, want a synthesized upstream_error", resp.Error)
	}
	encoded, err := EncodeResponse(ir.Responses, resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Status string `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Status != "failed" || wire.Error.Type != "upstream_error" || wire.Error.Message == "" {
		t.Fatalf("responses body = status %q error %+v, want failed/upstream_error", wire.Status, wire.Error)
	}

	const stream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"error"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	var buf strings.Builder
	if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(stream), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	errorFrames := 0
	failedWithError := false
	for _, payload := range mustResponsePayloads(t, buf.String()) {
		switch payload["type"] {
		case "error":
			errorFrames++
		case "response.failed":
			response, _ := payload["response"].(map[string]any)
			failure, _ := response["error"].(map[string]any)
			if failure != nil && failure["type"] == "upstream_error" && failure["message"] != "" {
				failedWithError = true
			}
		}
	}
	if errorFrames != 1 || !failedWithError {
		t.Fatalf("error frames = %d failed-with-error=%v, want one each: %s", errorFrames, failedWithError, buf.String())
	}
}

func TestAnthropicExplicitErrorKeepsProviderType(t *testing.T) {
	const stream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}

`
	var buf strings.Builder
	if err := TranslateStreamBetween(ir.Responses, ir.Anthropic, &buf, strings.NewReader(stream), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	seen := 0
	failed := false
	for _, payload := range mustResponsePayloads(t, buf.String()) {
		switch payload["type"] {
		case "error":
			seen++
			if payload["message"] != "busy" {
				t.Fatalf("explicit error message = %v, want busy", payload["message"])
			}
		case "response.failed":
			response, _ := payload["response"].(map[string]any)
			failure, _ := response["error"].(map[string]any)
			if failure["type"] != "overloaded_error" || failure["message"] != "busy" {
				t.Fatalf("provider failure = %v, want overloaded_error/busy", failure)
			}
			failed = true
		}
	}
	if seen != 1 || !failed {
		t.Fatalf("error frames = %d failed=%v, want one each: %s", seen, failed, buf.String())
	}
}
