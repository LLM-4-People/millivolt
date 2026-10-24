package format

// Round 2 TC3-6 regressions: a Chat client receives the extra usage chunk
// only when it requested stream_options.include_usage and the source actually
// reported usage. A source without usage must not produce an all-zero chunk.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesTerminalUsage(usage string) string {
	if usage != "" {
		usage = `,"usage":` + usage
	}
	return `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]` + usage + `}}

`
}

func anthropicUsageStream(startUsage, deltaUsage string) string {
	start := `{"type":"message_start","message":{"id":"msg_1","model":"m"`
	if startUsage != "" {
		start += `,"usage":` + startUsage
	}
	start += `}}`
	delta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"}`
	if deltaUsage != "" {
		delta += `,"usage":` + deltaUsage
	}
	delta += `}`
	return "event: message_start\ndata: " + start + "\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: " + delta + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// chatUsageFrames counts frames that carry a usage object.
func chatUsageFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var outFrames []map[string]any
	for _, frame := range chatDataFrames(t, bytes.NewBufferString(out)) {
		if usage, ok := frame["usage"]; ok {
			if usage == nil {
				continue
			}
			outFrames = append(outFrames, frame)
		}
	}
	return outFrames
}

func TestResponsesStreamUsageChunkGatedByRequest(t *testing.T) {
	withUsage := responsesTerminalUsage(`{"input_tokens":10,"output_tokens":5,"total_tokens":15}`)
	withoutUsage := responsesTerminalUsage("")

	for _, tc := range []struct {
		name    string
		source  string
		include bool
		want    int
	}{
		{name: "requested and reported", source: withUsage, include: true, want: 1},
		{name: "not requested", source: withUsage, include: false, want: 0},
		{name: "requested but absent", source: withoutUsage, include: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(tc.source), nil, nil, StreamOptions{IncludeUsage: tc.include}); err != nil {
				t.Fatal(err)
			}
			usageFrames := chatUsageFrames(t, buf.String())
			if len(usageFrames) != tc.want {
				t.Fatalf("usage chunks = %d, want %d: %s", len(usageFrames), tc.want, buf.String())
			}
			if tc.want == 1 {
				usage, _ := usageFrames[0]["usage"].(map[string]any)
				if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
					t.Fatalf("usage = %v, want 10/5: %s", usage, buf.String())
				}
			}
		})
	}
}

func TestAnthropicStreamUsageChunkGatedByRequest(t *testing.T) {
	withUsage := anthropicUsageStream(`{"input_tokens":10,"output_tokens":0}`, `{"output_tokens":5}`)
	withoutUsage := anthropicUsageStream("", "")

	for _, tc := range []struct {
		name    string
		source  string
		include bool
		want    int
	}{
		{name: "requested and reported", source: withUsage, include: true, want: 1},
		{name: "not requested", source: withUsage, include: false, want: 0},
		{name: "requested but absent", source: withoutUsage, include: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(tc.source), nil, nil, StreamOptions{IncludeUsage: tc.include}); err != nil {
				t.Fatal(err)
			}
			usageFrames := chatUsageFrames(t, buf.String())
			if len(usageFrames) != tc.want {
				t.Fatalf("usage chunks = %d, want %d: %s", len(usageFrames), tc.want, buf.String())
			}
			if tc.want == 1 {
				usage, _ := usageFrames[0]["usage"].(map[string]any)
				if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
					t.Fatalf("usage = %v, want 10/5: %s", usage, buf.String())
				}
			}
		})
	}
}

func TestZeroReportedUsageStillEmitsRequestedChunk(t *testing.T) {
	source := responsesTerminalUsage(`{"input_tokens":0,"output_tokens":0,"total_tokens":0}`)
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{IncludeUsage: true}); err != nil {
		t.Fatal(err)
	}
	usageFrames := chatUsageFrames(t, buf.String())
	if len(usageFrames) != 1 {
		t.Fatalf("usage chunks = %d, want one for a reported zero usage: %s", len(usageFrames), buf.String())
	}
	usage, _ := usageFrames[0]["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(0) || usage["completion_tokens"] != float64(0) {
		t.Fatalf("usage = %v, want explicit zeros: %s", usage, buf.String())
	}
}
