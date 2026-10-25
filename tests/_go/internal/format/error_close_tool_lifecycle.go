package format

// Round 12 C1/C-U regressions: an error close must not finalize an open tool
// call, and an Anthropic error event is the stream's terminal statement.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

type errorCloseFrame struct {
	name string
	data string
}

func errorCloseFrames(out string) []errorCloseFrame {
	var frames []errorCloseFrame
	for _, block := range strings.Split(out, "\n\n") {
		var frame errorCloseFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frame.data = strings.TrimPrefix(line, "data: ")
			}
		}
		if frame.data != "" {
			frames = append(frames, frame)
		}
	}
	return frames
}

func errorCloseSource(t *testing.T, source ir.Protocol) string {
	t.Helper()
	switch source {
	case ir.Chat:
		return `data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,` +
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,` +
			`"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}` + "\n\n" +
			`data: {"error":{"type":"server_error","message":"boom"}}` + "\n\n"
	case ir.Anthropic:
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"}}\n\n"
	case ir.Responses:
		return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"\"}}\n\n" +
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"call_1\",\"delta\":\"{\\\"a\\\":\"}\n\n" +
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"message\":\"boom\"},\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}]}}\n\n"
	default:
		t.Fatalf("unsupported source %q", source)
		return ""
	}
}

func assertOpenToolNotClosed(t *testing.T, target ir.Protocol, out string, err error, failure string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s became a translator failure: %v", failure, err)
	}
	frames := errorCloseFrames(out)
	if len(frames) == 0 {
		t.Fatalf("no client frames: %q", out)
	}
	switch target {
	case ir.Anthropic:
		for _, frame := range frames {
			if frame.name == "content_block_stop" && strings.Contains(frame.data, `"index":0`) {
				t.Fatalf("failure close emitted content_block_stop for the open tool: %s", out)
			}
		}
		if last := frames[len(frames)-1]; last.name != "error" {
			t.Fatalf("last Anthropic frame = %q, want the terminal error: %s", last.name, out)
		}
		if strings.Contains(out, "event: message_stop") {
			t.Fatalf("Anthropic error was followed by message_stop: %s", out)
		}
	case ir.Responses:
		for _, frame := range frames {
			if frame.name == "response.function_call_arguments.done" {
				t.Fatalf("failure close finalized the partial call: %s", out)
			}
		}
		var terminal map[string]any
		for _, frame := range frames {
			if frame.name != "response.failed" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(frame.data), &event); err != nil {
				t.Fatalf("parse response.failed: %v", err)
			}
			terminal, _ = event["response"].(map[string]any)
		}
		if terminal == nil {
			t.Fatalf("no response.failed terminal: %s", out)
		}
		output, _ := terminal["output"].([]any)
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call" && item["call_id"] == "call_1" {
				t.Fatalf("failed terminal lists the open call: %s", out)
			}
		}
	case ir.Chat:
		// The Chat target has no close frame. It may have already streamed the
		// announced call; the error chunk owns the turn and must still be
		// present.
		found := false
		for _, frame := range frames {
			if strings.Contains(frame.data, `"error"`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("Chat target lost the in-band error: %s", out)
		}
	}
}

func TestProviderErrorLeavesOpenToolCall(t *testing.T) {
	for _, source := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
			t.Run(string(source)+"->"+string(target), func(t *testing.T) {
				out, err := translateStreamSource(t, source, target, errorCloseSource(t, source))
				assertOpenToolNotClosed(t, target, out, err, "in-band error")
			})
		}
	}
}

// errorCloseTruncatedSource ends after a partial tool-call argument value with
// no terminal frame. The decoder reports ErrTruncatedStream, so FlushBlocks
// owns the close policy instead of an in-band provider error.
func errorCloseTruncatedSource(t *testing.T, source ir.Protocol) string {
	t.Helper()
	switch source {
	case ir.Chat:
		return `data: {"id":"c","object":"chat.completion.chunk","model":"m","created":1,` +
			`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,` +
			`"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}` + "\n\n"
	case ir.Anthropic:
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n"
	case ir.Responses:
		return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"\"}}\n\n" +
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"call_1\",\"delta\":\"{\\\"a\\\":\"}\n\n"
	default:
		t.Fatalf("unsupported source %q", source)
		return ""
	}
}

// TestTruncatedStreamLeavesOpenToolCall pins the FlushBlocks path for every
// source: a truncation is a normal in-band error, and it must not close a
// partial tool call that an in-band provider error leaves open.
func TestTruncatedStreamLeavesOpenToolCall(t *testing.T) {
	for _, source := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
			t.Run(string(source)+"->"+string(target), func(t *testing.T) {
				out, err := translateStreamSource(t, source, target, errorCloseTruncatedSource(t, source))
				assertOpenToolNotClosed(t, target, out, err, "truncation")
			})
		}
	}
}

// TestAnthropicErrorIsTerminal pins C-U independently of the tool lifecycle:
// the error event is the stream's terminal statement, so no message_stop
// follows it.
func TestAnthropicErrorIsTerminal(t *testing.T) {
	for _, source := range []ir.Protocol{ir.Chat, ir.Responses} {
		t.Run(string(source), func(t *testing.T) {
			var stream string
			switch source {
			case ir.Chat:
				stream = `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
					`data: {"error":{"type":"server_error","message":"boom"}}` + "\n\n"
			case ir.Responses:
				stream = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\"}}\n\n" +
					"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hi\"}\n\n" +
					"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"message\":\"boom\"},\"output\":[]}}\n\n"
			}
			out, err := translateStreamSource(t, source, ir.Anthropic, stream)
			if err != nil {
				t.Fatalf("in-band error became a translator failure: %v", err)
			}
			frames := errorCloseFrames(out)
			if len(frames) == 0 {
				t.Fatalf("no Anthropic frames: %q", out)
			}
			if last := frames[len(frames)-1]; last.name != "error" {
				t.Fatalf("last frame = %q, want error: %s", last.name, out)
			}
			if strings.Contains(out, "event: message_stop") {
				t.Fatalf("Anthropic error was followed by message_stop: %s", out)
			}
		})
	}
}
